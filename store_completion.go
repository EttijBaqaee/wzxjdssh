package main

import (
	"database/sql"
	"time"
)

type connectionCompletion struct {
	ID                          int64
	IP, Version, Status, Reason string
	At                          time.Time
	Duration, Delayed           time.Duration
	Sent, Received              uint64
	Parallel, Retry, Probe      bool
	Config                      Config
}

// Queue while policyMu establishes completion order, then wait after releasing
// it. This allows simultaneous disconnects to share durable commits.
func (s *Store) enqueueCompletion(c connectionCompletion) (*storeWrite, error) {
	return s.enqueue(func(tx *sql.Tx) error {
		if c.Parallel {
			until := c.At.Add(c.Config.ParallelCooldown).UnixMilli()
			if _, err := s.exec(tx, `INSERT INTO ip_slowdowns(ip,expires_at) VALUES(?,?) ON CONFLICT(ip) DO UPDATE SET expires_at=MAX(ip_slowdowns.expires_at,excluded.expires_at)`, c.IP, until); err != nil {
				return err
			}
			if _, err := s.exec(tx, `DELETE FROM client_cooldowns WHERE ip=?`, c.IP); err != nil {
				return err
			}
			if _, err := s.exec(tx, `INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) SELECT id,'parallel_slow',0 FROM connections WHERE id=? AND ended_at IS NULL AND NOT EXISTS(SELECT 1 FROM connection_marks m WHERE m.connection_id=connections.id AND (m.label='handshake_probe' OR (m.label='parallel_slow' AND m.expires_at=0)))`, c.ID); err != nil {
				return err
			}
		}
		if c.Retry {
			until := c.At.Add(c.Config.SingleAuthRetryCooldown).UnixMilli()
			if err := s.queryRow(tx, `INSERT INTO client_cooldowns(ip,client_version,expires_at) VALUES(?,?,?) ON CONFLICT(ip,client_version) DO UPDATE SET expires_at=CASE WHEN client_cooldowns.expires_at<=? THEN excluded.expires_at ELSE client_cooldowns.expires_at END RETURNING expires_at`, c.IP, c.Version, until, c.At.UnixMilli()).Scan(&until); err != nil {
				return err
			}
			if _, err := s.exec(tx, `INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'single_auth_retry',?)`, c.ID, until); err != nil {
				return err
			}
		}
		if _, err := s.exec(tx, `UPDATE connections SET ended_at=?,duration_ms=?,status=?,reason=?,bytes_sent=?,bytes_received=?,delayed_ms=? WHERE id=?`, c.At.UnixMilli(), c.Duration.Milliseconds(), c.Status, c.Reason, c.Sent, c.Received, c.Delayed.Milliseconds(), c.ID); err != nil {
			return err
		}
		if _, err := s.exec(tx, `UPDATE auth_attempts SET result='interrupted' WHERE connection_id=? AND result='pending'`, c.ID); err != nil {
			return err
		}
		if c.Probe {
			_, err := s.probeResultTx(tx, c.ID, c.IP, c.Reason == "handshake_incomplete", c.At, c.Config)
			return err
		}
		return nil
	})
}
