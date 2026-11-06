package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func (s *Store) SetDatabaseBudget(bytes int64) error {
	if bytes < 0 {
		return errors.New("negative database budget")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.maxBytes.Store(bytes)
	s.budgetCheckedAt = time.Time{}
	s.budgetReclaimedAt = time.Time{}
	if bytes == 0 {
		_, err := s.db.Exec(`PRAGMA max_page_count=4294967294`)
		return err
	}
	// Initial migration/recovery happens before SSH starts. Online maintenance
	// never uses a full VACUUM; new databases already use incremental vacuum.
	s.budgetPruning = true
	for {
		before := s.capacityDeleted.Load()
		if err := s.enforceBudgetLocked(); err != nil {
			return err
		}
		if !s.budgetPruning {
			break
		}
		if s.capacityDeleted.Load() == before {
			return errors.New("database budget cannot fit active metadata")
		}
	}
	var mode int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	if mode != 2 {
		// Convert a legacy database once, at startup, after pruning it. An
		// attacker cannot repeatedly trigger a full rewrite during service.
		if _, err := s.db.Exec(`PRAGMA auto_vacuum=INCREMENTAL; VACUUM`); err != nil {
			return err
		}
	}
	for {
		var free int64
		if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
			return err
		}
		if free == 0 {
			break
		}
		if err := s.reclaimDatabase(); err != nil {
			return err
		}
	}
	var pageSize int64
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return err
	}
	reserve := min(bytes/4, int64(32*1024*1024))
	_, err := s.db.Exec(fmt.Sprintf(`PRAGMA max_page_count=%d`, max(int64(1), (bytes-reserve)/pageSize)))
	if err != nil {
		return err
	}
	return s.checkpoint()
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
	// Yield the writer between bounded steps, including during idle cleanup.
	end := time.Now().Add(100 * time.Millisecond)
	for {
		s.writeMu.Lock()
		err := s.enforceBudgetLocked()
		more := s.budgetPruning
		s.writeMu.Unlock()
		if err != nil {
			return err
		}
		if !more || time.Now().After(end) {
			return nil
		}
	}
}

func (s *Store) checkpoint() error {
	// A slow administrator query must not hold all authentication writes for
	// busy_timeout. A busy checkpoint is retried after its short reader ends.
	if _, err := s.db.Exec(`PRAGMA busy_timeout=0`); err != nil {
		return err
	}
	defer s.db.Exec(`PRAGMA busy_timeout=5000`)
	var busy, log, done int
	return s.db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &done)
}

func (s *Store) reclaimDatabase() error {
	var mode, free int64
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		return err
	}
	if mode == 2 && free > 0 {
		// This pragma yields a row per reclaimed page. Exec stops at the first row;
		// consume the bounded result so all requested pages are actually reclaimed.
		rows, err := s.db.Query(`PRAGMA incremental_vacuum(64)`)
		if err != nil {
			return err
		}
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return s.checkpoint()
}

type budgetQueryer interface{ QueryRow(string, ...any) *sql.Row }

func databaseWatermarks(limit int64) (high, low int64) {
	usable := limit - min(limit/4, int64(32*1024*1024))
	high = min(usable*95/100, usable-min(usable/3, int64(8*1024*1024)))
	low = min(usable*9/10, high-min(usable/12, int64(1024*1024)))
	return
}

// Before listeners and summary triggers exist, remove closed legacy rows in
// larger transactions. Reusing online 5 ms steps here multiplies FULL commits
// and checkpoints over millions of old records without serving any clients.
func (s *Store) pruneLegacyMigration(limit int64) error {
	high, low := databaseWatermarks(limit)
	live, err := s.liveDatabaseBytes(s.db)
	if err != nil {
		return err
	}
	if live < high {
		return nil
	}
	for live > low {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT c.id,c.auth_attempts,512+COALESCE((SELECT SUM(length(credential)+length(public_key)+256) FROM auth_attempts a WHERE a.connection_id=c.id),0) FROM connections c WHERE c.ended_at IS NOT NULL ORDER BY c.started_at,c.id LIMIT 512`)
		if err != nil {
			tx.Rollback()
			return err
		}
		ids := []any{}
		var attempts, bytes int64
		byteLimit := min(int64(4*1024*1024), max(int64(1), live-low))
		for rows.Next() {
			var id, n, size int64
			if err = rows.Scan(&id, &n, &size); err != nil {
				break
			}
			if len(ids) > 0 && bytes+size > byteLimit {
				break
			}
			ids = append(ids, id)
			attempts += n
			bytes += size
			if bytes >= byteLimit {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err == nil && len(ids) == 0 {
			err = errors.New("database migration budget cannot fit metadata")
		}
		if err == nil {
			_, err = tx.Exec(`DELETE FROM connections WHERE id IN (`+strings.Repeat("?,", len(ids)-1)+`?)`, ids...)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		s.capacityDeleted.Add(uint64(int64(len(ids)) + attempts))
		live, err = s.liveDatabaseBytes(s.db)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) liveDatabaseBytes(q budgetQueryer) (int64, error) {
	var size int64
	err := q.QueryRow(`SELECT (page_count-freelist_count)*page_size FROM pragma_page_count(),pragma_freelist_count(),pragma_page_size()`).Scan(&size)
	return size, err
}

// The writer must not outrun reclamation. Post-commit maintenance alone can
// accumulate a deficit until max_page_count rejects both inserts and deletes.
// Wait before the next transaction, while readers retain their WAL snapshots.
func (s *Store) makeWriteRoomLocked(required int64, credential bool) error {
	limit := s.maxBytes.Load()
	if limit == 0 {
		return nil
	}
	high, _ := databaseWatermarks(limit)
	usable := limit - min(limit/4, int64(32*1024*1024))
	target := usable - required - min(usable/20, int64(1024*1024))
	if credential {
		target = min(target, high)
	}
	if target <= 0 {
		return errors.New("database budget cannot fit write batch")
	}
	for {
		live, err := s.liveDatabaseBytes(s.db)
		if err != nil {
			return err
		}
		if live <= target {
			return nil
		}
		before := s.capacityDeleted.Load()
		if err = s.enforceBudgetTargetLocked(target); err != nil {
			return err
		}
		if before == s.capacityDeleted.Load() {
			return errors.New("database budget cannot fit active metadata")
		}
	}
}

// Each call removes a bounded amount of old data and compacts at most 64 pages.
// Active connection rows are never deleted. Every caller holds writeMu.
func (s *Store) enforceBudgetLocked() error { return s.enforceBudgetTargetLocked(0) }
func (s *Store) enforceBudgetTargetLocked(target int64) error {
	limit := s.maxBytes.Load()
	if limit == 0 {
		return nil
	}
	now := time.Now()
	s.budgetCheckedAt = now
	high, low := databaseWatermarks(limit)
	if target > 0 {
		high = min(high, target)
		low = min(low, target)
	}
	live, err := s.liveDatabaseBytes(s.db)
	if err != nil {
		return err
	}
	if live >= high {
		s.budgetPruning = true
	}
	if s.budgetPruning && live > low {
		var schemaVersion int
		if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&schemaVersion); err != nil {
			return err
		}
		var bulk uint64
		if schemaVersion >= 4 {
			bulk, err = s.pruneClosedBatch(live - low)
			if err != nil {
				return err
			}
		}
		if bulk > 0 {
			s.capacityDeleted.Add(bulk)
			live, err = s.liveDatabaseBytes(s.db)
			if err != nil {
				return err
			}
		} else {
			tx, err := s.db.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var deleted uint64
			started := time.Now()
			for batch := 0; batch < 64; batch++ {
				if batch > 0 && time.Since(started) >= 5*time.Millisecond {
					break
				}
				var id, attempts int64
				var ended bool
				err = s.queryRow(tx, `SELECT id,auth_attempts,ended_at IS NOT NULL FROM connections WHERE ended_at IS NOT NULL OR auth_attempts>0 ORDER BY started_at,id LIMIT 1`).Scan(&id, &attempts, &ended)
				if errors.Is(err, sql.ErrNoRows) {
					break
				}
				if err != nil {
					return err
				}
				if attempts > 0 {
					count, err := s.deleteOldAuth(tx, id, 0)
					if err != nil {
						return err
					}
					deleted += uint64(count)
					if ended {
						result, err := s.exec(tx, `DELETE FROM connections WHERE id=? AND auth_attempts=0`, id)
						if err != nil {
							return err
						}
						n, err := result.RowsAffected()
						if err != nil {
							return err
						}
						deleted += uint64(n)
					}
					if count >= 16 {
						break
					}
				} else {
					if _, err = s.exec(tx, `DELETE FROM connections WHERE id=? AND ended_at IS NOT NULL`, id); err != nil {
						return err
					}
					deleted++
				}
				live, err = s.liveDatabaseBytes(tx)
				if err != nil {
					return err
				}
				if live <= low {
					break
				}
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			s.capacityDeleted.Add(deleted)
			live, err = s.liveDatabaseBytes(s.db)
			if err != nil {
				return err
			}
		}
	}
	if live <= low {
		s.budgetPruning = false
	}
	physical, err := s.DatabaseBytes()
	if err != nil {
		return err
	}
	if s.budgetPruning || physical > limit || now.Sub(s.budgetReclaimedAt) >= time.Second {
		s.budgetReclaimedAt = now
		return s.reclaimDatabase()
	}
	return nil
}

// The byte limit also bounds work for unusually large captured credentials.
func (s *Store) deleteOldAuth(tx *sql.Tx, id, cutoff int64) (int64, error) {
	result, err := s.exec(tx, `WITH sample AS (SELECT id,length(credential)+length(public_key)+512 AS bytes FROM auth_attempts WHERE connection_id=? AND (?=0 OR at<?) ORDER BY id LIMIT 128), bounded AS (SELECT id,SUM(bytes) OVER(ORDER BY id) AS bytes FROM sample)
 DELETE FROM auth_attempts WHERE id IN (SELECT id FROM bounded WHERE bytes<=262144 UNION SELECT MIN(id) FROM sample)`, id, cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	_, err = s.exec(tx, `UPDATE connections SET auth_attempts=MAX(0,auth_attempts-?) WHERE id=?`, count, id)
	return count, err
}
