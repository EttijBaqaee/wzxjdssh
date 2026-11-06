package main

import "database/sql"

func (s *Store) prepareWrites() error {
	s.statements = make(map[string]*sql.Stmt)
	for _, query := range []string{
		`DELETE FROM client_cooldowns WHERE expires_at<=?`,
		`DELETE FROM client_cooldowns WHERE ip=?`,
		`DELETE FROM connections WHERE id=? AND auth_attempts=0`,
		`DELETE FROM connections WHERE id=? AND ended_at IS NOT NULL`,
		`DELETE FROM ip_probes WHERE (expires_at>0 AND expires_at<=?) OR (expires_at=0 AND updated_at<?)`,
		`DELETE FROM ip_slowdowns WHERE expires_at<=? AND NOT EXISTS(SELECT 1 FROM connections c JOIN connection_marks m ON m.connection_id=c.id WHERE c.ip=ip_slowdowns.ip AND c.ended_at IS NULL AND m.label='parallel_slow')`,
		`INSERT INTO auth_attempts(connection_id,at,username,method,credential,encoding,public_key,fingerprint,truncated) VALUES(?,?,?,?,?,?,?,?,?)`,
		`INSERT INTO client_cooldowns(ip,client_version,expires_at) VALUES(?,?,?) ON CONFLICT(ip,client_version) DO UPDATE SET expires_at=CASE WHEN client_cooldowns.expires_at<=? THEN excluded.expires_at ELSE client_cooldowns.expires_at END RETURNING expires_at`,
		`INSERT INTO connections(peer,ip,mode,started_at,ended_at,status,reason) VALUES(?,?,?,?,?,'rejected',?)`,
		`INSERT INTO connections(peer,ip,mode,started_at,status) VALUES(?,?,?,?,'connected')`,
		`INSERT INTO ip_probes(ip,streak,expires_at,updated_at) VALUES(?,?,?,?) ON CONFLICT(ip) DO UPDATE SET streak=excluded.streak,expires_at=excluded.expires_at,updated_at=excluded.updated_at`,
		`INSERT INTO ip_slowdowns(ip,expires_at) VALUES(?,?) ON CONFLICT(ip) DO UPDATE SET expires_at=MAX(ip_slowdowns.expires_at,excluded.expires_at)`,
		`INSERT INTO simulated_sessions(connection_id,trace) VALUES(?,?) ON CONFLICT(connection_id) DO UPDATE SET trace=excluded.trace`,
		`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) SELECT id,'parallel_slow',0 FROM connections WHERE id=? AND ended_at IS NULL AND NOT EXISTS(SELECT 1 FROM connection_marks m WHERE m.connection_id=connections.id AND (m.label='handshake_probe' OR (m.label='parallel_slow' AND m.expires_at=0)))`,
		`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) SELECT id,'parallel_slow',0 FROM connections WHERE ip=? AND ended_at IS NULL AND NOT EXISTS(SELECT 1 FROM connection_marks m WHERE m.connection_id=connections.id AND (m.label='handshake_probe' OR (m.label='parallel_slow' AND m.expires_at=0)))`,
		`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'cooldown',?)`,
		`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'handshake_probe',?)`,
		`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'parallel_slow',0)`,
		`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'simulated_network',?)`,
		`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,'single_auth_retry',?)`,
		`INSERT OR REPLACE INTO connection_marks(connection_id,label,expires_at) VALUES(?,?,?)`,
		`SELECT connection_id FROM auth_attempts WHERE at<? ORDER BY at,id LIMIT 1`,
		`SELECT id FROM connections WHERE started_at<? AND ended_at IS NOT NULL ORDER BY started_at,id LIMIT 1`,
		`SELECT id,auth_attempts,ended_at IS NOT NULL FROM connections WHERE ended_at IS NOT NULL OR auth_attempts>0 ORDER BY started_at,id LIMIT 1`,
		`SELECT streak,expires_at FROM ip_probes WHERE ip=?`,
		`UPDATE auth_attempts SET result='interrupted' WHERE connection_id=? AND result='pending'`,
		`UPDATE auth_attempts SET result='simulated_success' WHERE connection_id=? AND result='pending'`,
		`UPDATE auth_attempts SET result=? WHERE id=?`,
		`UPDATE connections SET auth_attempts=MAX(0,auth_attempts-?) WHERE id=?`,
		`UPDATE connections SET auth_attempts=auth_attempts+1 WHERE id=?`,
		`UPDATE connections SET ended_at=?,duration_ms=?,status=?,reason=?,bytes_sent=?,bytes_received=?,delayed_ms=? WHERE id=?`,
		`UPDATE connections SET status='authenticating',client_version=? WHERE id=?`,
		`UPDATE connections SET status='simulated_network' WHERE id=? AND ended_at IS NULL`,
		`UPDATE connections SET username=? WHERE id=?`,
		`WITH sample AS (SELECT id,length(credential)+length(public_key)+512 AS bytes FROM auth_attempts WHERE connection_id=? AND (?=0 OR at<?) ORDER BY id LIMIT 128), bounded AS (SELECT id,SUM(bytes) OVER(ORDER BY id) AS bytes FROM sample)
 DELETE FROM auth_attempts WHERE id IN (SELECT id FROM bounded WHERE bytes<=262144 UNION SELECT MIN(id) FROM sample)`,
	} {
		statement, err := s.db.Prepare(query)
		if err != nil {
			return err
		}
		s.statements[query] = statement
	}
	return nil
}
func (s *Store) exec(tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	if statement := s.statements[query]; statement != nil {
		return tx.Stmt(statement).Exec(args...)
	}
	return tx.Exec(query, args...)
}
func (s *Store) queryRow(tx *sql.Tx, query string, args ...any) *sql.Row {
	if statement := s.statements[query]; statement != nil {
		return tx.Stmt(statement).QueryRow(args...)
	}
	return tx.QueryRow(query, args...)
}
