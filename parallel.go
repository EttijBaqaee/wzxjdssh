package main

import (
	"database/sql"
	"errors"
	"time"
)

// Called after admission with server.mu held. Source ports only distinguish
// overlapping TCP connections; policy identity is the normalized source IP.
func (s *Server) detectParallelLocked(c *clientConn) {
	if c.cfg.Mode != "ssh" {
		return
	}
	count, inherited := 0, false
	for other := range s.clients {
		if other.ip != c.ip || other.ctx.Err() != nil {
			continue
		}
		if c.started.Sub(other.started) <= c.cfg.ParallelWindow {
			count++
		}
		inherited = inherited || other.forceSlow.Load()
	}
	if count < c.cfg.ParallelConnections && !inherited {
		return
	}
	for other := range s.clients {
		if other.ip == c.ip {
			other.forceSlow.Store(true)
		}
	}
}

func (s *Server) applyParallelPolicy(c *clientConn) error {
	if s.store == nil || c.cfg.Mode != "ssh" {
		return nil
	}
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	until, err := s.store.ParallelUntil(c.ip, time.Now())
	if err != nil {
		return err
	}
	if until > 0 {
		c.forceSlow.Store(true)
	}
	if !c.forceSlow.Load() {
		return nil
	}
	return s.store.ForceParallelIP(c.ip, time.Now().Add(c.cfg.ParallelWindow).UnixMilli())
}

func (s *Store) ParallelUntil(ip string, now time.Time) (int64, error) {
	var until int64
	err := s.db.QueryRow(`SELECT expires_at FROM ip_slowdowns WHERE ip=? AND expires_at>?`, ip, now.UnixMilli()).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return until, err
}

func (s *Store) ForceParallelIP(ip string, until int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO ip_slowdowns(ip,expires_at) VALUES(?,?) ON CONFLICT(ip) DO UPDATE SET expires_at=MAX(ip_slowdowns.expires_at,excluded.expires_at)`, ip, until); err != nil {
		return err
	}
	// Parallel scanners must never inherit the single-client timeout shortcut.
	if _, err = tx.Exec(`DELETE FROM client_cooldowns WHERE ip=?`, ip); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) SELECT id,'parallel_slow',0 FROM connections WHERE ip=? AND ended_at IS NULL`, ip); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.enforceBudgetLocked()
}

func (s *Store) MarkParallelConnection(id int64) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'parallel_slow',0)`, id)
	return err
}
