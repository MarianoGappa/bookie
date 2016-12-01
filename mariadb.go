package main

import (
	"database/sql"
	"io/ioutil"
	"regexp"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	_ "github.com/go-sql-driver/mysql"
)

var (
	errMySQLDuplicateEntry = uint16(0x426)
)

type mariaDB struct {
	db *sql.DB
}

func mustSetupMariaDB(config mariadbConfig) *mariaDB {
	db, err := setupMariaDB(config)
	if err != nil {
		log.WithFields(log.Fields{"err": err}).Fatal("Could not setup MariaDB.")
	}
	return db
}

func setupMariaDB(conf mariadbConfig) (*mariaDB, error) {
	db, err := sql.Open("mysql", conf.URL)
	if err != nil {
		return nil, err
	}

	if err = db.Ping(); err != nil {
		return nil, err
	}

	log.Infof("Set up MariaDB connection for %#v.", conf.URL)

	mustSetupTables(db)

	log.Infof("Initialize schema and tables")

	return &mariaDB{db}, nil
}

func mustSetupTables(db *sql.DB) {
	createTableRegexp := regexp.MustCompile("(?s)(CREATE TABLE.+?;)")

	queries, err := ioutil.ReadFile("mariadb.sql")
	if err != nil {
		log.WithFields(log.Fields{"err": err}).Fatalf("Failed to read cql initialization file.")
	}

	createQueries := createTableRegexp.FindAllString(string(queries), -1)
	for _, query := range createQueries {
		mustRunQuery(db, query)
	}
}

func mustRunQuery(db *sql.DB, sql string) {
	for _, q := range strings.Split(sql, ";") {
		if len(q) > 5 {
			_, err := db.Exec(q)
			if err != nil {
				log.WithFields(log.Fields{"query": q, "err": err}).Fatal("Failed to run sql in mariadb")
			}
		}
	}
}

func (m *mariaDB) saveScrape(topic string, partition int32, offset int64) error {
	q := `INSERT INTO bookie.scrape(topic, topic_partition, topic_lastOffset, updated) values (?, ?, ?, UTC_TIMESTAMP())`

	_, err := m.db.Exec(q,
		topic,
		partition,
		offset,
	)

	if err != nil {
		fs := log.Fields{
			"tenant":     topic,
			"campaignID": partition,
			"execution":  offset,
			"err":        err,
		}
		log.WithFields(fs).Error("Failed to save scrape")
		return err
	}

	return nil
}

func (m *mariaDB) getLastNFSMs(n int) ([]fsm, error) {
	fsms := []fsm{}
	q := `SELECT 
			f.fsmID, f.created, o.topic, o.topic_partition, o.startOffset, o.lastOffset, o.count
		  FROM 
		  		(SELECT fsmID, created FROM bookie.fsm ORDER BY created DESC LIMIT ?) f 
		  INNER JOIN 
		  		offset o USING fsmID`

	dbRows, err := m.db.Query(q, n)
	if err != nil {
		return fsms, err
	}

	defer closeRows(dbRows)

	fsmMap := map[string]fsm{}
	for dbRows.Next() {
		var fsmID, topicValue string
		var partitionValue int32
		var startOffset, lastOffset, count int64
		var created time.Time

		if err = dbRows.Scan(&fsmID, &created, &topicValue, &partitionValue, &startOffset, &lastOffset, &count); err != nil {
			log.WithFields(log.Fields{"err": err, "number": n}).Errorf("failed to get last n fsms")
			return fsms, err
		}

		f, ok := fsmMap[fsmID]
		if !ok {
			f = fsm{
				ID:      fsmID,
				Created: created,
				Topics:  map[string]topic{},
			}
			fsmMap[fsmID] = f
		}

		t, ok := f.Topics[topicValue]
		if !ok {
			t = topic{
				Partitions: map[int32]partition{},
				Count:      0,
			}
			f.Topics[topicValue] = t
		}

		_, ok = t.Partitions[partitionValue]
		if !ok {
			p := partition{
				Start: startOffset,
				End:   lastOffset,
				Count: count,
			}
			t.Partitions[partitionValue] = p
		}
	}

	for _, f := range fsmMap {
		fsms = append(fsms, f)
	}

	return fsms, nil
}

func (m *mariaDB) saveFSM(f fsmDataPoint) error {
	q := `INSERT INTO bookie.fsm(fsmID, created) values (?, ?) ON DUPLICATE KEY UPDATE created = ?;
	INSERT INTO bookie.offset(fsmID, topic, topic_partition, startOffset, lastOffset, updated) values (?, ?, ?, ?, ?, UTC_TIMESTAMP())`

	_, err := m.db.Exec(q,
		f.fsmID,
		f.created,
		f.created,
		f.fsmID,
		f.topic,
		f.partition,
		f.startOffset,
		f.lastOffset,
	)

	if err != nil {
		fs := log.Fields{
			"fsmID":       f.fsmID,
			"created":     f.created,
			"topic":       f.topic,
			"partition":   f.partition,
			"startOffset": f.startOffset,
			"lastOffset":  f.lastOffset,
		}
		log.WithFields(fs).Error("Failed to save FSM")
		return err
	}

	return nil
}

func (m *mariaDB) saveAlias(fsmID string, fsmAlias string) error {
	q := `INSERT INTO bookie.fsmAliases(fsmID, fsmAlias) (?, ?)`

	_, err := m.db.Exec(q,
		fsmID,
		fsmAlias,
	)

	if err != nil {
		fs := log.Fields{
			"fsmID":    fsmID,
			"fsmAlias": fsmAlias,
		}
		log.WithFields(fs).Error("Failed to save FSM")
		return err
	}

	return nil
}

func (m *mariaDB) mustLoadScrapes() map[string]topicRecord {
	trs := map[string]topicRecord{}

	q := `SELECT topic, partition, lastOffset FROM bookie.scrape`
	dbRows, err := m.db.Query(q)
	if err != nil {
		log.Fatal(err)
	}

	for dbRows.Next() {
		var topic string
		var partition int32
		var lastOffset int64

		if err = dbRows.Scan(&topic, &partition, &lastOffset); err != nil {
			log.Fatal(err)
		}
		if _, ok := trs[topic]; !ok {
			trs[topic] = topicRecord{
				topic:      topic,
				partitions: map[int32]int64{},
			}
		}
		trs[topic].partitions[partition] = lastOffset
	}

	return trs
}

func (m *mariaDB) findFSM(fsmID string) ([]FSMRow, error) {
	var (
		err error
		fs  = log.Fields{"fsmID": fsmID}
	)
	rows := []FSMRow{}

	q := `SELECT fsmID, topic, partition, startOffset, lastOffset, updated FROM bookie.fsm WHERE fsmID = ?`
	dbRows, err := m.db.Query(q, fsmID)
	if err != nil {
		return rows, err
	}

	defer closeRows(dbRows)

	for dbRows.Next() {
		var fsmID, topic string
		var partition int32
		var startOffset, lastOffset int64
		var updated time.Time

		if err = dbRows.Scan(&fsmID, &topic, &partition, &startOffset, &lastOffset, &updated); err != nil {
			fs["error"] = err
			log.WithFields(fs).Errorf("failed to scan execution uuid")
			return rows, err
		}
		rows = append(rows, FSMRow{
			FSMID:       fsmID,
			Topic:       topic,
			Partition:   partition,
			StartOffset: startOffset,
			LastOffset:  lastOffset,
			Updated:     updated,
		})
	}

	return rows, nil
}

func closeRows(rows *sql.Rows) {
	if err := rows.Close(); err != nil {
		log.WithField("error", err).Error("failed to close rows")
	}
}