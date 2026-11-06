package main

import (
	"database/sql"
	"errors"
	"time"
)

const clientCooldown = time.Hour

// Expiry is based on the interruption, not subsequent visits.
func (s *Store) MarkInterruptedClient(id int64, ip, version string, at time.Time) (int64, error) {
	return s.markClientCooldown(id, ip, version, "auth_interrupted", at, clientCooldown)
}

func (s *Store) markClientCooldown(id int64, ip, version, label string, at time.Time, duration time.Duration) (int64, error) {
	until := at.Add(duration).UnixMilli()
	err := s.write(func(tx *sql.Tx) error {
		if err := s.queryRow(tx, `INSERT INTO client_cooldowns(ip,client_version,expires_at) VALUES(?,?,?) ON CONFLICT(ip,client_version) DO UPDATE SET expires_at=CASE WHEN client_cooldowns.expires_at<=? THEN excluded.expires_at ELSE client_cooldowns.expires_at END RETURNING expires_at`, ip, version, until, at.UnixMilli()).Scan(&until); err != nil {
			return err
		}
		_, err := s.exec(tx, `INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,?,?)`, id, label, until)
		return err
	})
	return until, err
}

func (s *Store) ClientCooldown(ip, version string, now time.Time) (int64, error) {
	var until int64
	err := s.reader.QueryRow(`SELECT expires_at FROM client_cooldowns WHERE ip=? AND client_version=? AND expires_at>?`, ip, version, now.UnixMilli()).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return until, err
}

func (s *Store) MarkReturningClient(id, until int64) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := s.exec(tx, `INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'cooldown',?)`, id, until)
		return err
	})
}
func (s *Store) RecordSimulatedSession(id, until int64) error {
	return s.write(func(tx *sql.Tx) error {
		if _, err := s.exec(tx, `INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'simulated_network',?)`, id, until); err != nil {
			return err
		}
		if _, err := s.exec(tx, `UPDATE connections SET status='simulated_network' WHERE id=? AND ended_at IS NULL`, id); err != nil {
			return err
		}
		_, err := s.exec(tx, `UPDATE auth_attempts SET result='simulated_success' WHERE connection_id=? AND result='pending'`, id)
		return err
	})
}
