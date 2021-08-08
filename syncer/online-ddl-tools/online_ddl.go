// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package onlineddl

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/pingcap/failpoint"

	"github.com/pingcap/dm/dm/config"
	"github.com/pingcap/dm/pkg/conn"
	tcontext "github.com/pingcap/dm/pkg/context"
	"github.com/pingcap/dm/pkg/cputil"
	"github.com/pingcap/dm/pkg/terror"
	"github.com/pingcap/dm/syncer/dbconn"

	"github.com/pingcap/parser/ast"
	"github.com/pingcap/tidb-tools/pkg/dbutil"
	"github.com/pingcap/tidb-tools/pkg/filter"
	"go.uber.org/zap"
)

// OnlineDDLSchemes is scheme name => online ddl handler.
var OnlineDDLSchemes = map[string]func(*tcontext.Context, *config.SubTaskConfig) (OnlinePlugin, error){
	config.PT:    NewPT,
	config.GHOST: NewGhost,
}

// refactor to reduce duplicate later.
var (
	maxCheckPointTimeout = "1m"
)

// OnlinePlugin handles online ddl solutions like pt, gh-ost.
type OnlinePlugin interface {
	// Apply does:
	// * detect online ddl
	// * record changes
	// * apply online ddl on real table
	// returns sqls, replaced/self schema, replaced/self table, error
	Apply(tctx *tcontext.Context, tables []*filter.Table, statement string, stmt ast.StmtNode) ([]string, string, string, error)
	// Finish would delete online ddl from memory and storage
	Finish(tctx *tcontext.Context, schema, table string) error
	// TableType returns ghhost/real table
	TableType(table string) TableType
	// RealName returns real table name that removed ghost suffix and handled by table router
	RealName(table string) string
	// ResetConn reset db connection
	ResetConn(tctx *tcontext.Context) error
	// Clear clears all online information
	// TODO: not used now, check if we could remove it later
	Clear(tctx *tcontext.Context) error
	// Close closes online ddl plugin
	Close()
	// CheckAndUpdate try to check and fix the schema/table case-sensitive issue
	CheckAndUpdate(tctx *tcontext.Context, schemas map[string]string, tables map[string]map[string]string) error
}

// TableType is type of table.
type TableType string

// below variables will be explained later.
const (
	RealTable  TableType = "real table"
	GhostTable TableType = "ghost table"
	TrashTable TableType = "trash table" // means we should ignore these tables
)

// GhostDDLInfo stores ghost information and ddls.
type GhostDDLInfo struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`

	DDLs []string `json:"ddls"`
}

// Storage stores sharding group online ddls information.
type Storage struct {
	sync.RWMutex

	cfg *config.SubTaskConfig

	db        *conn.BaseDB
	dbConn    *dbconn.DBConn
	schema    string // schema name, set through task config
	tableName string // table name with schema, now it's task name
	id        string // the source ID of the upstream MySQL/MariaDB replica.

	// map ghost schema => [ghost table => ghost ddl info, ...]
	ddls map[string]map[string]*GhostDDLInfo

	logCtx *tcontext.Context
}

// NewOnlineDDLStorage creates a new online ddl storager.
func NewOnlineDDLStorage(logCtx *tcontext.Context, cfg *config.SubTaskConfig) *Storage {
	s := &Storage{
		cfg:       cfg,
		schema:    dbutil.ColumnName(cfg.MetaSchema),
		tableName: dbutil.TableName(cfg.MetaSchema, cputil.SyncerOnlineDDL(cfg.Name)),
		id:        cfg.SourceID,
		ddls:      make(map[string]map[string]*GhostDDLInfo),
		logCtx:    logCtx,
	}

	return s
}

// Init initials online handler.
func (s *Storage) Init(tctx *tcontext.Context) error {
	onlineDB := s.cfg.To
	onlineDB.RawDBCfg = config.DefaultRawDBConfig().SetReadTimeout(maxCheckPointTimeout)
	db, dbConns, err := dbconn.CreateConns(tctx, s.cfg, onlineDB, 1)
	if err != nil {
		return terror.WithScope(err, terror.ScopeDownstream)
	}
	s.db = db
	s.dbConn = dbConns[0]

	err = s.prepare(tctx)
	if err != nil {
		return err
	}

	return s.Load(tctx)
}

// Load loads information from storage.
func (s *Storage) Load(tctx *tcontext.Context) error {
	s.Lock()
	defer s.Unlock()

	query := fmt.Sprintf("SELECT `ghost_schema`, `ghost_table`, `ddls` FROM %s WHERE `id`= ?", s.tableName)
	rows, err := s.dbConn.QuerySQL(tctx, query, s.id)
	if err != nil {
		return terror.WithScope(err, terror.ScopeDownstream)
	}
	defer rows.Close()

	var (
		schema string
		table  string
		ddls   string
	)
	for rows.Next() {
		err := rows.Scan(&schema, &table, &ddls)
		if err != nil {
			return terror.WithScope(terror.DBErrorAdapt(err, terror.ErrDBDriverError), terror.ScopeDownstream)
		}

		mSchema, ok := s.ddls[schema]
		if !ok {
			mSchema = make(map[string]*GhostDDLInfo)
			s.ddls[schema] = mSchema
		}

		mSchema[table] = &GhostDDLInfo{}
		err = json.Unmarshal([]byte(ddls), mSchema[table])
		if err != nil {
			return terror.ErrSyncerUnitOnlineDDLInvalidMeta.Delegate(err)
		}
		tctx.L().Info("loaded online ddl meta from checkpoint",
			zap.String("db", schema),
			zap.String("table", table))
	}

	return terror.WithScope(terror.DBErrorAdapt(rows.Err(), terror.ErrDBDriverError), terror.ScopeDownstream)
}

// Get returns ddls by given schema/table.
func (s *Storage) Get(ghostSchema, ghostTable string) *GhostDDLInfo {
	s.RLock()
	defer s.RUnlock()

	mSchema, ok := s.ddls[ghostSchema]
	if !ok {
		return nil
	}

	if mSchema == nil || mSchema[ghostTable] == nil {
		return nil
	}

	clone := new(GhostDDLInfo)
	*clone = *mSchema[ghostTable]

	return clone
}

// Save saves online ddl information.
func (s *Storage) Save(tctx *tcontext.Context, ghostSchema, ghostTable, realSchema, realTable, ddl string) error {
	s.Lock()
	defer s.Unlock()

	mSchema, ok := s.ddls[ghostSchema]
	if !ok {
		mSchema = make(map[string]*GhostDDLInfo)
		s.ddls[ghostSchema] = mSchema
	}

	info, ok := mSchema[ghostTable]
	if !ok {
		info = &GhostDDLInfo{
			Schema: realSchema,
			Table:  realTable,
		}
		mSchema[ghostTable] = info
	}

	// maybe we meed more checks for it

	if len(info.DDLs) != 0 && info.DDLs[len(info.DDLs)-1] == ddl {
		tctx.L().Warn("online ddl may be saved before, just ignore it", zap.String("ddl", ddl))
		return nil
	}
	info.DDLs = append(info.DDLs, ddl)
	err := s.saveToDB(tctx, ghostSchema, ghostTable, info)
	return terror.WithScope(err, terror.ScopeDownstream)
}

func (s *Storage) saveToDB(tctx *tcontext.Context, ghostSchema, ghostTable string, ddl *GhostDDLInfo) error {
	ddlsBytes, err := json.Marshal(ddl)
	if err != nil {
		return terror.ErrSyncerUnitOnlineDDLInvalidMeta.Delegate(err)
	}

	query := fmt.Sprintf("REPLACE INTO %s(`id`,`ghost_schema`, `ghost_table`, `ddls`) VALUES (?, ?, ?, ?)", s.tableName)
	_, err = s.dbConn.ExecuteSQL(tctx, []string{query}, []interface{}{s.id, ghostSchema, ghostTable, string(ddlsBytes)})
	failpoint.Inject("ExitAfterSaveOnlineDDL", func() {
		tctx.L().Info("failpoint ExitAfterSaveOnlineDDL")
		panic("ExitAfterSaveOnlineDDL")
	})
	return terror.WithScope(err, terror.ScopeDownstream)
}

// Delete deletes online ddl informations.
func (s *Storage) Delete(tctx *tcontext.Context, ghostSchema, ghostTable string) error {
	s.Lock()
	defer s.Unlock()
	return s.delete(tctx, ghostSchema, ghostTable)
}

func (s *Storage) delete(tctx *tcontext.Context, ghostSchema, ghostTable string) error {
	mSchema, ok := s.ddls[ghostSchema]
	if !ok {
		return nil
	}

	// delete all checkpoints
	sql := fmt.Sprintf("DELETE FROM %s WHERE `id` = ? and `ghost_schema` = ? and `ghost_table` = ?", s.tableName)
	_, err := s.dbConn.ExecuteSQL(tctx, []string{sql}, []interface{}{s.id, ghostSchema, ghostTable})
	if err != nil {
		return terror.WithScope(err, terror.ScopeDownstream)
	}

	delete(mSchema, ghostTable)
	return nil
}

// Clear clears online ddl information from storage.
func (s *Storage) Clear(tctx *tcontext.Context) error {
	s.Lock()
	defer s.Unlock()

	// delete all checkpoints
	sql := fmt.Sprintf("DELETE FROM %s WHERE `id` = ?", s.tableName)
	_, err := s.dbConn.ExecuteSQL(tctx, []string{sql}, []interface{}{s.id})
	if err != nil {
		return terror.WithScope(err, terror.ScopeDownstream)
	}

	s.ddls = make(map[string]map[string]*GhostDDLInfo)
	return nil
}

// ResetConn implements OnlinePlugin.ResetConn.
func (s *Storage) ResetConn(tctx *tcontext.Context) error {
	return s.dbConn.ResetConn(tctx)
}

// Close closes database connection.
func (s *Storage) Close() {
	s.Lock()
	defer s.Unlock()

	dbconn.CloseBaseDB(s.logCtx, s.db)
}

func (s *Storage) prepare(tctx *tcontext.Context) error {
	if err := s.createSchema(tctx); err != nil {
		return err
	}

	return s.createTable(tctx)
}

func (s *Storage) createSchema(tctx *tcontext.Context) error {
	sql := fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", s.schema)
	_, err := s.dbConn.ExecuteSQL(tctx, []string{sql})
	return terror.WithScope(err, terror.ScopeDownstream)
}

func (s *Storage) createTable(tctx *tcontext.Context) error {
	sql := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id VARCHAR(32) NOT NULL,
			ghost_schema VARCHAR(128) NOT NULL,
			ghost_table VARCHAR(128) NOT NULL,
			ddls text,
			update_time timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			UNIQUE KEY uk_id_schema_table (id, ghost_schema, ghost_table)
		)`, s.tableName)
	_, err := s.dbConn.ExecuteSQL(tctx, []string{sql})
	return terror.WithScope(err, terror.ScopeDownstream)
}

// CheckAndUpdate try to check and fix the schema/table case-sensitive issue.
func (s *Storage) CheckAndUpdate(
	tctx *tcontext.Context,
	schemaMap map[string]string,
	tablesMap map[string]map[string]string,
	realNameFn func(table string) string,
) error {
	s.Lock()
	defer s.Unlock()

	changedSchemas := make([]string, 0)
	for schema, tblDDLInfos := range s.ddls {
		realSchema, hasChange := schemaMap[schema]
		if !hasChange {
			realSchema = schema
		} else {
			changedSchemas = append(changedSchemas, schema)
		}
		tblMap := tablesMap[schema]
		for tbl, ddlInfos := range tblDDLInfos {
			realTbl, tableChange := tblMap[tbl]
			if !tableChange {
				realTbl = tbl
				tableChange = hasChange
			}
			if tableChange {
				targetTable := realNameFn(realTbl)
				ddlInfos.Table = targetTable
				err := s.saveToDB(tctx, realSchema, realTbl, ddlInfos)
				if err != nil {
					return err
				}
				err = s.delete(tctx, schema, tbl)
				if err != nil {
					return err
				}
			}
		}
	}
	for _, schema := range changedSchemas {
		ddl := s.ddls[schema]
		s.ddls[schemaMap[schema]] = ddl
		delete(s.ddls, schema)
	}
	return nil
}