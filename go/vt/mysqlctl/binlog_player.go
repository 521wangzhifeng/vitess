// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mysqlctl

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/mysql"
	"github.com/youtube/vitess/go/mysql/proto"
	"github.com/youtube/vitess/go/rpcplus"
	"github.com/youtube/vitess/go/stats"
	"github.com/youtube/vitess/go/vt/key"
	cproto "github.com/youtube/vitess/go/vt/mysqlctl/proto"
)

var (
	SLOW_TXN_THRESHOLD        = time.Duration(100 * time.Millisecond)
	ROLLBACK                  = "rollback"
	BLPL_STREAM_COMMENT_START = "/* _stream "
	BLPL_SPACE                = " "
	UPDATE_RECOVERY           = "update _vt.blp_checkpoint set master_filename='%v', master_position=%v, group_id='%v', txn_timestamp=unix_timestamp(), time_updated=%v where source_shard_uid=%v"
	UPDATE_RECOVERY_LAST_EOF  = "update _vt.blp_checkpoint set last_eof_group_id='%v' where source_shard_uid=%v"
	SELECT_FROM_RECOVERY      = "select * from _vt.blp_checkpoint where source_shard_uid=%v"
)

// binlogRecoveryState is the checkpoint data we read / save into
// _vt.blp_checkpoint table
type binlogRecoveryState struct {
	Uid      uint32
	Addr     string
	Position cproto.ReplicationCoordinates
}

// VtClient is a high level interface to the database
type VtClient interface {
	Connect() error
	Begin() error
	Commit() error
	Rollback() error
	Close()
	ExecuteFetch(query string, maxrows int, wantfields bool) (qr *proto.QueryResult, err error)
}

// DummyVtClient is a VtClient that writes to a writer instead of executing
// anything
type DummyVtClient struct {
	stdout *bufio.Writer
}

func NewDummyVtClient() *DummyVtClient {
	stdout := bufio.NewWriterSize(os.Stdout, 16*1024)
	return &DummyVtClient{stdout}
}

func (dc DummyVtClient) Connect() error {
	return nil
}

func (dc DummyVtClient) Begin() error {
	dc.stdout.WriteString("BEGIN;\n")
	return nil
}
func (dc DummyVtClient) Commit() error {
	dc.stdout.WriteString("COMMIT;\n")
	return nil
}
func (dc DummyVtClient) Rollback() error {
	dc.stdout.WriteString("ROLLBACK;\n")
	return nil
}
func (dc DummyVtClient) Close() {
	return
}

func (dc DummyVtClient) ExecuteFetch(query string, maxrows int, wantfields bool) (qr *proto.QueryResult, err error) {
	dc.stdout.WriteString(string(query) + ";\n")
	return &proto.QueryResult{Fields: nil, RowsAffected: 1, InsertId: 0, Rows: nil}, nil
}

// DBClient is a real VtClient backed by a mysql connection
type DBClient struct {
	dbConfig *mysql.ConnectionParams
	dbConn   *mysql.Connection
}

func NewDbClient(dbConfig *mysql.ConnectionParams) *DBClient {
	dbClient := &DBClient{}
	dbClient.dbConfig = dbConfig
	return dbClient
}

func (dc *DBClient) handleError(err error) {
	// log.Errorf("in DBClient handleError %v", err.(error))
	if sqlErr, ok := err.(*mysql.SqlError); ok {
		if sqlErr.Number() >= 2000 && sqlErr.Number() <= 2018 { // mysql connection errors
			dc.Close()
		}
		if sqlErr.Number() == 1317 { // Query was interrupted
			dc.Close()
		}
	}
}

func (dc *DBClient) Connect() error {
	var err error
	dc.dbConn, err = mysql.Connect(*dc.dbConfig)
	if err != nil {
		return fmt.Errorf("error in connecting to mysql db, err %v", err)
	}
	return nil
}

func (dc *DBClient) Begin() error {
	_, err := dc.dbConn.ExecuteFetch(cproto.BEGIN, 1, false)
	if err != nil {
		log.Errorf("BEGIN failed w/ error %v", err)
		dc.handleError(err)
	}
	return err
}

func (dc *DBClient) Commit() error {
	_, err := dc.dbConn.ExecuteFetch(cproto.COMMIT, 1, false)
	if err != nil {
		log.Errorf("COMMIT failed w/ error %v", err)
		dc.dbConn.Close()
	}
	return err
}

func (dc *DBClient) Rollback() error {
	_, err := dc.dbConn.ExecuteFetch(ROLLBACK, 1, false)
	if err != nil {
		log.Errorf("ROLLBACK failed w/ error %v", err)
		dc.dbConn.Close()
	}
	return err
}

func (dc *DBClient) Close() {
	if dc.dbConn != nil {
		dc.dbConn.Close()
		dc.dbConn = nil
	}
}

func (dc *DBClient) ExecuteFetch(query string, maxrows int, wantfields bool) (*proto.QueryResult, error) {
	mqr, err := dc.dbConn.ExecuteFetch(query, maxrows, wantfields)
	if err != nil {
		log.Errorf("ExecuteFetch failed w/ error %v", err)
		dc.handleError(err)
		return nil, err
	}
	qr := proto.QueryResult(*mqr)
	return &qr, nil
}

// blplStats is the internal stats of this player
type blplStats struct {
	queryCount    *stats.Counters
	txnCount      *stats.Counters
	queriesPerSec *stats.Rates
	txnsPerSec    *stats.Rates
	txnTime       *stats.Timings
	queryTime     *stats.Timings
}

func NewBlplStats() *blplStats {
	bs := &blplStats{}
	bs.txnCount = stats.NewCounters("")
	bs.queryCount = stats.NewCounters("")
	bs.queriesPerSec = stats.NewRates("", bs.queryCount, 15, 60e9)
	bs.txnsPerSec = stats.NewRates("", bs.txnCount, 15, 60e9)
	bs.txnTime = stats.NewTimings("")
	bs.queryTime = stats.NewTimings("")
	return bs
}

// statsJSON returns a json encoded version of stats
func (bs *blplStats) statsJSON() string {
	buf := bytes.NewBuffer(make([]byte, 0, 128))
	fmt.Fprintf(buf, "{")
	fmt.Fprintf(buf, "\n \"TxnCount\": %v,", bs.txnCount)
	fmt.Fprintf(buf, "\n \"QueryCount\": %v,", bs.queryCount)
	fmt.Fprintf(buf, "\n \"QueriesPerSec\": %v,", bs.queriesPerSec)
	fmt.Fprintf(buf, "\n \"TxnPerSec\": %v", bs.txnsPerSec)
	fmt.Fprintf(buf, "\n \"TxnTime\": %v,", bs.txnTime)
	fmt.Fprintf(buf, "\n \"QueryTime\": %v,", bs.queryTime)
	fmt.Fprintf(buf, "\n}")
	return buf.String()
}

// BinlogPlayer is handling reading a stream of updates from BinlogServer
type BinlogPlayer struct {
	// filters for replication
	keyRange key.KeyRange

	// saved position in _vt.blp_checkpoint
	uid           uint32
	recoveryState binlogRecoveryState

	// runtime variables used for replication
	inTxn      bool
	txnBuffer  []*cproto.BinlogResponse
	dbClient   VtClient
	txnIndex   int
	batchStart time.Time

	// configuration
	tables         []string
	txnBatch       int
	maxTxnInterval time.Duration
	execDdl        bool

	// runtime stats
	blplStats *blplStats
}

func NewBinlogPlayer(dbClient VtClient, keyRange key.KeyRange, uid uint32, startPosition *binlogRecoveryState, tables []string, txnBatch int, maxTxnInterval time.Duration, execDdl bool) (*BinlogPlayer, error) {
	if err := startPositionValid(startPosition); err != nil {
		return nil, err
	}

	blp := new(BinlogPlayer)
	blp.keyRange = keyRange
	blp.uid = uid
	blp.recoveryState = *startPosition
	blp.inTxn = false
	blp.txnBuffer = make([]*cproto.BinlogResponse, 0, MAX_TXN_BATCH)
	blp.dbClient = dbClient
	blp.txnIndex = 0
	blp.batchStart = time.Now()
	blp.tables = tables
	blp.txnBatch = txnBatch
	blp.maxTxnInterval = maxTxnInterval
	blp.execDdl = execDdl
	blp.blplStats = NewBlplStats()
	return blp, nil
}

func (blp *BinlogPlayer) StatsJSON() string {
	return blp.blplStats.statsJSON()
}

func (blp *BinlogPlayer) writeRecoveryPosition(currentPosition *cproto.ReplicationCoordinates) error {
	blp.recoveryState.Position = *currentPosition
	updateRecovery := fmt.Sprintf(UPDATE_RECOVERY,
		currentPosition.MasterFilename,
		currentPosition.MasterPosition,
		currentPosition.GroupId,
		time.Now().Unix(),
		blp.uid)

	queryStartTime := time.Now()
	qr, err := blp.dbClient.ExecuteFetch(updateRecovery, 0, false)
	if err != nil {
		return fmt.Errorf("Error %v in writing recovery info %v", err, updateRecovery)
	}
	if qr.RowsAffected != 1 {
		return fmt.Errorf("Cannot update blp_recovery table, affected %v rows", qr.RowsAffected)
	}
	blp.blplStats.txnTime.Record("QueryTime", queryStartTime)
	if time.Now().Sub(queryStartTime) > SLOW_TXN_THRESHOLD {
		log.Infof("SLOW QUERY '%v'", updateRecovery)
	}
	return nil
}

func (blp *BinlogPlayer) saveLastEofGroupId(groupId string) error {
	if err := blp.dbClient.Begin(); err != nil {
		return fmt.Errorf("Failed query BEGIN, err: %s", err)
	}

	updateRecovery := fmt.Sprintf(UPDATE_RECOVERY_LAST_EOF,
		groupId,
		blp.uid)

	queryStartTime := time.Now()
	qr, err := blp.dbClient.ExecuteFetch(updateRecovery, 0, false)
	if err != nil {
		return fmt.Errorf("Error %v in writing recovery info %v", err, updateRecovery)
	}
	if qr.RowsAffected != 1 {
		return fmt.Errorf("Cannot update blp_recovery table, affected %v rows", qr.RowsAffected)
	}
	blp.blplStats.txnTime.Record("QueryTime", queryStartTime)
	if time.Now().Sub(queryStartTime) > SLOW_TXN_THRESHOLD {
		log.Infof("SLOW QUERY '%v'", updateRecovery)
	}

	if err := blp.dbClient.Commit(); err != nil {
		return fmt.Errorf("Failed query 'COMMIT', err: %s", err)
	}
	return nil
}

func startPositionValid(startPos *binlogRecoveryState) error {
	if startPos.Addr == "" {
		return fmt.Errorf("invalid connection params, empty Addr")
	}
	if (startPos.Position.MasterFilename == "" || startPos.Position.MasterPosition == 0) && (startPos.Position.GroupId == "") {
		return fmt.Errorf("invalid start coordinates, need GroupId or MasterFilename+MasterPosition")
	}
	return nil
}

func ReadStartPosition(dbClient VtClient, uid uint32) (*binlogRecoveryState, error) {
	brs := new(binlogRecoveryState)
	brs.Uid = uid

	selectRecovery := fmt.Sprintf(SELECT_FROM_RECOVERY, uid)
	qr, err := dbClient.ExecuteFetch(selectRecovery, 1, true)
	if err != nil {
		return nil, fmt.Errorf("Error %v in selecting from recovery table %v", err, selectRecovery)
	}
	if qr.RowsAffected != 1 {
		return nil, fmt.Errorf("Checkpoint information not available in db for %v", uid)
	}
	row := qr.Rows[0]
	for i, field := range qr.Fields {
		switch strings.ToLower(field.Name) {
		case "addr":
			val := row[i]
			if !val.IsNull() {
				brs.Addr = val.String()
			}
		case "master_filename":
			val := row[i]
			if !val.IsNull() {
				brs.Position.MasterFilename = val.String()
			}
		case "master_position":
			val := row[i]
			if !val.IsNull() {
				strVal := val.String()
				masterPos, err := strconv.ParseUint(strVal, 0, 64)
				if err != nil {
					return nil, fmt.Errorf("Couldn't obtain correct value for '%v'", field.Name)
				}
				brs.Position.MasterPosition = masterPos
			}
		case "group_id":
			val := row[i]
			if !val.IsNull() {
				brs.Position.GroupId = val.String()
			}
		default:
			continue
		}
	}
	return brs, nil
}

func (blp *BinlogPlayer) flushTxnBatch() error {
	for {
		txnOk, err := blp.handleTxn()
		if err != nil {
			return err
		}
		if txnOk {
			break
		} else {
			log.Infof("Retrying txn")
			time.Sleep(1)
		}
	}
	blp.inTxn = false
	blp.txnBuffer = blp.txnBuffer[:0]
	blp.txnIndex = 0
	return nil
}

func (blp *BinlogPlayer) processBinlogEvent(binlogResponse *cproto.BinlogResponse) (err error) {
	// Read event
	if binlogResponse.Error != "" {
		// This is to handle the terminal condition where the client is exiting but there
		// maybe pending transactions in the buffer.
		if strings.Contains(binlogResponse.Error, "EOF") {
			log.Infof("Flushing last few txns before exiting, txnIndex %v, len(txnBuffer) %v", blp.txnIndex, len(blp.txnBuffer))
			if blp.txnIndex > 0 && blp.txnBuffer[len(blp.txnBuffer)-1].Data.SqlType == cproto.COMMIT {
				if err := blp.flushTxnBatch(); err != nil {
					return err
				}
			}

			// nothing left to process, we got it all
			if blp.txnIndex == 0 {
				if err := blp.saveLastEofGroupId(binlogResponse.Position.Position.GroupId); err != nil {
					return err
				}
			}
		}
		if binlogResponse.Position.Position.MasterFilename != "" {
			return fmt.Errorf("Error encountered at position %v, err: '%v'", binlogResponse.Position.Position.String(), binlogResponse.Error)
		} else {
			return fmt.Errorf("Error encountered from server %v", binlogResponse.Error)
		}
	}

	switch binlogResponse.Data.SqlType {
	case cproto.DDL:
		if blp.txnIndex > 0 {
			log.Infof("Flushing before ddl, Txn Batch %v len %v", blp.txnIndex, len(blp.txnBuffer))
			if err := blp.flushTxnBatch(); err != nil {
				return err
			}
		}
		if blp.execDdl {
			if err := blp.handleDdl(binlogResponse); err != nil {
				return err
			}
		}
	case cproto.BEGIN:
		if blp.txnIndex == 0 {
			if blp.inTxn {
				return fmt.Errorf("Invalid txn: txn already in progress, len(blp.txnBuffer) %v", len(blp.txnBuffer))
			}
			blp.txnBuffer = blp.txnBuffer[:0]
			blp.inTxn = true
			blp.batchStart = time.Now()
		}
		blp.txnBuffer = append(blp.txnBuffer, binlogResponse)
	case cproto.COMMIT:
		if !blp.inTxn {
			return fmt.Errorf("Invalid event: COMMIT event without a transaction.")
		}
		blp.txnIndex += 1
		blp.txnBuffer = append(blp.txnBuffer, binlogResponse)

		if time.Now().Sub(blp.batchStart) > blp.maxTxnInterval || blp.txnIndex == blp.txnBatch {
			// log.Infof("Txn Batch %v len %v", blp.txnIndex, len(blp.txnBuffer))
			if err := blp.flushTxnBatch(); err != nil {
				return err
			}
		}
	case cproto.DML:
		if !blp.inTxn {
			return fmt.Errorf("Invalid event: DML outside txn context.")
		}
		blp.txnBuffer = append(blp.txnBuffer, binlogResponse)
	default:
		return fmt.Errorf("Unknown SqlType %v '%v'", binlogResponse.Data.SqlType, binlogResponse.Data.Sql)
	}

	return nil
}

// DDL - apply the schema
func (blp *BinlogPlayer) handleDdl(ddlEvent *cproto.BinlogResponse) error {
	for _, sql := range ddlEvent.Data.Sql {
		if sql == "" {
			continue
		}
		if _, err := blp.dbClient.ExecuteFetch(sql, 0, false); err != nil {
			return fmt.Errorf("Error %v in executing sql %v", err, sql)
		}
	}
	var err error
	if err = blp.dbClient.Begin(); err != nil {
		return fmt.Errorf("Failed query BEGIN, err: %s", err)
	}
	if err = blp.writeRecoveryPosition(&ddlEvent.Position.Position); err != nil {
		return err
	}
	if err = blp.dbClient.Commit(); err != nil {
		return fmt.Errorf("Failed query 'COMMIT', err: %s", err)
	}
	return nil
}

func (blp *BinlogPlayer) dmlTableMatch(sqlSlice []string) bool {
	if blp.tables == nil {
		return true
	}
	if len(blp.tables) == 0 {
		return true
	}
	var firstKw string
	for _, sql := range sqlSlice {
		firstKw = strings.TrimSpace(strings.Split(sql, BLPL_SPACE)[0])
		if firstKw != "insert" && firstKw != "update" && firstKw != "delete" {
			continue
		}
		streamCommentIndex := strings.Index(sql, BLPL_STREAM_COMMENT_START)
		if streamCommentIndex == -1 {
			// log.Warningf("sql doesn't have stream comment '%v'", sql)
			// If sql doesn't have stream comment, don't match
			return false
		}
		tableName := strings.TrimSpace(strings.Split(sql[(streamCommentIndex+len(BLPL_STREAM_COMMENT_START)):], BLPL_SPACE)[0])
		for _, table := range blp.tables {
			if tableName == table {
				return true
			}
		}
	}

	return false
}

// Since each batch of txn maybe not contain max txns, we
// flush till the last counter (blp.txnIndex).
// blp.TxnBuffer contains 'n' complete txns, we
// send one begin at the start and then ignore blp.txnIndex - 1 "Commit" events
// and commit the entire batch at the last commit.
func (blp *BinlogPlayer) handleTxn() (bool, error) {
	var err error

	dmlMatch := 0
	txnCount := 0
	var queryCount int64
	var txnStartTime, queryStartTime time.Time

	for _, dmlEvent := range blp.txnBuffer {
		switch dmlEvent.Data.SqlType {
		case cproto.BEGIN:
			continue
		case cproto.COMMIT:
			txnCount += 1
			if txnCount < blp.txnIndex {
				continue
			}
			if err = blp.writeRecoveryPosition(&dmlEvent.Position.Position); err != nil {
				return false, err
			}
			if err = blp.dbClient.Commit(); err != nil {
				return false, fmt.Errorf("Failed query 'COMMIT', err: %s", err)
			}
			// added 1 for recovery dml
			queryCount += 2
			blp.blplStats.queryCount.Add("QueryCount", queryCount)
			blp.blplStats.txnCount.Add("TxnCount", int64(blp.txnIndex))
			blp.blplStats.txnTime.Record("TxnTime", txnStartTime)
		case cproto.DML:
			if blp.dmlTableMatch(dmlEvent.Data.Sql) {
				dmlMatch += 1
				if dmlMatch == 1 {
					if err = blp.dbClient.Begin(); err != nil {
						return false, fmt.Errorf("Failed query 'BEGIN', err: %s", err)
					}
					queryCount += 1
					txnStartTime = time.Now()
				}

				for _, sql := range dmlEvent.Data.Sql {
					queryStartTime = time.Now()
					if _, err = blp.dbClient.ExecuteFetch(sql, 0, false); err != nil {
						if sqlErr, ok := err.(*mysql.SqlError); ok {
							// Deadlock found when trying to get lock
							// Rollback this transaction and exit.
							if sqlErr.Number() == 1213 {
								log.Infof("Detected deadlock, returning")
								_ = blp.dbClient.Rollback()
								return false, nil
							}
						}
						return false, err
					}
					blp.blplStats.txnTime.Record("QueryTime", queryStartTime)
				}
				queryCount += int64(len(dmlEvent.Data.Sql))
			}
		default:
			return false, fmt.Errorf("Invalid SqlType %v", dmlEvent.Data.SqlType)
		}
	}
	return true, nil
}

// ApplyBinlogEvents makes a gob rpc request to BinlogServer
// and processes the events.
func (blp *BinlogPlayer) ApplyBinlogEvents(interrupted chan struct{}) error {
	log.Infof("BinlogPlayer client %v for keyrange '%v-%v' starting @ '%v'",
		blp.uid,
		blp.keyRange.Start.Hex(),
		blp.keyRange.End.Hex(),
		blp.recoveryState.Position)

	log.Infof("Dialing server @ %v", blp.recoveryState.Addr)
	rpcClient, err := rpcplus.DialHTTP("tcp", blp.recoveryState.Addr)
	defer rpcClient.Close()
	if err != nil {
		log.Errorf("Error in dialing to vt_binlog_server, %v", err)
		return fmt.Errorf("Error in dialing to vt_binlog_server, %v", err)
	}

	responseChan := make(chan *cproto.BinlogResponse)
	log.Infof("making rpc request @ %v for keyrange %v-%v", blp.recoveryState.Position.String(), blp.keyRange.Start.Hex(), blp.keyRange.End.Hex())
	blServeRequest := &cproto.BinlogServerRequest{
		StartPosition: blp.recoveryState.Position,
		KeyRange:      blp.keyRange,
	}
	resp := rpcClient.StreamGo("BinlogServer.ServeBinlog", blServeRequest, responseChan)

processLoop:
	for {
		select {
		case response, ok := <-responseChan:
			if !ok {
				break processLoop
			}
			err = blp.processBinlogEvent(response)
			if err != nil {
				return fmt.Errorf("Error in processing binlog event %v", err)
			}
		case <-interrupted:
			return nil
		}
	}
	if resp.Error != nil {
		return fmt.Errorf("Error received from ServeBinlog %v", resp.Error)
	}
	return nil
}
