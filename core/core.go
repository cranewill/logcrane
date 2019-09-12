// The core package contains all the main logic code
package core

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/cranewill/logcrane/def"
	"github.com/cranewill/logcrane/utils"
	_ "github.com/go-sql-driver/mysql"
	"log"
	"strconv"
	"sync/atomic"
	"time"
)

var craneChan chan def.Logger

func init() {
	craneChan = make(chan def.Logger, 1024)
}

type LogCrane struct {
	MysqlDb                *sql.DB                    // the mysql database handle
	Running                bool                       // is running
	ServerId               string                     // server id
	CreateStatements       map[string]string          // tableName -> createSql
	SingleInsertStatements map[string]string          // tableName -> insertSql
	BatchInsertStatements  map[string]string          // tableName -> insertSql
	ExistTables            map[string]string          // tableName _. tableFullName
	LogCounters            map[string]*def.LogCounter // tableName -> logCounter
	LogChannels            map[string]chan def.Logger // tableName -> channel. every channel deal one type of cLog
}

// Execute throws the logs and put them into a channel to avoid from concurrent panic
func (c *LogCrane) Execute(cLog def.Logger) {
	if c == nil {
		log.Println("Log system not init!")
		return
	}
	if !c.Running {
		return
	}
	craneChan <- cLog
}

// Lift gives every cLog to its own channel waiting for saving
func (c *LogCrane) Lift() {
	for {
		if !c.Running {
			return
		}
		cLog := <-craneChan
		tableName := cLog.TableName()
		if _, exist := c.LogChannels[tableName]; !exist {
			c.LogChannels[tableName] = make(chan def.Logger, 1024)
			c.LogCounters[tableName] = &def.LogCounter{TotalCount: 0, Count: 0}
			go c.Fly(c.LogChannels[tableName], tableName, cLog.RollType(), cLog.SaveType())
		}
		logChan, _ := c.LogChannels[tableName]
		logChan <- cLog
	}
}

// Fly accepts a logs channel and deals the recording tasks of this logs according to the save type
func (c *LogCrane) Fly(logChan chan def.Logger, tableName string, rollType, saveType int32) {
	cleanTime := time.Now().Unix()
	for {
		switch saveType {
		case def.Single:
			cLog := <-logChan
			c.doSingle(cLog, tableName, rollType)
		case def.Batch:
			buffSize := len(logChan)
			dealNum := def.BatchNum
			if buffSize < def.BatchNum {
				dealNum = 0
			}
			if time.Now().Unix()-cleanTime >= def.BatchCleanTime { // no batch insert over BatchCleanTime, clean
				dealNum = buffSize
				cleanTime = time.Now().Unix()
			}
			logs := make([]def.Logger, 0)
			for i := 0; i < dealNum; i++ {
				cLog := <-logChan
				logs = append(logs, cLog)
			}
			c.doBatch(logs, tableName, rollType)
		}
	}
}

// doSingle deals one log recording
func (c *LogCrane) doSingle(cLog def.Logger, tableName string, rollType int32) {
	defer func() {
		if err := recover(); err != nil {
			log.Println(tableName, ":")
			log.Println(err)
		}
	}()
	tableFullName := utils.GetTableFullNameByTableName(tableName, rollType)
	count := c.LogCounters[tableName]
	insertStmt, exist := c.SingleInsertStatements[tableName]
	if !exist { // no insert sql, check if this table created
		err := c.checkCreate(cLog, tableName, tableFullName, rollType)
		if err != nil {
			log.Println("Create table " + tableFullName + " error!")
			log.Println(err)
			return
		}
		insertStmt = utils.GetInsertSql(cLog) // do insert
		c.SingleInsertStatements[tableName] = insertStmt
	}
	err := c.doSingleInsert(cLog, tableFullName, insertStmt)
	if err != nil {
		log.Println("Insert log " + tableFullName + " error!")
		log.Println(err)
		return
	}
	atomic.AddUint64(&count.Count, 1)
	atomic.AddUint64(&count.TotalCount, 1)
}

// doBatch deals a batch of logs
func (c *LogCrane) doBatch(logs []def.Logger, tableName string, rollType int32) {
	defer func() {
		if err := recover(); err != nil {
			log.Println(tableName, ":")
			log.Println(err)
		}
	}()
	tableFullName := utils.GetTableFullNameByTableName(tableName, rollType)
	count := c.LogCounters[tableName]
	insertStmt, exist := c.BatchInsertStatements[tableName]
	if !exist && len(logs) > 0 {
		err := c.checkCreate(logs[0], tableName, tableFullName, rollType)
		if err != nil {
			log.Println("Create table " + tableFullName + " error!")
			log.Println(err)
			return
		}
		insertStmt = utils.GetBatchInsertSql(logs[0])
		c.SingleInsertStatements[tableName] = insertStmt
	}
	err := c.doBatchInsert(logs, tableFullName, insertStmt)
	if err != nil {
		log.Println("Insert log " + tableFullName + " error!")
		log.Println(err)
		return
	}
	atomic.AddUint64(&count.Count, uint64(len(logs)))
	atomic.AddUint64(&count.TotalCount, uint64(len(logs)))
}

// checkCreate creates the table if the table doesn't exist in db
func (c *LogCrane) checkCreate(cLog def.Logger, tableName, tableFullName string, rollType int32) error {
	existTable, exist := c.ExistTables[tableName]
	if !exist || existTable != tableName {
		var result string
		err := c.MysqlDb.QueryRow("SHOW TABLES LIKE '" + tableFullName + "';").Scan(&result)
		if err != nil {
			if err == sql.ErrNoRows { // table not exist in db
				createStmt, exist := c.CreateStatements[tableName]
				if !exist {
					createStmt = utils.GetCreateSql(cLog)
					c.CreateStatements[tableName] = createStmt
				}
				ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
				stmt := fmt.Sprintf(createStmt, tableFullName)
				_, err := c.MysqlDb.ExecContext(ctx, stmt)
				log.Println("Create table ", tableFullName)
				if err != nil {
					log.Println(stmt)
					return err
				}
			} else {
				return err
			}
		}
		c.ExistTables[tableName] = tableFullName
	}
	return nil
}

// doSingleInsert inserts a single cLog
func (c *LogCrane) doSingleInsert(cLog def.Logger, tableFullName, insertStmt string) error {
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	values := utils.GetInsertValues(cLog)
	preparedStmt := insertStmt + "(" + values + ");"
	stmt := fmt.Sprintf(preparedStmt, tableFullName)
	_, err := c.MysqlDb.ExecContext(ctx, stmt)
	if err != nil {
		log.Println(stmt)
		return err
	}
	return nil
}

// doBatchInsert inserts numbers of logs at one time
func (c *LogCrane) doBatchInsert(logs []def.Logger, tableFullName, insertStmt string) error {
	if len(logs) == 0 {
		return nil
	}
	for i := 0; i < len(logs); i++ {
		cLog := logs[i]
		sep := ","
		if i == len(logs)-1 {
			sep = ""
		}
		insertStmt += "(" + utils.GetInsertValues(cLog) + ")" + sep
	}
	insertStmt = fmt.Sprintf(insertStmt, tableFullName) + ";"
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := c.MysqlDb.ExecContext(ctx, insertStmt)
	if err != nil {
		log.Println(insertStmt)
		return err
	}
	return nil
}

// Monitor creates a time ticker with  duration, and prints the monitor log
// of the log system every tick
func (c *LogCrane) Monitor(duration time.Duration) {
	t := time.NewTicker(duration)
	for range t.C {
		for tableName, counter := range c.LogCounters {
			tCount := &counter.TotalCount
			count := &counter.Count
			log.Println(tableName + ": New " + strconv.Itoa(int(atomic.LoadUint64(count))) + ", Total " + strconv.Itoa(int(atomic.LoadUint64(tCount))))
			counter.Count = 0
		}
	}
}

// Stop ends all the goroutine and finish all the logs left,
// use batch insert to finish the logs
func (c *LogCrane) Stop() {
	defer c.MysqlDb.Close()
	for tableName, logChan := range c.LogChannels {
		size := len(logChan)
		if size <= 0 {
			continue
		}
		unFinished := make([]def.Logger, size)
		for i := 0; i < size; i++ {
			unFinished[i] = <-logChan
		}
		rollType := unFinished[0].RollType()
		tableFullName := utils.GetTableFullNameByTableName(tableName, rollType)
		insertStmt, exist := c.BatchInsertStatements[tableName]
		if !exist && size > 0 {
			err := c.checkCreate(unFinished[0], tableName, tableFullName, rollType)
			if err != nil {
				log.Println("Create table " + tableFullName + " error!")
				continue
			}
			insertStmt = utils.GetBatchInsertSql(unFinished[0])
			c.SingleInsertStatements[tableName] = insertStmt
		}
		err := c.doBatchInsert(unFinished, tableFullName, insertStmt)
		if err != nil {
			log.Println("Insert cLog " + tableFullName + " error!")
			break
		}
	}
}
