package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS inputs (
	id      INTEGER PRIMARY KEY,
	name    TEXT NOT NULL,
	url     TEXT NOT NULL UNIQUE,
	enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS outputs (
	id           INTEGER PRIMARY KEY,
	input_id     INTEGER NOT NULL REFERENCES inputs(id) ON DELETE CASCADE,
	src_sid      INTEGER NOT NULL,
	out_sid      INTEGER NOT NULL,
	tsid         INTEGER NOT NULL,
	onid         INTEGER NOT NULL,
	name         TEXT NOT NULL,
	provider     TEXT NOT NULL DEFAULT '',
	service_type INTEGER NOT NULL DEFAULT 1,
	url          TEXT NOT NULL UNIQUE,
	enabled      INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS outputs_input ON outputs(input_id);
`

var defaultSettings = map[string]string{
	"auto_group":     "239.100.0.1",
	"auto_port":      "1234",
	"auto_proto":     "udp",
	"auto_localaddr": "",
	"ttl":            "16",
}

type Store struct{ db *sql.DB }

type Input struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

type Output struct {
	ID       int64  `json:"id"`
	InputID  int64  `json:"input_id"`
	SrcSID   int    `json:"src_sid"`
	OutSID   int    `json:"out_sid"`
	TSID     int    `json:"tsid"`
	ONID     int    `json:"onid"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	SType    int    `json:"service_type"`
	URL      string `json:"url"`
	Enabled  bool   `json:"enabled"`
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000&_synchronous=FULL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	for k, v := range defaultSettings {
		db.Exec(`INSERT OR IGNORE INTO settings(key,value) VALUES(?,?)`, k, v)
	}
	return &Store{db}, nil
}

func (s *Store) Settings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		m[k] = v
	}
	return m, rows.Err()
}

func (s *Store) Setting(k string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, k).Scan(&v)
	return v
}

func (s *Store) SetSetting(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
	return err
}

func (s *Store) Inputs() ([]Input, error) {
	rows, err := s.db.Query(`SELECT id, name, url, enabled FROM inputs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Input
	for rows.Next() {
		var in Input
		if err := rows.Scan(&in.ID, &in.Name, &in.URL, &in.Enabled); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) Input(id int64) (Input, error) {
	var in Input
	err := s.db.QueryRow(`SELECT id, name, url, enabled FROM inputs WHERE id=?`, id).
		Scan(&in.ID, &in.Name, &in.URL, &in.Enabled)
	return in, err
}

func (s *Store) SaveInput(in *Input) error {
	if in.ID == 0 {
		r, err := s.db.Exec(`INSERT INTO inputs(name,url,enabled) VALUES(?,?,?)`, in.Name, in.URL, in.Enabled)
		if err != nil {
			return uniqueErr(err, "an input with this URL already exists")
		}
		in.ID, _ = r.LastInsertId()
		return nil
	}
	_, err := s.db.Exec(`UPDATE inputs SET name=?, url=?, enabled=? WHERE id=?`, in.Name, in.URL, in.Enabled, in.ID)
	return uniqueErr(err, "an input with this URL already exists")
}

// AddInputs inserts inputs in one transaction, skipping URLs that exist.
func (s *Store) AddInputs(ins []Input) (ids []int64, skipped []string, err error) {
	ids, skipped = []int64{}, []string{}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	for _, in := range ins {
		r, err := tx.Exec(`INSERT OR IGNORE INTO inputs(name,url,enabled) VALUES(?,?,?)`, in.Name, in.URL, in.Enabled)
		if err != nil {
			return nil, nil, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			skipped = append(skipped, in.URL)
			continue
		}
		id, _ := r.LastInsertId()
		ids = append(ids, id)
	}
	return ids, skipped, tx.Commit()
}

func (s *Store) DeleteInput(id int64) error {
	_, err := s.db.Exec(`DELETE FROM inputs WHERE id=?`, id)
	return err
}

const outCols = `id, input_id, src_sid, out_sid, tsid, onid, name, provider, service_type, url, enabled`

func scanOutput(sc interface{ Scan(...any) error }) (Output, error) {
	var o Output
	err := sc.Scan(&o.ID, &o.InputID, &o.SrcSID, &o.OutSID, &o.TSID, &o.ONID, &o.Name, &o.Provider, &o.SType, &o.URL, &o.Enabled)
	return o, err
}

func (s *Store) Outputs() ([]Output, error) {
	rows, err := s.db.Query(`SELECT ` + outCols + ` FROM outputs ORDER BY input_id, src_sid, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Output
	for rows.Next() {
		o, err := scanOutput(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) Output(id int64) (Output, error) {
	return scanOutput(s.db.QueryRow(`SELECT `+outCols+` FROM outputs WHERE id=?`, id))
}

func (s *Store) SaveOutput(o *Output) error {
	if o.ID == 0 {
		r, err := s.db.Exec(`INSERT INTO outputs(input_id,src_sid,out_sid,tsid,onid,name,provider,service_type,url,enabled)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, o.InputID, o.SrcSID, o.OutSID, o.TSID, o.ONID, o.Name, o.Provider, o.SType, o.URL, o.Enabled)
		if err != nil {
			return uniqueErr(err, "output address "+o.URL+" is already used")
		}
		o.ID, _ = r.LastInsertId()
		return nil
	}
	_, err := s.db.Exec(`UPDATE outputs SET src_sid=?, out_sid=?, tsid=?, onid=?, name=?, provider=?, service_type=?, url=?, enabled=? WHERE id=?`,
		o.SrcSID, o.OutSID, o.TSID, o.ONID, o.Name, o.Provider, o.SType, o.URL, o.Enabled, o.ID)
	return uniqueErr(err, "output address "+o.URL+" is already used")
}

func (s *Store) SetOutputsEnabled(ids []int64, on bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE outputs SET enabled=? WHERE id=?`, on, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteOutputs(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM outputs WHERE id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetAllInputsEnabled(on bool) error {
	_, err := s.db.Exec(`UPDATE inputs SET enabled=?`, on)
	return err
}

func (s *Store) DeleteOutput(id int64) error {
	_, err := s.db.Exec(`DELETE FROM outputs WHERE id=?`, id)
	return err
}

// NextAutoAddr returns the first group:port at or after the configured base
// whose ip:port is not used by any output (on any interface) or in skip.
func (s *Store) NextAutoAddr(skip map[netip.AddrPort]bool) (netip.AddrPort, error) {
	used := map[netip.AddrPort]bool{}
	for k := range skip {
		used[k] = true
	}
	outs, err := s.Outputs()
	if err != nil {
		return netip.AddrPort{}, err
	}
	for _, o := range outs {
		if su, err := ParseStreamURL(o.URL); err == nil {
			used[su.Addr] = true
		}
	}
	base, err := netip.ParseAddr(s.Setting("auto_group"))
	if err != nil || !base.Is4() {
		return netip.AddrPort{}, fmt.Errorf("invalid auto_group setting")
	}
	port, _ := strconv.Atoi(s.Setting("auto_port"))
	if port <= 0 || port > 65535 {
		return netip.AddrPort{}, fmt.Errorf("invalid auto_port setting")
	}
	for a, i := base, 0; i < 1<<16 && a.Is4(); a, i = a.Next(), i+1 {
		if b := a.As4(); b[3] == 0 || b[3] == 255 {
			continue
		}
		if ap := netip.AddrPortFrom(a, uint16(port)); !used[ap] {
			return ap, nil
		}
	}
	return netip.AddrPort{}, errors.New("address pool exhausted")
}

func uniqueErr(err error, msg string) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint") {
		return errors.New(msg)
	}
	return err
}
