// The core package contains all the main logic code
package core

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/cranewill/logcrane/def"
	"github.com/cranewill/logcrane/utils"
	_ "github.com/go-sql-driver/mysql"
	log2 "log"
	"time"
)

var craneChan chan def.Logger

func init() {
	craneChan = make(chan def.Logger, 1024)
}

type LogCrane struct {
	MysqlDb                *sql.DB                    // the mysql database handle
	Init                   bool                       // is initialized
	ServerId               string                     // server id
	LogChannels            map[string]chan def.Logger // logName -> channel. every channel deal one type of log
	CreateStatements       map[string]string          // logName -> createSql
	SingleInsertStatements map[string]string          // logName -> insertSql
	BatchInsertStatements  map[string]string          // logName -> insertSql
}

// Execute throws the logs and put them into a channel to avoid from concurrent panic
func (c *LogCrane) Execute(log def.Logger) {
	if c == nil {
		log2.Println("Log system not init!")
		return
	}
	craneChan <- log
}

// Lift gives every log to its own channel waiting for saving
func (c *LogCrane) Lift() {
	for {
		log := <-craneChan
		tableName := log.TableName()
		if _, exist := c.LogChannels[tableName]; !exist {
			c.LogChannels[tableName] = make(chan def.Logger, 1024)
			go c.Fly(c.LogChannels[tableName], tableName, log.RollType(), log.SaveType())
		}
		logChan, _ := c.LogChannels[tableName]
		logChan <- log
	}
}

// Fly accepts a logs channel and deals the recording tasks of this logs according to the save type
func (c *LogCrane) Fly(logChan chan def.Logger, tableName string, rollType, saveType int32) {
	tableFullName := utils.GetTableFullNameByTableName(tableName, rollType)
	for {
		switch saveType {
		case def.Single:
			log := <-logChan
			insertStmt, exist := c.SingleInsertStatements[tableName]
			if !exist { // no insert sql, check if this table created
				err := c.checkCreate(log, tableName, tableFullName, rollType)
				if err != nil {
					log2.Println("Create table " + tableFullName + " error!")
					break
				}
				insertStmt = utils.GetInsertSql(log) // do insert
				c.SingleInsertStatements[tableName] = insertStmt
			}
			err := c.doSingleInsert(log, tableFullName, insertStmt)
			if err != nil {
				log2.Println("Insert log " + tableFullName + " error!")
				break
			}
		case def.Batch:
			buffSize := len(logChan)
			if buffSize < def.BatchNum {
				break
			}
			logs := make([]def.Logger, 0)
			for i := 0; i < def.BatchNum; i++ {
				log := <-logChan
				logs = append(logs, log)
			}
			insertStmt, exist := c.BatchInsertStatements[tableName]
			if !exist {
				err := c.checkCreate(logs[0], tableName, tableFullName, rollType)
				if err != nil {
					log2.Println("Create table " + tableFullName + " error!")
					break
				}
				insertStmt = utils.GetBatchInsertSql(logs[0])
				c.SingleInsertStatements[tableName] = insertStmt
			}
			err := c.doBatchInsert(logs, tableFullName, insertStmt)
			if err != nil {
				log2.Println("Insert log " + tableFullName + " error!")
				break
			}
		}
	}
}

// checkCreate creates the table
func (c *LogCrane) checkCreate(log def.Logger, tableName, tableFullName string, rollType int32) error {
	createStmt, exist := c.CreateStatements[tableName]
	if !exist { // not created, do create
		createStmt = utils.GetCreateSql(log)
		_, err := c.MysqlDb.Exec(fmt.Sprintf(createStmt, tableFullName))
		if err != nil {
			return err
		}
		c.CreateStatements[tableName] = createStmt
	}
	return nil
}

// doSingleInsert inserts a single log
func (c *LogCrane) doSingleInsert(log def.Logger, tableFullName, insertStmt string) error {
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	values := utils.GetInsertValues(log)
	preparedStmt := insertStmt + "(" + values + ");"
	_, err := c.MysqlDb.ExecContext(ctx, fmt.Sprintf(preparedStmt, tableFullName))
	if err != nil {
		failSql := insertStmt
		log2.Println(failSql)
		return err
	}
	return nil
}

// doBatchInsert inserts numbers of logs at one time
func (c *LogCrane) doBatchInsert(logs []def.Logger, tableFullName, insertStmt string) error {
	for i := 0; i < len(logs); i++ {
		log := logs[i]
		sep := ","
		if i == len(logs)-1 {
			sep = ""
		}
		insertStmt += "(" + utils.GetInsertValues(log) + ")" + sep
	}
	insertStmt = fmt.Sprintf(insertStmt, tableFullName) + ";"
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := c.MysqlDb.ExecContext(ctx, insertStmt)
	if err != nil {
		failSql := insertStmt
		log2.Println(failSql)
		return err
	}
	return nil
}