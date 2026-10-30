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

type budgetQueryer interface {
	QueryRow(string, ...any) *sql.Row
}

// Committed WAL pages are included in SQLite's view. Free pages are excluded
// so large old databases are pruned before any potentially expensive VACUUM.
func (s *Store) liveDatabaseBytes(q budgetQueryer) (int64, error) {
	var size int64
	err := q.QueryRow(`SELECT (page_count-freelist_count)*page_size FROM pragma_page_count(),pragma_freelist_count(),pragma_page_size()`).Scan(&size)
	if err != nil {
		return 0, err
	}
	if info, err := os.Stat(s.path + "-shm"); err == nil {
		size += info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	return size, nil
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
	if _, err = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	size, err = s.DatabaseBytes()
	if err != nil || size <= limit {
		return err
	}
	size, err = s.liveDatabaseBytes(s.db)
	if err != nil {
		return err
	}
	target := limit * 9 / 10
	for size > target {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		var deleted uint64
		noCandidates := false
		for batch := 0; batch < 64 && size > target; batch++ {
			var id int64
			var hasAuth bool
			err = tx.QueryRow(`SELECT id,EXISTS(SELECT 1 FROM auth_attempts a WHERE a.connection_id=connections.id) FROM connections WHERE ended_at IS NOT NULL OR EXISTS(SELECT 1 FROM auth_attempts a WHERE a.connection_id=connections.id) ORDER BY started_at,id LIMIT 1`).Scan(&id, &hasAuth)
			if errors.Is(err, sql.ErrNoRows) {
				noCandidates = true
				_, err = tx.Exec(`DELETE FROM client_cooldowns; DELETE FROM ip_slowdowns`)
				if err != nil {
					tx.Rollback()
					return err
				}
				break
			}
			if err != nil {
				tx.Rollback()
				return err
			}
			var result sql.Result
			if hasAuth {
				result, err = tx.Exec(`DELETE FROM auth_attempts WHERE id IN (SELECT id FROM auth_attempts WHERE connection_id=? ORDER BY at,id LIMIT 100)`, id)
			} else {
				result, err = tx.Exec(`DELETE FROM connections WHERE id=?`, id)
			}
			if err != nil {
				tx.Rollback()
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				tx.Rollback()
				return err
			}
			deleted += uint64(n)
			if hasAuth {
				if _, err = tx.Exec(`UPDATE connections SET auth_attempts=MAX(0,auth_attempts-?) WHERE id=?`, n, id); err != nil {
					tx.Rollback()
					return err
				}
				if _, err = tx.Exec(`DELETE FROM connections WHERE id=? AND ended_at IS NOT NULL AND auth_attempts=0`, id); err != nil {
					tx.Rollback()
					return err
				}
			}
			size, err = s.liveDatabaseBytes(tx)
			if err != nil {
				tx.Rollback()
				return err
			}
			// Bound WAL growth when an old connection has many large credentials.
			if hasAuth && n >= 16 {
				break
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		s.capacityDeleted.Add(deleted)
		if noCandidates {
			break
		}
	}
	if err = s.reclaimDatabase(); err != nil {
		return err
	}
	size, err = s.DatabaseBytes()
	if err != nil {
		return err
	}
	if size > limit {
		return fmt.Errorf("database capacity cannot fit active connection metadata within %d bytes", limit)
	}
	return nil
}
