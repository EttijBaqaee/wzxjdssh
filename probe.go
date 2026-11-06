package main

import (
	"database/sql"
	"errors"
	"io"
	"time"
)

func (s *Store) ProbeCooldown(ip string, now time.Time) (int64, error) {
	var until int64
	err := s.reader.QueryRow(`SELECT expires_at FROM ip_probes WHERE ip=? AND expires_at>?`, ip, now.UnixMilli()).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return until, err
}

// Called in completion order per IP. Successful transport resets the streak
// immediately; other nonmatching closures also break the consecutive sequence.
func (s *Store) ProbeResult(id int64, ip string, incomplete bool, now time.Time, cfg Config) (int64, error) {
	var until int64
	err := s.write(func(tx *sql.Tx) error {
		var err error
		until, err = s.probeResultTx(tx, id, ip, incomplete, now, cfg)
		return err
	})
	return until, err
}
func (s *Store) probeResultTx(tx *sql.Tx, id int64, ip string, incomplete bool, now time.Time, cfg Config) (int64, error) {
	var until int64
	var streak int
	err := s.queryRow(tx, `SELECT streak,expires_at FROM ip_probes WHERE ip=?`, ip).Scan(&streak, &until)
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
	if _, err = s.exec(tx, `INSERT INTO ip_probes(ip,streak,expires_at,updated_at) VALUES(?,?,?,?) ON CONFLICT(ip) DO UPDATE SET streak=excluded.streak,expires_at=excluded.expires_at,updated_at=excluded.updated_at`, ip, streak, until, now.UnixMilli()); err != nil {
		return 0, err
	}
	if until > 0 && id != 0 {
		if _, err = s.exec(tx, `INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'handshake_probe',?)`, id, until); err != nil {
			return 0, err
		}
	}
	return until, nil
}

func (s *Store) MarkProbeConnection(id, until int64) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := s.exec(tx, `INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'handshake_probe',?)`, id, until)
		return err
	})
}

func (s *Server) discardProbe(c *clientConn) {
	c.releasePending()
	_ = c.Conn.SetReadDeadline(time.UnixMilli(c.probeUntil))
	// No server identification, key exchange, authentication, or other payload.
	// Bounded by the existing global/per-IP admission limits and cooldown deadline.
	_, _ = io.Copy(io.Discard, c)
}
