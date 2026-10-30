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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	until := at.Add(duration).UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err = tx.QueryRow(`INSERT INTO client_cooldowns(ip,client_version,expires_at) VALUES(?,?,?) ON CONFLICT(ip,client_version) DO UPDATE SET expires_at=CASE WHEN client_cooldowns.expires_at<=? THEN excluded.expires_at ELSE client_cooldowns.expires_at END RETURNING expires_at`, ip, version, until, at.UnixMilli()).Scan(&until); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,?,?)`, id, label, until); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return until, s.enforceBudgetLocked()
}

func (s *Store) ClientCooldown(ip, version string, now time.Time) (int64, error) {
	var until int64
	err := s.db.QueryRow(`SELECT expires_at FROM client_cooldowns WHERE ip=? AND client_version=? AND expires_at>?`, ip, version, now.UnixMilli()).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return until, err
}

func (s *Store) MarkReturningClient(id, until int64) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'cooldown',?)`, id, until)
	return err
}

func (s *Store) RecordSimulatedSession(id, until int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'simulated_network',?)`, id, until); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE connections SET status='simulated_network' WHERE id=? AND ended_at IS NULL`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE auth_attempts SET result='simulated_success' WHERE connection_id=? AND result='pending'`, id); err != nil {
		return err
	}
	return tx.Commit()
}
