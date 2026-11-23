package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const backupFormat = "mpts2spts-backup"
const backupVersion = 1

// Backup is the JSON document produced by GET /api/backup and accepted by
// POST /api/restore. It holds the whole configuration: settings, inputs and
// outputs. The session secret is never included.
type Backup struct {
	Format     string            `json:"format"`
	Version    int               `json:"version"`
	AppVersion string            `json:"app_version,omitempty"`
	Created    string            `json:"created,omitempty"`
	Settings   map[string]string `json:"settings"`
	Inputs     []Input           `json:"inputs"`
	Outputs    []Output          `json:"outputs"`
}

// Snapshot reads the full configuration.
func (s *Store) Snapshot() (Backup, error) {
	b := Backup{Format: backupFormat, Version: backupVersion, AppVersion: version,
		Created: time.Now().Format(time.RFC3339), Inputs: []Input{}, Outputs: []Output{}}
	var err error
	if b.Settings, err = s.Settings(); err != nil {
		return b, err
	}
	for k := range b.Settings {
		if ok, _ := validSetting(k, ""); !ok {
			delete(b.Settings, k) // internal keys such as session_secret
		}
	}
	ins, err := s.Inputs()
	if err != nil {
		return b, err
	}
	outs, err := s.Outputs()
	if err != nil {
		return b, err
	}
	b.Inputs = append(b.Inputs, ins...)
	b.Outputs = append(b.Outputs, outs...)
	return b, nil
}

// Replace swaps the whole configuration for b in one transaction. Settings
// not present in b keep their current value. IDs are kept so outputs stay
// attached to their inputs.
func (s *Store) Replace(b Backup) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM outputs`, `DELETE FROM inputs`} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	for _, in := range b.Inputs {
		if _, err := tx.Exec(`INSERT INTO inputs(id,name,url,enabled,services) VALUES(?,?,?,?,?)`,
			in.ID, in.Name, in.URL, in.Enabled, joinSIDs(in.Services)); err != nil {
			return err
		}
	}
	for _, o := range b.Outputs {
		if _, err := tx.Exec(`INSERT INTO outputs(id,input_id,src_sid,out_sid,tsid,onid,name,provider,service_type,url,enabled)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, o.ID, o.InputID, o.SrcSID, o.OutSID, o.TSID, o.ONID,
			o.Name, o.Provider, o.SType, o.URL, o.Enabled); err != nil {
			return err
		}
	}
	for k, v := range b.Settings {
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// validate checks a backup and normalises it in place. Nothing is written
// unless the whole file is valid.
func (b *Backup) validate() error {
	if b.Format != backupFormat {
		return errors.New("not an mpts2spts backup file")
	}
	if b.Version < 1 || b.Version > backupVersion {
		return fmt.Errorf("unsupported backup version %d", b.Version)
	}
	settings := map[string]string{}
	for k, v := range b.Settings {
		v = strings.TrimSpace(v)
		ok, err := validSetting(k, v)
		if err != nil {
			return fmt.Errorf("setting %s: %v", k, err)
		}
		if ok {
			settings[k] = v
		}
	}
	b.Settings = settings

	inIDs, urls := map[int64]bool{}, map[string]string{}
	for i := range b.Inputs {
		in := &b.Inputs[i]
		if in.ID <= 0 || inIDs[in.ID] {
			return fmt.Errorf("input %d: missing or duplicate id", i+1)
		}
		inIDs[in.ID] = true
		su, err := ParseStreamURL(in.URL)
		if err != nil {
			return fmt.Errorf("input %q: %v", in.Name, err)
		}
		in.URL = su.String()
		if prev, dup := urls[in.URL]; dup {
			return fmt.Errorf("input URL %s appears twice (%s)", in.URL, prev)
		}
		urls[in.URL] = "input"
		if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
			in.Name = su.Addr.String()
		}
		in.Services = normSIDs(in.Services)
	}

	outIDs, outURLs := map[int64]bool{}, map[string]bool{}
	for i := range b.Outputs {
		o := &b.Outputs[i]
		if o.ID <= 0 || outIDs[o.ID] {
			return fmt.Errorf("output %d: missing or duplicate id", i+1)
		}
		outIDs[o.ID] = true
		if !inIDs[o.InputID] {
			return fmt.Errorf("output %q refers to input %d, which is not in the backup", o.Name, o.InputID)
		}
		for _, f := range []struct {
			n string
			v int
		}{{"source service id", o.SrcSID}, {"service id", o.OutSID}, {"transport stream id", o.TSID}} {
			if f.v < 1 || f.v > 65535 {
				return fmt.Errorf("output %q: %s must be 1..65535", o.Name, f.n)
			}
		}
		if o.ONID < 0 || o.ONID > 65535 {
			return fmt.Errorf("output %q: invalid original network id", o.Name)
		}
		if o.SType < 0 || o.SType > 255 {
			return fmt.Errorf("output %q: invalid service type", o.Name)
		}
		su, err := ParseStreamURL(o.URL)
		if err != nil {
			return fmt.Errorf("output %q: %v", o.Name, err)
		}
		o.URL = su.String()
		if outURLs[o.URL] {
			return fmt.Errorf("output address %s appears twice", o.URL)
		}
		outURLs[o.URL] = true
		if o.Name = strings.TrimSpace(o.Name); o.Name == "" {
			o.Name = fmt.Sprintf("Service %d", o.SrcSID)
		}
		o.Provider = strings.TrimSpace(o.Provider)
	}
	return nil
}

func (a *App) backup(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	b, err := a.store.Snapshot()
	a.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := "mpts2spts-backup-" + time.Now().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(b)
}

func (a *App) restore(r *http.Request) (any, error) {
	var b Backup
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<20)).Decode(&b); err != nil {
		return nil, fmt.Errorf("cannot read backup file: %v", err)
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	skipIn, skipOut := b.dedupe()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.Replace(b); err != nil {
		return nil, uniqueErr(err, "backup contains conflicting addresses")
	}
	return map[string]int{"inputs": len(b.Inputs), "outputs": len(b.Outputs), "settings": len(b.Settings),
		"skipped_inputs": skipIn, "skipped_outputs": skipOut}, a.Apply()
}
