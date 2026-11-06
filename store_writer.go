package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"
)

type storeWrite struct {
	run        func(*sql.Tx) error
	done       chan error
	reserve    int64
	credential bool
}

func sqliteDSN(path string, readOnly bool) string {
	abs, _ := filepath.Abs(path)
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	q := u.Query()
	if readOnly {
		q.Set("mode", "ro")
		for _, pragma := range []string{"busy_timeout(1000)", "query_only(1)", "cache_size(-4096)"} {
			q.Add("_pragma", pragma)
		}
	} else {
		// database/sql may replace a connection after cancellation. These settings
		// must be applied on every new physical connection, not only at startup.
		for _, pragma := range []string{"busy_timeout(5000)", "foreign_keys(1)", "synchronous(FULL)", "secure_delete(ON)", "wal_autocheckpoint(128)", "journal_size_limit(0)", "cache_size(-8192)"} {
			q.Add("_pragma", pragma)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Store) startWriter() {
	s.writes = make(chan *storeWrite, 1024)
	s.urgentWrites = make(chan *storeWrite, 128)
	s.writerDone = make(chan struct{})
	s.budgetErrors = make(chan error, 1)
	go s.writeLoop()
}

func (s *Store) enqueue(run func(*sql.Tx) error) (*storeWrite, error) {
	return s.enqueueWrite(run, 65536, false)
}
func (s *Store) enqueueWrite(run func(*sql.Tx) error, reserve int64, credential bool) (*storeWrite, error) {
	job := &storeWrite{run: run, done: make(chan error, 1), reserve: reserve, credential: credential}
	s.writerMu.RLock()
	if s.writerClosing {
		s.writerMu.RUnlock()
		return nil, errors.New("database is closing")
	}
	if credential {
		s.writes <- job
	} else {
		s.urgentWrites <- job
	}
	s.writerMu.RUnlock()
	return job, nil
}
func (s *Store) write(run func(*sql.Tx) error) error {
	job, err := s.enqueue(run)
	if err != nil {
		return err
	}
	return <-job.done
}
func (s *Store) writeBulk(run func(*sql.Tx) error, reserve int64) error {
	job, err := s.enqueueWrite(run, reserve, true)
	if err != nil {
		return err
	}
	return <-job.done
}

func writeOne(tx *sql.Tx, job *storeWrite) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("database write panicked")
		}
	}()
	return job.run(tx)
}

func (s *Store) commitWrites(jobs []*storeWrite) {
	results := make([]error, len(jobs))
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var tx *sql.Tx
	var reserve int64
	credential := false
	for _, job := range jobs {
		reserve += job.reserve
		credential = credential || job.credential
	}
	err := s.checkDatabaseFile()
	if err == nil {
		err = s.makeWriteRoomLocked(reserve, credential)
	}
	if err == nil {
		tx, err = s.db.Begin()
	}
	if err == nil {
		for i, job := range jobs {
			if _, err = tx.Exec(`SAVEPOINT wzx_write`); err != nil {
				break
			}
			results[i] = writeOne(tx, job)
			if results[i] != nil {
				if _, err = tx.Exec(`ROLLBACK TO wzx_write`); err != nil {
					break
				}
			}
			if _, err = tx.Exec(`RELEASE wzx_write`); err != nil {
				break
			}
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	}
	if err != nil {
		for i := range results {
			results[i] = fmt.Errorf("database commit: %w", err)
		}
	}
	s.noteStorageFailure(err)
	for _, result := range results {
		s.noteStorageFailure(result)
	}
	// Committed writes are acknowledged as committed. A maintenance error must
	// not turn a successful Start into an orphaned record with no returned ID.
	if s.fault.Load() == nil {
		if budgetErr := s.enforceBudgetLocked(); budgetErr != nil {
			s.noteStorageFailure(budgetErr)
			select {
			case s.budgetErrors <- budgetErr:
			default:
			}
		}
	}
	for i, job := range jobs {
		job.done <- results[i]
	}
}

func (s *Store) writeLoop() {
	defer close(s.writerDone)
	urgent, credentials := s.urgentWrites, s.writes
	for urgent != nil || credentials != nil {
		var first *storeWrite
		// Check lifecycle work first so credential backpressure cannot starve
		// admission, authentication replies, completion or administrator writes.
		if urgent != nil {
			select {
			case job, ok := <-urgent:
				if ok {
					first = job
				} else {
					urgent = nil
				}
			default:
			}
		}
		if first == nil && urgent == nil && credentials == nil {
			return
		}
		if first == nil {
			select {
			case job, ok := <-urgent:
				if ok {
					first = job
				} else {
					urgent = nil
				}
			case job, ok := <-credentials:
				if ok {
					first = job
				} else {
					credentials = nil
				}
			}
		}
		if first == nil {
			continue
		}
		jobs := []*storeWrite{first}
		batchLimit := 128
		take := urgent
		if first.credential {
			batchLimit = 16
			take = credentials
		}
		if budget := s.maxBytes.Load(); budget > 0 {
			batchLimit = int(min(int64(128), max(int64(1), (budget/20)/(256*1024))))
		}
		if first.credential {
			batchLimit = min(batchLimit, 16)
		}
		timer := time.NewTimer(time.Millisecond)
	collect:
		for take != nil && len(jobs) < batchLimit {
			select {
			case job, ok := <-take:
				if !ok {
					if first.credential {
						credentials = nil
					} else {
						urgent = nil
					}
					break collect
				}
				jobs = append(jobs, job)
			case <-timer.C:
				break collect
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		s.commitWrites(jobs)
	}
}

func (s *Store) closeStore() error {
	s.writerMu.Lock()
	if !s.writerClosing {
		s.writerClosing = true
		close(s.writes)
		close(s.urgentWrites)
	}
	s.writerMu.Unlock()
	<-s.writerDone
	if fault := s.fault.Load(); fault != nil {
		// Fatal storage faults terminate the process. Let the OS release SQLite
		// handles at exit: SQLite's normal last-close checkpoint could overwrite
		// a truncated or replaced main file with the still-open WAL contents.
		return fault.err
	}
	if err := s.reader.Close(); err != nil {
		_ = s.db.Close()
		return err
	}
	return s.db.Close()
}
