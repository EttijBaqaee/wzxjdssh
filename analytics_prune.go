package main

import (
	"strings"
)

// Batch only the oldest closed prefix. A live connection with captured
// credentials remains the boundary and falls back to bounded auth pruning.
func (s *Store) pruneClosedBatch(byteLimit int64) (uint64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT c.id,c.ended_at IS NOT NULL,512+COALESCE((SELECT SUM(length(credential)+length(public_key)+256) FROM auth_attempts a WHERE a.connection_id=c.id),0)
 FROM connections c WHERE ended_at IS NOT NULL OR auth_attempts>0 ORDER BY started_at,id LIMIT 512`)
	if err != nil {
		return 0, err
	}
	ids := []any{}
	var bytes int64
	byteLimit = min(int64(4*1024*1024), max(int64(1), byteLimit))
	for rows.Next() {
		var id, size int64
		var ended bool
		if err = rows.Scan(&id, &ended, &size); err != nil {
			break
		}
		if !ended || (len(ids) > 0 && bytes+size > byteLimit) {
			break
		}
		ids = append(ids, id)
		bytes += size
		if bytes >= byteLimit {
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return 0, err
	}
	if len(ids) < 16 {
		return 0, nil
	}
	_, err = tx.Exec(`
 CREATE TEMP TABLE IF NOT EXISTS wzx_prune_ids(id INTEGER PRIMARY KEY);
 CREATE TEMP TABLE IF NOT EXISTS wzx_prune_ips(ip TEXT PRIMARY KEY);
 CREATE TEMP TABLE IF NOT EXISTS wzx_prune_days(day TEXT PRIMARY KEY,connections INTEGER NOT NULL DEFAULT 0,attempts INTEGER NOT NULL DEFAULT 0);
 CREATE TEMP TABLE IF NOT EXISTS wzx_prune_groups(kind TEXT NOT NULL,key BLOB NOT NULL,n INTEGER NOT NULL,PRIMARY KEY(kind,key));
 DELETE FROM wzx_prune_ids; DELETE FROM wzx_prune_ips; DELETE FROM wzx_prune_days; DELETE FROM wzx_prune_groups;`)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`INSERT INTO wzx_prune_ids(id) VALUES (`+strings.Repeat(`?),(`, len(ids)-1)+`?)`, ids...)
	if err != nil {
		return 0, err
	}
	var attempts int64
	if err = tx.QueryRow(`SELECT COUNT(*) FROM wzx_prune_ids p CROSS JOIN auth_attempts a ON a.connection_id=p.id`).Scan(&attempts); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`INSERT INTO wzx_prune_ips SELECT DISTINCT ip FROM wzx_prune_ids p CROSS JOIN connections c ON c.id=p.id;
 INSERT INTO wzx_prune_days(day,connections) SELECT strftime('%Y-%m-%d',started_at/1000,'unixepoch'),COUNT(*) FROM wzx_prune_ids p CROSS JOIN connections c ON c.id=p.id GROUP BY 1;
 INSERT INTO wzx_prune_days(day,attempts) SELECT strftime('%Y-%m-%d',at/1000,'unixepoch'),COUNT(*) FROM wzx_prune_ids p CROSS JOIN auth_attempts a ON a.connection_id=p.id WHERE 1 GROUP BY 1 ON CONFLICT(day) DO UPDATE SET attempts=excluded.attempts;`); err != nil {
		return 0, err
	}
	for _, item := range []struct{ kind, query string }{
		{"clients", `SELECT client_version AS name,'' AS encoding FROM wzx_prune_ids p CROSS JOIN connections c ON c.id=p.id WHERE client_version<>''`},
		{"usernames", `SELECT username AS name,'' AS encoding FROM wzx_prune_ids p CROSS JOIN auth_attempts a ON a.connection_id=p.id`},
		{"methods", `SELECT method AS name,'' AS encoding FROM wzx_prune_ids p CROSS JOIN auth_attempts a ON a.connection_id=p.id`},
		{"passwords", `SELECT ` + passwordExpression("a.") + ` AS name,CASE WHEN method='password' THEN encoding ELSE 'utf8' END AS encoding FROM wzx_prune_ids p CROSS JOIN auth_attempts a ON a.connection_id=p.id`},
	} {
		if _, err = tx.Exec(`INSERT INTO wzx_prune_groups(kind,key,n) SELECT ?,wzx_group_key(name,encoding),COUNT(*) FROM (`+item.query+`) WHERE name IS NOT NULL GROUP BY 2`, item.kind); err != nil {
			return 0, err
		}
	}
	for _, item := range []struct{ key, query string }{
		{"connections", `SELECT COUNT(*) FROM wzx_prune_ids`},
		{"rejected", `SELECT COUNT(*) FROM wzx_prune_ids p CROSS JOIN connections c ON c.id=p.id WHERE status='rejected'`},
		{"probe_blocked", `SELECT COUNT(*) FROM wzx_prune_ids p CROSS JOIN connections c ON c.id=p.id WHERE status='probe_blocked'`},
		{"delayed_ms", `SELECT COALESCE(SUM(MAX(0,delayed_ms)),0) FROM wzx_prune_ids p CROSS JOIN connections c ON c.id=p.id`},
		{"bytes_sent", `SELECT COALESCE(SUM(MAX(0,bytes_sent)),0) FROM wzx_prune_ids p CROSS JOIN connections c ON c.id=p.id`},
		{"bytes_received", `SELECT COALESCE(SUM(MAX(0,bytes_received)),0) FROM wzx_prune_ids p CROSS JOIN connections c ON c.id=p.id`},
		{"attempts", `SELECT COUNT(*) FROM wzx_prune_ids p CROSS JOIN auth_attempts a ON a.connection_id=p.id`},
		{"auth_rejected", `SELECT COUNT(*) FROM wzx_prune_ids p CROSS JOIN auth_attempts a ON a.connection_id=p.id WHERE result='rejected'`},
		{"verified_ms", `SELECT COALESCE(SUM(` + verifiedExpression("t") + `),0) FROM wzx_prune_ids p CROSS JOIN simulated_sessions t ON t.connection_id=p.id`},
	} {
		if _, err = tx.Exec(`UPDATE analytics_totals SET value=value-(`+item.query+`) WHERE key=?`, item.key); err != nil {
			return 0, err
		}
	}
	if _, err = tx.Exec(`UPDATE analytics_days SET connections=connections-(SELECT d.connections FROM wzx_prune_days d WHERE d.day=analytics_days.day),attempts=attempts-(SELECT d.attempts FROM wzx_prune_days d WHERE d.day=analytics_days.day) WHERE day IN (SELECT day FROM wzx_prune_days);
 DELETE FROM analytics_days WHERE day IN (SELECT day FROM wzx_prune_days) AND connections=0 AND attempts=0;
 UPDATE analytics_groups SET n=n-(SELECT d.n FROM wzx_prune_groups d WHERE d.kind=analytics_groups.kind AND d.key=analytics_groups.key) WHERE (kind,key) IN (SELECT kind,key FROM wzx_prune_groups);
 DELETE FROM analytics_groups WHERE (kind,key) IN (SELECT kind,key FROM wzx_prune_groups) AND n=0;`); err != nil {
		return 0, err
	}
	// Readers see either side of the commit. Trigger changes and all summaries
	// share this transaction; no partially updated state survives rollback.
	for name := range analyticsTriggers() {
		if _, err = tx.Exec(`DROP TRIGGER wzx_` + name); err != nil {
			return 0, err
		}
	}
	if _, err = tx.Exec(`DELETE FROM auth_attempts WHERE connection_id IN (SELECT id FROM wzx_prune_ids);
 DELETE FROM connections WHERE id IN (SELECT id FROM wzx_prune_ids);
 DELETE FROM analytics_ips WHERE ip IN (SELECT ip FROM wzx_prune_ips);
 INSERT INTO analytics_ips
 SELECT ip,COUNT(*),SUM(ended_at IS NULL),SUM(auth_attempts),MIN(started_at),MAX(started_at),
 (SELECT id FROM connections b WHERE b.ip=a.ip ORDER BY started_at DESC,id DESC LIMIT 1),MAX(MAX(0,duration_ms)),
 (SELECT id FROM connections b WHERE b.ip=a.ip ORDER BY duration_ms DESC,id DESC LIMIT 1),SUM(MAX(0,delayed_ms)),MAX(MAX(0,delayed_ms))
 FROM connections a WHERE ip IN (SELECT ip FROM wzx_prune_ips) GROUP BY ip;`); err != nil {
		return 0, err
	}
	for name, body := range analyticsTriggers() {
		if _, err = tx.Exec(`CREATE TRIGGER wzx_` + name + ` ` + body); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return uint64(int64(len(ids)) + attempts), nil
}
