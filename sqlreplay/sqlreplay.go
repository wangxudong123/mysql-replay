/*******************************************************************************
 * Copyright (c)  2021 PingCAP, Inc.
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 ******************************************************************************/

/**
 * @Author: guobob
 * @Description:
 * @File:  sqlreplay.go
 * @Version: 1.0.0
 * @Date: 2021/11/6 11:35
 */

package sqlreplay

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sync"
	"time"
	"unsafe"

	"github.com/bobguo/mysql-replay/result"
	"github.com/bobguo/mysql-replay/stats"
	"github.com/bobguo/mysql-replay/stream"
	"github.com/bobguo/mysql-replay/util"
	"github.com/go-sql-driver/mysql"
	"github.com/pingcap/errors"
	"go.uber.org/zap"
)

//Store prepare statement and handle
type statement struct {
	query  string
	handle *sql.Stmt
}

func NewReplayEventHandler(conn stream.ConnID, log *zap.Logger, cfg *util.Config) *ReplayEventHandler {
	return &ReplayEventHandler{
		pconn:          conn,
		log:            log,
		dsn:            cfg.Dsn,
		MySQLConfig:    cfg.MySQLConfig,
		ctx:            context.Background(),
		ch:             make(chan stream.MySQLEvent, 10000),
		wg:             new(sync.WaitGroup),
		stmts:          make(map[string]statement),
		once:           new(sync.Once),
		wf:             NewWriteFile(),
		fileNamePrefix: conn.HashStr() + ":" + conn.SrcAddr(),
		filePath:       cfg.OutputDir,
		storePath:      cfg.StoreDir,
		preFileSize:    cfg.PreFileSize,
		cfg:            cfg,
	}
}

//Used for replay  SQL
type ReplayEventHandler struct {
	pconn stream.ConnID
	dsn   string
	//fsm                         *stream.MySQLFSM
	log                         *zap.Logger
	MySQLConfig                 *mysql.Config
	schema                      string
	pool                        *sql.DB
	conn                        *sql.Conn
	stmts                       map[string]statement
	ctx                         context.Context
	filterStr                   string
	needCompareRes              bool
	needCompareExecTime         bool
	rrLastGetCheckPointTime     time.Time
	rrCheckPoint                time.Time
	rrGetCheckPointTimeInterval int64
	//rrContinueRun               bool
	//rrNeedReplay bool
	once           *sync.Once
	ch             chan stream.MySQLEvent
	wg             *sync.WaitGroup
	file           *os.File
	wf             *WriteFile
	fileNamePrefix string
	fileName       string
	fileOpenTime   time.Time
	filePath       string
	storePath      string
	preFileSize    uint64
	pos            uint64
	cfg            *util.Config
}

type WriteFile struct {
	ch               chan stream.MySQLEvent
	rrStartGoRuntine bool
	wg               *sync.WaitGroup
	once             *sync.Once
}

func NewWriteFile() *WriteFile {
	wf := new(WriteFile)
	wf.ch = make(chan stream.MySQLEvent, 10000)
	wf.wg = new(sync.WaitGroup)
	wf.rrStartGoRuntine = false
	wf.once = new(sync.Once)
	return wf
}

func (h *ReplayEventHandler) GenerateNextFileName() string {
	return h.fileNamePrefix + util.FileNameSuffix.GetNextFileNameSuffix()
}

func (h *ReplayEventHandler) OpenNextFile() error {
	h.fileName = h.GenerateNextFileName()
	var err error
	h.file, err = util.OpenFile(h.filePath, h.fileName)
	if err != nil {
		h.file = nil
		return err
	}
	h.pos = 0
	h.fileOpenTime = time.Now()
	return nil
}

//change file every 10 min
//change file when file size lg than specified
func (h *ReplayEventHandler) CheckIfChangeFile() bool {
	if time.Since(h.fileOpenTime).Seconds() > float64(10*60) {
		return true
	}

	if h.pos > h.preFileSize {
		return true
	}
	return false
}

func (h *ReplayEventHandler) CloseAndBackupFile() error {
	if h.file != nil {
		err := h.file.Sync()
		if err != nil {
			return err
		}
		err = h.file.Close()
		h.file = nil
		if err != nil {
			return err
		}
	}
	if len(h.storePath) > 0 {
		err := os.Rename(h.filePath+"/"+h.fileName, h.storePath+"/"+h.fileName)
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *ReplayEventHandler) DoWriteResToFile() {

	if h.file == nil {
		err := h.OpenNextFile()
		if err != nil {
			h.log.Warn("open file fail , " + err.Error())
			h.wf.wg.Done()
			return
		}
	}

	h.log.Info("thread begin to run for write " + h.fileNamePrefix)
	for {
		e, ok := <-h.wf.ch
		if ok {
			res, err := result.NewResForWriteFile(e.Pr, e.Rr, &e, h.filePath, h.fileNamePrefix,
				h.file, h.pos)
			if err != nil {
				if err != nil {
					h.log.Warn("new write compare result to file struct fail , " + err.Error())
				}
			} else {
				h.pos, err = res.WriteResToFile()
				if err != nil {
					stats.AddStatic("WriteResFileFail", 1, false)
					h.log.Warn("write compare result to file fail , " + err.Error())
				}
			}
			stats.AddStatic("WriteRes", 1, false)
			if h.CheckIfChangeFile() {
				err = h.CloseAndBackupFile()
				if err != nil {
					h.log.Warn("close or backup file fail , " + err.Error())
				}
				err = h.OpenNextFile()
				if err != nil {
					h.log.Error("open file fail , " + err.Error() + " program will exit now")
					//TODO Unable to open the next file, the current solution is to
					//exit the program and consider a better solution later
					os.Exit(-1)
					//break
				}
			}
		} else {
			err := h.CloseAndBackupFile()
			if err != nil {
				h.log.Warn("close and backup file fail , " + err.Error())
			}
			h.wf.wg.Done()
			h.log.Info("thread end to run for write " + h.fileNamePrefix)
			h.log.Info("chan close ,func exit ")
			return
		}

	}

}

func (h *ReplayEventHandler) AsyncWriteResToFile(e stream.MySQLEvent) {
	h.wf.once.Do(
		func() {
			h.wf.wg.Add(1)
			go h.DoWriteResToFile()
		})
	h.wf.ch <- e
	stats.AddStatic("GetRes", 1, false)
	stats.AddStatic("WriteResChanLen", uint64(len(h.wf.ch)), true)
	/*
		if len(h.wf.ch) >90000 && len(h.wf.ch)% 1000 ==0{
			h.log.Warn("write Channel is nearly  full , " + fmt.Sprintf("%v-%v",len(h.wf.ch),100000))
		}
	*/
}

func (h *ReplayEventHandler) WriteEvent(e stream.MySQLEvent) {
	js, err := json.Marshal(e)
	if err != nil {
		stats.AddStatic("FormatJsonFail", 1, false)
		h.log.Warn(fmt.Sprintf("format struct to json fail , %v", err))
		return
	} else {
		h.log.Info(fmt.Sprintf("output of data within 100ms of the moment of consistency , %v", js))
		return
	}
}

func (h *ReplayEventHandler) DoEvent(e stream.MySQLEvent) {
	if e.Type == util.EventStmtPrepare || e.Type == util.EventStmtClose {
		h.ReplayEventAndWriteRes(e)
		return
	}

	handleType := h.cfg.CheckNeedReplay(e.Time)
	switch handleType {
	case util.NotWriteLog:
		return
	case util.NeedWriteLog:
		h.WriteEvent(e)
	case util.NeedReplaySQL:
		//e.Rr = new(stream.ReplayRes)
		e.NewReplayRes()
		//e.InitRr()

		h.ReplayEventAndWriteRes(e)
	default:
		h.log.Warn(fmt.Sprintf("unsport case %v", handleType))
	}
}

func (h *ReplayEventHandler) ReplayEventAndWriteRes(e stream.MySQLEvent) {
	err := h.ApplyEvent(h.ctx, &e)
	if err != nil {
		if mysqlError, ok := err.(*mysql.MySQLError); ok {
			e.Rr.ErrNO = mysqlError.Number
			e.Rr.ErrDesc = mysqlError.Message
		} else {
			e.Rr.ErrNO = 20000
			e.Rr.ErrDesc = "Failed to execute SQL and failed to convert to mysql error"
		}
	}
	stats.AddStatic("DealSQL", 1, false)
	h.AsyncWriteResToFile(e)
}

func (h *ReplayEventHandler) ReplayEvent(ch chan stream.MySQLEvent, wg *sync.WaitGroup) {
	defer func() {
		if err := recover(); err != nil {
			h.log.Warn(err.(string))
		}

	}()
	h.log.Info("thread begin to run for apply mysql event " + h.fileNamePrefix)
	for {
		e, ok := <-ch
		if ok {
			h.DoEvent(e)
			h.writeEventToFile(e)
		} else {
			wg.Done()
			h.log.Info("thread end to run for apply mysql event " + h.fileNamePrefix)
			h.log.Info("chan close ,func exit ")
			return
		}
	}

}

var eventsMap sync.Map

func (h *ReplayEventHandler) writeEventToFile(e stream.MySQLEvent) {
	hash := e.Conn.HashStr()

	events := []stream.MySQLEvent{}
	if v, ok := eventsMap.Load(hash); ok {
		events = v.([]stream.MySQLEvent)
	}
	events = append(events, e)

	eventsMap.Store(hash, events)

	writeMap := map[interface{}]interface{}{}
	eventsMap.Range(func(key, value interface{}) bool {
		writeMap[key] = value
		return true
	})

	jsonBytes, err := json.Marshal(writeMap)
	if err == nil {
		err = os.WriteFile(h.cfg.OutputDir+"/mysql_events.json", jsonBytes, 0644)
		if err != nil {
			h.log.Warn(fmt.Sprintf("write file fail , %v", err))
		}
	}
}

func (h *ReplayEventHandler) OnEvent(e stream.MySQLEvent) {
	//Process SQL events. Note that unlike the events in binlog,
	//this SQL event is raw and may involve multiple rows

	h.once.Do(func() {
		h.wg.Add(1)
		go h.ReplayEvent(h.ch, h.wg)
	})
	h.ch <- e
	stats.AddStatic("GetSQL", 1, false)
	stats.AddStatic("SQLChanLen", uint64(len(h.ch)), true)
	/*if len(h.ch) >90000 && len(h.ch)% 1000 ==0{
		h.log.Warn("sql Channel is nearly  full , " + fmt.Sprintf("%v-%v",len(h.ch),100000))
	}*/

	h.log.Info(fmt.Sprintf("OnEvent: %s", e.String()))
}

func (h *ReplayEventHandler) OnClose() {
	close(h.ch)
	h.wg.Wait()
	//wait write goroutine end
	close(h.wf.ch)
	h.wf.wg.Wait()
	h.quit(false)
}

func (h *ReplayEventHandler) ApplyEvent(ctx context.Context, e *stream.MySQLEvent) error {
	if len(h.cfg.Dsn) == 0 {
		//skip apply mysql event on replay server
		return nil
	}

	//apply mysql event on replay server
	var err error
LOOP:
	switch e.Type {
	case util.EventQuery:
		var mysqlError *mysql.MySQLError
		e.Rr.ColValues = make([][]driver.Value, 0)
		var ok bool
	RETRYCOMQUERY:
		err = h.execute(ctx, e.Query, e)
		//fmt.Println(err)
		if err != nil {
			stats.AddStatic("ExecSQLFail", 1, false)
			if mysqlError, ok = err.(*mysql.MySQLError); ok {
				//If TiDB thrown 1205: Lock wait timeout exceeded; try restarting transaction
				//we try again until execute success
				if mysqlError.Number == 1205 {
					h.log.Warn(fmt.Sprintf("replay sql with lock wait timeout , try again %v", mysqlError))
					e.Rr.ColValues = e.Rr.ColValues[:0][:0]
					goto RETRYCOMQUERY
				}
			}
		}
	case util.EventStmtPrepare:
		err = h.stmtPrepare(ctx, e.StmtID, e.Query)
		if err != nil {
			stats.AddStatic("ExecSQLFail", 1, false)
			if mysqlError, ok := err.(*mysql.MySQLError); ok {
				logstr := fmt.Sprintf("prepare statment exec fail ,%s , %d ,%s ",
					e.Query, mysqlError.Number, mysqlError.Message)
				h.log.Error(logstr)
			} else {
				e.Rr.ErrNO = 20000
				e.Rr.ErrDesc = "exec sql fail and covert  to mysql error struct err"
			}
		}
	case util.EventStmtExecute:
		_, ok := h.stmts[e.StmtID]
		if ok {
			var mysqlError *mysql.MySQLError
			e.Rr.ColValues = make([][]driver.Value, 0)
		RETRYCOMSTMTEXECUTE:
			err = h.stmtExecute(ctx, e.StmtID, e.Params, e)
			if err != nil {
				stats.AddStatic("ExecSQLFail", 1, false)
				if mysqlError, ok = err.(*mysql.MySQLError); ok {
					//If TiDB thrown 1205: Lock wait timeout exceeded; try restarting transaction
					//we try again until execute success
					if mysqlError.Number == 1205 {
						e.Rr.ColValues = e.Rr.ColValues[:0][:0]
						goto RETRYCOMSTMTEXECUTE
					}
				}
			}
		} else {
			err := new(mysql.MySQLError)
			err.Number = 10000
			err.Message = fmt.Sprintf("%v is not exist , maybe prepare fail", e.StmtID)
			return err
		}
	case util.EventStmtClose:
		h.stmtClose(e.StmtID)
	case util.EventHandshake:
		h.quit(false)
		err = h.handshake(ctx, e.DB)
		if err != nil {
			stats.AddStatic("ExecSQLFail", 1, false)
		}
	case util.EventQuit:
		h.quit(false)
	default:
		h.log.Warn("unknown event", zap.Any("value", e))
	}
	if err != nil {
		if sqlErr := errors.Unwrap(err); sqlErr == context.DeadlineExceeded || sqlErr == sql.ErrConnDone || sqlErr == mysql.ErrInvalidConn {
			h.log.Warn("reconnect after "+e.String(), zap.String("cause", sqlErr.Error()))
			h.quit(true)
			err = h.handshake(ctx, h.schema)
			if err != nil {
				h.log.Warn("reconnect error", zap.Error(err))
			} else {
				//reconnect success ,try exec query again
				goto LOOP
			}
		} else {
			h.log.Warn("failed to apply "+e.String(), zap.Error(err))
		}
	}
	return err
}

//connect to server and set autocommit on
func (h *ReplayEventHandler) open(schema string) (*sql.DB, error) {
	cfg := h.MySQLConfig
	if len(schema) > 0 && cfg.DBName != schema {
		cfg = cfg.Clone()
		cfg.DBName = schema
	}
	return sql.Open("mysql", cfg.FormatDSN())
}

//Handle Handshake messages, similar to Use Database
func (h *ReplayEventHandler) handshake(ctx context.Context, schema string) error {
	pool, err := h.open(schema)
	if err != nil {
		return err
	}
	h.pool = pool
	h.schema = schema
	_, err = h.getConn(ctx)
	return err
}

// Conn returns a single connection by either opening a new connection
// or returning an existing connection from the connection pool. Conn will
// block until either a connection is returned or ctx is canceled.
// Queries run on the same Conn will be run in the same database session.
//
// Every Conn must be returned to the database pool after use by
// calling Conn.Close.
func (h *ReplayEventHandler) getConn(ctx context.Context) (*sql.Conn, error) {
	var err error
	if h.pool == nil {
		h.pool, err = h.open(h.schema)
		//fmt.Println(477,h.pool,h.schema,err)
		if err != nil {
			return nil, err
		}
	}
	if h.conn == nil {
		h.conn, err = h.pool.Conn(ctx)
		if err != nil {
			//fmt.Println(485,err)
			return nil, err
		}
		//stats.Add(stats.Connections, 1)
	}
	return h.conn, nil
}

//Disconnect from replay server
func (h *ReplayEventHandler) quit(reconnect bool) {
	for id, stmt := range h.stmts {
		if stmt.handle != nil {
			if err := stmt.handle.Close(); err != nil {
				h.log.Warn("close stmt.handle fail ," + err.Error())
			}
			stmt.handle = nil
		}
		if reconnect {
			h.stmts[id] = stmt
		} else {
			delete(h.stmts, id)
		}
	}
	if h.conn != nil {
		if err := h.conn.Close(); err != nil {
			h.log.Warn("close conn fail ," + err.Error())
		}
		h.conn = nil
		//stats.Add(stats.Connections, -1)
	}
	if h.pool != nil {
		if err := h.pool.Close(); err != nil {
			h.log.Warn("close pool fail ," + err.Error())
		}
		h.pool = nil
	}
}

//Execute SQL on replay Server
func (h *ReplayEventHandler) execute(ctx context.Context, query string, e *stream.MySQLEvent) error {
	conn, err := h.getConn(ctx)
	//fmt.Println(526,err)
	if err != nil {
		return err
	}
	//stats.Add(stats.Queries, 1)
	//stats.Add(stats.ConnRunning, 1)
	e.Rr.SqlBeginTime = uint64(time.Now().UnixNano())
	e.Rr.SqlStatment = query
	//fmt.Println(query)
	rows, err := conn.QueryContext(ctx, query)
	e.Rr.SqlEndTime = uint64(time.Now().UnixNano())
	defer func() {
		if rows != nil {
			if rs := rows.Close(); rs != nil {
				h.log.Warn("close row fail," + rs.Error())
			}
		}
	}()
	//stats.Add(stats.ConnRunning, -1)
	if err != nil {
		//stats.Add(stats.FailedQueries, 1)
		return err
	}
	for rows.Next() {
		h.ReadRowValues(rows, e)
	}

	return nil
}

//Exec prepare statment on replay sql
func (h *ReplayEventHandler) stmtPrepare(ctx context.Context, id string, query string) error {
	stmt := h.stmts[id]
	stmt.query = query
	if stmt.handle != nil {
		if err := stmt.handle.Close(); err != nil {
			h.log.Warn("close stmt handle fail ," + err.Error())
		}
		stmt.handle = nil
	}
	delete(h.stmts, id)
	conn, err := h.getConn(ctx)
	if err != nil {
		return err
	}
	//stats.Add(stats.StmtPrepares, 1)
	stmt.handle, err = conn.PrepareContext(ctx, stmt.query)
	if err != nil {
		//stats.Add(stats.FailedStmtPrepares, 1)
		return err
	}
	h.stmts[id] = stmt
	h.log.Debug(fmt.Sprintf("%v id is %v", query, id))
	return nil
}

//Retrieve the prepare statement from SQL.Stmt
//via the unsafe and reflection mechanisms
func (h *ReplayEventHandler) getQuery(s *sql.Stmt) string {
	rs := reflect.ValueOf(s)
	foo := rs.Elem().FieldByName("query")
	rf := foo
	rf = reflect.NewAt(rf.Type(), unsafe.Pointer(rf.UnsafeAddr())).Elem()
	z := rf.Interface().(string)
	return z
}

//Exec prepare on replay server
func (h *ReplayEventHandler) stmtExecute(ctx context.Context, id string, params []interface{}, e *stream.MySQLEvent) error {
	stmt, err := h.getStmt(ctx, id)
	if err != nil {
		return err
	}

	e.Rr.SqlStatment = h.getQuery(stmt)
	e.Rr.Values = params

	//fmt.Println(e.Rr.SqlStatment,e.Rr.Values)
	//stats.Add(stats.StmtExecutes, 1)
	//stats.Add(stats.ConnRunning, 1)
	e.Rr.SqlBeginTime = uint64(time.Now().UnixNano())
	rows, err := stmt.QueryContext(ctx, params...)
	e.Rr.SqlEndTime = uint64(time.Now().UnixNano())
	defer func() {
		if rows != nil {
			if err := rows.Close(); err != nil {
				h.log.Warn("close rows fail," + err.Error())
			}
		}
	}()
	//stats.Add(stats.ConnRunning, -1)
	if err != nil {
		//stats.Add(stats.FailedStmtExecutes, 1)
		return err
	}
	for rows.Next() {
		h.ReadRowValues(rows, e)
	}

	return nil
}

//Close prepare handle
func (h *ReplayEventHandler) stmtClose(id string) {
	stmt, ok := h.stmts[id]
	if !ok {
		return
	}
	if stmt.handle != nil {
		if err := stmt.handle.Close(); err != nil {
			h.log.Warn("close stmt handle fail," + err.Error())
		}
		stmt.handle = nil
	}
	delete(h.stmts, id)
}

//Get prepare handle ID
func (h *ReplayEventHandler) getStmt(ctx context.Context, id string) (*sql.Stmt, error) {
	stmt, ok := h.stmts[id]
	if ok && stmt.handle != nil {
		return stmt.handle, nil
	} else if !ok {
		return nil, errors.Errorf("no such statement #%d", id)
	}
	conn, err := h.getConn(ctx)
	if err != nil {
		return nil, err
	}
	stmt.handle, err = conn.PrepareContext(ctx, stmt.query)
	if err != nil {
		return nil, err
	}
	h.stmts[id] = stmt
	return stmt.handle, nil
}

//read row values from replay server result
func (h *ReplayEventHandler) ReadRowValues(f *sql.Rows, e *stream.MySQLEvent) {
	//Get the lastcols value from the sql.Rows
	//structure using unsafe and reflection mechanisms
	//and load it into the cache

	rs := reflect.ValueOf(f)
	foo := rs.Elem().FieldByName("lastcols")
	rf := foo
	rf = reflect.NewAt(rf.Type(), unsafe.Pointer(rf.UnsafeAddr())).Elem()
	z := rf.Interface().([]driver.Value)
	rr := make([]driver.Value, 0, len(z))
	var err error
	for i := range z {
		if z[i] == nil {
			rr = append(rr, nil)
			continue
		}
		var a string
		err = stream.ConvertAssignRows(z[i], &a)
		if err == nil {
			rr = append(rr, a)
		} else {
			h.log.Warn("get row values fail , covert column value to string fail ," + err.Error())
		}
	}
	if err == nil {
		e.Rr.ColValues = append(e.Rr.ColValues, rr)
	}
}
