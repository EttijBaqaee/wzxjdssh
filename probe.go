package main

import (
	"database/sql"
	"errors"
	"io"
	"time"
)

func (s *Store) ProbeCooldown(ip string, now time.Time) (int64, error) {
	var until int64
	err := s.db.QueryRow(`SELECT expires_at FROM ip_probes WHERE ip=? AND expires_at>?`, ip, now.UnixMilli()).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return until, err
}

// Called in completion order per IP. Successful transport resets the streak
// immediately; other nonmatching closures also break the consecutive sequence.
func (s *Store) ProbeResult(id int64, ip string, incomplete bool, now time.Time, cfg Config) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var streak int
	var until int64
	err = tx.QueryRow(`SELECT streak,expires_at FROM ip_probes WHERE ip=?`, ip).Scan(&streak, &until)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if until > now.UnixMilli() {
		return until, nil
	}
	if until > 0 {
		streak = 0
	}
	until = 0
	if incomplete {
		streak++
	} else {
		streak = 0
	}
	if streak >= cfg.ProbeFailureThreshold {
		until = now.Add(cfg.ProbeCooldown).UnixMilli()
		streak = 0
	}
	if _, err = tx.Exec(`INSERT INTO ip_probes(ip,streak,expires_at,updated_at) VALUES(?,?,?,?) ON CONFLICT(ip) DO UPDATE SET streak=excluded.streak,expires_at=excluded.expires_at,updated_at=excluded.updated_at`, ip, streak, until, now.UnixMilli()); err != nil {
		return 0, err
	}
	if until > 0 && id != 0 {
		if _, err = tx.Exec(`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'handshake_probe',?)`, id, until); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return until, s.enforceBudgetLocked()
}

func (s *Store) MarkProbeConnection(id, until int64) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'handshake_probe',?)`, id, until)
	return err
}

func (s *Server) discardProbe(c *clientConn) {
	c.releasePending()
	_ = c.Conn.SetReadDeadline(time.UnixMilli(c.probeUntil))
	// No server identification, key exchange, authentication, or other payload.
	// Bounded by the existing global/per-IP admission limits and cooldown deadline.
	_, _ = io.Copy(io.Discard, c)
}
