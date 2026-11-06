package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type storageFault struct{ err error }

func checkDatabaseInitialization(path string) error {
	marker, err := os.Stat(path + ".initialized")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !marker.Mode().IsRegular() {
		return errors.New("invalid database initialization marker")
	}
	info, err := os.Stat(path)
	if err != nil {
		return errors.New("previously initialized database is missing; restore records before restarting")
	}
	if info.Size() < 512 {
		return errors.New("previously initialized database is truncated")
	}
	return nil
}

func markDatabaseInitialized(path string) error {
	marker := path + ".initialized"
	file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = file.WriteString("wzxjdssh database initialized\n"); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func verifyExistingDatabase(db *sql.DB, version int) error {
	if version == 0 {
		var tables int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
			return err
		}
		if tables == 0 {
			return nil
		}
	}
	tables := []string{"connections", "auth_attempts"}
	if version >= 4 {
		tables = append(tables, "web_preferences", "client_cooldowns", "connection_marks", "simulated_sessions", "ip_slowdowns", "ip_probes", "analytics_totals", "analytics_days", "analytics_groups", "analytics_ips")
	}
	for _, name := range tables {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name=?`, name).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("existing database is missing table %s", name)
		}
	}
	if version >= 4 {
		for name := range analyticsTriggers() {
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='trigger' AND name=?`, "wzx_"+name).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				return errors.New("existing database summary trigger is missing")
			}
		}
	}
	var check string
	if err := db.QueryRow(`PRAGMA quick_check(1)`).Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return errors.New("existing database integrity check failed")
	}
	return nil
}

func (s *Store) failStorage(err error) error {
	if err == nil {
		return nil
	}
	fault := &storageFault{err: errors.New("persistent database is unavailable; existing files were preserved")}
	if s.fault.CompareAndSwap(nil, fault) {
		select {
		case s.fatalErrors <- fault.err:
		default:
		}
	}
	return s.fault.Load().err
}

func (s *Store) noteStorageFailure(err error) {
	if err == nil {
		return
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		switch coded.Code() & 255 {
		case 8, 10, 11, 26:
			s.failStorage(err)
		}
	}
}

func (s *Store) checkDatabaseFile() error {
	if fault := s.fault.Load(); fault != nil {
		return fault.err
	}
	if s.fileInfo == nil {
		return nil
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return s.failStorage(err)
	}
	if !os.SameFile(s.fileInfo, info) || info.Size() < 512 {
		return s.failStorage(errors.New("database file removed, replaced or truncated"))
	}
	file, err := os.Open(s.path)
	if err != nil {
		return s.failStorage(err)
	}
	var header [16]byte
	_, err = file.ReadAt(header[:], 0)
	file.Close()
	if err != nil || !bytes.Equal(header[:], []byte("SQLite format 3\x00")) {
		return s.failStorage(errors.New("invalid database header"))
	}
	return nil
}

func (s *Store) Health(ctx context.Context) error {
	if err := s.checkDatabaseFile(); err != nil {
		return err
	}
	var count, triggers int
	err := s.reader.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name IN ('connections','auth_attempts','web_preferences','client_cooldowns','connection_marks','simulated_sessions','ip_slowdowns','ip_probes','analytics_totals','analytics_days','analytics_groups','analytics_ips')),
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='trigger' AND name GLOB 'wzx_*')`).Scan(&count, &triggers)
	s.noteStorageFailure(err)
	if err == nil && (count != 12 || triggers < 12) {
		return s.failStorage(errors.New("database schema objects are missing"))
	}
	return err
}
