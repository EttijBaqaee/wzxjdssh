package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
)

func (s *Store) SetDatabaseBudget(bytes int64) error {
	if bytes < 0 {
		return errors.New("negative database budget")
	}
	s.maxBytes.Store(bytes)
	return s.EnforceBudget()
}

func (s *Store) DatabaseBytes() (int64, error) {
	var size int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(s.path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		size += info.Size()
	}
	return size, nil
}

func (s *Store) EnforceBudget() error {
	if s.maxBytes.Load() == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.enforceBudgetLocked()
}

func (s *Store) reclaimDatabase() error {
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	var mode, free int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		return err
	}
	if free > 0 {
		statement := `PRAGMA incremental_vacuum`
		if mode != 2 {
			statement = `VACUUM`
		}
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Normal writes serialize with cleanup. This bounds growth even if attempts
// arrive faster than the periodic maintenance tick. Never evict an active
// connection's metadata: its already recorded oldest attempts can be removed.
func (s *Store) enforceBudgetLocked() error {
	limit := s.maxBytes.Load()
	if limit == 0 {
		return nil
	}
	size, err := s.DatabaseBytes()
	if err != nil || size <= limit {
		return err
	}
	if err = s.reclaimDatabase(); err != nil {
		return err
	}
	size, err = s.DatabaseBytes()
	if err != nil || size <= limit {
		return err
	}
	target := limit * 9 / 10
	for size > target {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		var id int64
		var active bool
		err = tx.QueryRow(`SELECT id,ended_at IS NULL FROM connections WHERE ended_at IS NOT NULL OR EXISTS(SELECT 1 FROM auth_attempts a WHERE a.connection_id=connections.id) ORDER BY started_at,id LIMIT 1`).Scan(&id, &active)
		if errors.Is(err, sql.ErrNoRows) {
			// Expirable policy tables may also occupy space after a source-IP flood.
			_, err = tx.Exec(`DELETE FROM client_cooldowns; DELETE FROM ip_slowdowns`)
			if err != nil {
				tx.Rollback()
				return err
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			if err = s.reclaimDatabase(); err != nil {
				return err
			}
			size, err = s.DatabaseBytes()
			if err != nil {
				return err
			}
			if size <= limit {
				return nil
			}
			return fmt.Errorf("database capacity cannot fit active connection metadata within %d bytes", limit)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		var result sql.Result
		if active {
			result, err = tx.Exec(`DELETE FROM auth_attempts WHERE id IN (SELECT id FROM auth_attempts WHERE connection_id=? ORDER BY at,id LIMIT 100)`, id)
			if err == nil {
				_, err = tx.Exec(`UPDATE connections SET auth_attempts=(SELECT COUNT(*) FROM auth_attempts WHERE connection_id=?) WHERE id=?`, id, id)
			}
		} else {
			result, err = tx.Exec(`DELETE FROM connections WHERE id=?`, id)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err == nil {
			s.capacityDeleted.Add(uint64(n))
		}
		if err = s.reclaimDatabase(); err != nil {
			return err
		}
		size, err = s.DatabaseBytes()
		if err != nil {
			return err
		}
	}
	return nil
}
