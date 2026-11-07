package main

import (
	"crypto/sha256"
	"database/sql/driver"
	"fmt"

	"modernc.org/sqlite"
)

func init() {
	// Bound index keys even when an attempted password is 64 KiB. The original
	// value remains in the row; hashes are never exposed as attempted passwords.
	sqlite.MustRegisterDeterministicScalarFunction("wzx_group_key", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		name, _ := args[0].(string)
		encoding, _ := args[1].(string)
		key := sha256.Sum256([]byte(encoding + "\x00" + name))
		return key[:], nil
	})
}

func passwordExpression(ref string) string {
	p := func(column string) string { return ref + column }
	valid := "CASE WHEN json_valid(" + p("credential") + ") THEN " + p("credential") + " ELSE 'null' END"
	return "CASE WHEN " + p("method") + "='password' AND " + p("truncated") + "=0 THEN " + p("credential") +
		" WHEN " + p("method") + "='keyboard-interactive' AND " + p("encoding") + "='utf8' AND " + p("truncated") + "=0" +
		" AND json_array_length(" + valid + ")=1 AND json_type(" + valid + ",'$[0]')='text' THEN json_extract(" + valid + ",'$[0]') END"
}

func groupChange(kind, name, encoding, condition string, delta int) string {
	key := "wzx_group_key(" + name + "," + encoding + ")"
	if delta > 0 {
		return "INSERT INTO analytics_groups(kind,key,name,encoding,n) SELECT '" + kind + "'," + key + "," + name + "," + encoding + ",1 WHERE " + condition +
			" ON CONFLICT(kind,key) DO UPDATE SET n=n+1;\n"
	}
	return "UPDATE analytics_groups SET n=n-1 WHERE kind='" + kind + "' AND key=" + key + " AND " + condition + ";\n" +
		"DELETE FROM analytics_groups WHERE kind='" + kind + "' AND key=" + key + " AND n=0;\n"
}

func totalChange(key, expression string) string {
	return "UPDATE analytics_totals SET value=value+(" + expression + ") WHERE key='" + key + "' AND (" + expression + ")<>0;\n"
}

func dayChange(ref, column, field string, delta int) string {
	day := "strftime('%Y-%m-%d'," + ref + "." + column + "/1000,'unixepoch')"
	if delta > 0 {
		return "INSERT INTO analytics_days(day," + field + ") VALUES(" + day + ",1) ON CONFLICT(day) DO UPDATE SET " + field + "=" + field + "+1;\n"
	}
	return "UPDATE analytics_days SET " + field + "=" + field + "-1 WHERE day=" + day + "; DELETE FROM analytics_days WHERE day=" + day + " AND connections=0 AND attempts=0;\n"
}

func connectionInsert(ref string) string {
	return totalChange("connections", "1") + totalChange("active", ref+".ended_at IS NULL") +
		totalChange("rejected", ref+".status='rejected'") + totalChange("probe_blocked", ref+".status='probe_blocked'") +
		totalChange("delayed_ms", "MAX(0,"+ref+".delayed_ms)") +
		totalChange("bytes_sent", "MAX(0,"+ref+".bytes_sent)") + totalChange("bytes_received", "MAX(0,"+ref+".bytes_received)") +
		dayChange(ref, "started_at", "connections", 1) +
		groupChange("clients", ref+".client_version", "''", ref+".client_version<>''", 1) +
		`INSERT INTO analytics_ips(ip,connections,active,attempts,first_seen,last_seen,latest_id,longest_ms,longest_id,delayed_ms,max_delayed_ms)
 VALUES(` + ref + `.ip,1,` + ref + `.ended_at IS NULL,` + ref + `.auth_attempts,` + ref + `.started_at,` + ref + `.started_at,` + ref + `.id,MAX(0,` + ref + `.duration_ms),` + ref + `.id,MAX(0,` + ref + `.delayed_ms),MAX(0,` + ref + `.delayed_ms))
 ON CONFLICT(ip) DO UPDATE SET connections=connections+1,active=active+excluded.active,attempts=attempts+excluded.attempts,
 first_seen=MIN(first_seen,excluded.first_seen),
 latest_id=CASE WHEN excluded.last_seen>last_seen OR (excluded.last_seen=last_seen AND excluded.latest_id>latest_id) THEN excluded.latest_id ELSE latest_id END,
 last_seen=MAX(last_seen,excluded.last_seen),
 longest_id=CASE WHEN excluded.longest_ms>longest_ms OR (excluded.longest_ms=longest_ms AND excluded.longest_id>longest_id) THEN excluded.longest_id ELSE longest_id END,
 longest_ms=MAX(longest_ms,excluded.longest_ms),delayed_ms=delayed_ms+excluded.delayed_ms,max_delayed_ms=MAX(max_delayed_ms,excluded.max_delayed_ms);` + "\n"
}

func connectionDelete(ref string) string {
	return totalChange("connections", "-1") + totalChange("active", "-("+ref+".ended_at IS NULL)") +
		totalChange("rejected", "-("+ref+".status='rejected')") + totalChange("probe_blocked", "-("+ref+".status='probe_blocked')") +
		totalChange("delayed_ms", "-MAX(0,"+ref+".delayed_ms)") +
		totalChange("bytes_sent", "-MAX(0,"+ref+".bytes_sent)") + totalChange("bytes_received", "-MAX(0,"+ref+".bytes_received)") +
		dayChange(ref, "started_at", "connections", -1) +
		groupChange("clients", ref+".client_version", "''", ref+".client_version<>''", -1) +
		`UPDATE analytics_ips SET connections=connections-1,active=active-(` + ref + `.ended_at IS NULL),attempts=attempts-` + ref + `.auth_attempts,delayed_ms=delayed_ms-MAX(0,` + ref + `.delayed_ms) WHERE ip=` + ref + `.ip;
 DELETE FROM analytics_ips WHERE ip=` + ref + `.ip AND connections=0;
 UPDATE analytics_ips SET
 first_seen=CASE WHEN first_seen=` + ref + `.started_at THEN (SELECT MIN(started_at) FROM connections WHERE ip=` + ref + `.ip) ELSE first_seen END,
 last_seen=CASE WHEN latest_id=` + ref + `.id THEN (SELECT MAX(started_at) FROM connections WHERE ip=` + ref + `.ip) ELSE last_seen END,
 latest_id=CASE WHEN latest_id=` + ref + `.id THEN (SELECT id FROM connections WHERE ip=` + ref + `.ip ORDER BY started_at DESC,id DESC LIMIT 1) ELSE latest_id END,
 longest_ms=CASE WHEN longest_id=` + ref + `.id THEN (SELECT MAX(0,duration_ms) FROM connections WHERE ip=` + ref + `.ip ORDER BY duration_ms DESC,id DESC LIMIT 1) ELSE longest_ms END,
 longest_id=CASE WHEN longest_id=` + ref + `.id THEN (SELECT id FROM connections WHERE ip=` + ref + `.ip ORDER BY duration_ms DESC,id DESC LIMIT 1) ELSE longest_id END,
 max_delayed_ms=CASE WHEN max_delayed_ms=MAX(0,` + ref + `.delayed_ms) THEN (SELECT MAX(0,delayed_ms) FROM connections WHERE ip=` + ref + `.ip ORDER BY delayed_ms DESC,id DESC LIMIT 1) ELSE max_delayed_ms END
 WHERE ip=` + ref + `.ip;` + "\n"
}

func authGroups(ref string, delta int) string {
	password := passwordExpression(ref + ".")
	encoding := "CASE WHEN " + ref + ".method='password' THEN " + ref + ".encoding ELSE 'utf8' END"
	return groupChange("usernames", ref+".username", "''", "1", delta) +
		groupChange("methods", ref+".method", "''", "1", delta) +
		groupChange("passwords", password, encoding, "("+password+") IS NOT NULL", delta)
}

func verifiedExpression(ref string) string {
	return "CASE WHEN json_extract(" + ref + ".trace,'$.verification_version')=1 THEN MAX(0,COALESCE(json_extract(" + ref + ".trace,'$.verified_post_auth_ms'),0)) ELSE 0 END"
}

// Summaries are durable and updated by the same transaction as their records.
// Request paths never rebuild them or scan all captured credentials.
func (s *Store) initAnalytics() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 4 {
		if _, err := s.db.Exec(statusCoverIndex); err != nil {
			return err
		}
		return s.initTrafficAnalytics()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	schema := `
 CREATE INDEX IF NOT EXISTS connections_active_ip ON connections(ip,started_at,id) WHERE ended_at IS NULL;
 CREATE INDEX IF NOT EXISTS connections_ip_duration ON connections(ip,duration_ms DESC,id DESC);
 CREATE INDEX IF NOT EXISTS connections_ip_delay ON connections(ip,delayed_ms DESC,id DESC);
 CREATE INDEX IF NOT EXISTS connections_username ON connections(username,id);
 CREATE INDEX IF NOT EXISTS connections_status ON connections(status,started_at DESC,id DESC);
 CREATE TABLE analytics_totals(key TEXT PRIMARY KEY,value INTEGER NOT NULL);
 CREATE TABLE analytics_days(day TEXT PRIMARY KEY,connections INTEGER NOT NULL DEFAULT 0,attempts INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE analytics_groups(id INTEGER PRIMARY KEY,kind TEXT NOT NULL,key BLOB NOT NULL,name TEXT NOT NULL,encoding TEXT NOT NULL,n INTEGER NOT NULL,UNIQUE(kind,key));
 CREATE INDEX analytics_groups_rank ON analytics_groups(kind,n DESC,id);
 CREATE TABLE analytics_ips(ip TEXT PRIMARY KEY,connections INTEGER NOT NULL,active INTEGER NOT NULL,attempts INTEGER NOT NULL,first_seen INTEGER NOT NULL,last_seen INTEGER NOT NULL,latest_id INTEGER NOT NULL,longest_ms INTEGER NOT NULL,longest_id INTEGER NOT NULL,delayed_ms INTEGER NOT NULL,max_delayed_ms INTEGER NOT NULL);
 CREATE INDEX analytics_ips_recent ON analytics_ips(last_seen DESC,latest_id DESC,ip);
 CREATE INDEX analytics_ips_count ON analytics_ips(connections DESC,ip);
 CREATE INDEX analytics_ips_longest ON analytics_ips(longest_ms DESC,delayed_ms DESC,ip);
 INSERT INTO analytics_totals SELECT 'connections',COUNT(*) FROM connections;
 INSERT INTO analytics_totals SELECT 'active',COUNT(*) FROM connections WHERE ended_at IS NULL;
 INSERT INTO analytics_totals SELECT 'rejected',COUNT(*) FROM connections WHERE status='rejected';
 INSERT INTO analytics_totals SELECT 'probe_blocked',COUNT(*) FROM connections WHERE status='probe_blocked';
 INSERT INTO analytics_totals SELECT 'delayed_ms',COALESCE(SUM(MAX(0,delayed_ms)),0) FROM connections;
 INSERT INTO analytics_totals SELECT 'attempts',COUNT(*) FROM auth_attempts;
 INSERT INTO analytics_totals SELECT 'auth_rejected',COUNT(*) FROM auth_attempts WHERE result='rejected';
 INSERT INTO analytics_totals SELECT 'verified_ms',COALESCE(SUM(CASE WHEN json_extract(trace,'$.verification_version')=1 THEN MAX(0,COALESCE(json_extract(trace,'$.verified_post_auth_ms'),0)) ELSE 0 END),0) FROM simulated_sessions;
 INSERT INTO analytics_days(day,connections) SELECT strftime('%Y-%m-%d',started_at/1000,'unixepoch'),COUNT(*) FROM connections GROUP BY 1;
 INSERT INTO analytics_days(day,attempts) SELECT strftime('%Y-%m-%d',at/1000,'unixepoch'),COUNT(*) FROM auth_attempts WHERE 1 GROUP BY 1 ON CONFLICT(day) DO UPDATE SET attempts=excluded.attempts;
 INSERT INTO analytics_ips
 SELECT ip,COUNT(*),SUM(ended_at IS NULL),SUM(auth_attempts),MIN(started_at),MAX(started_at),
 (SELECT id FROM connections b WHERE b.ip=a.ip ORDER BY started_at DESC,id DESC LIMIT 1),MAX(MAX(0,duration_ms)),
 (SELECT id FROM connections b WHERE b.ip=a.ip ORDER BY duration_ms DESC,id DESC LIMIT 1),SUM(MAX(0,delayed_ms)),MAX(MAX(0,delayed_ms)) FROM connections a GROUP BY ip;
 `
	schema += statusCoverIndex + ";"
	if _, err = tx.Exec(schema); err != nil {
		return fmt.Errorf("analytics schema: %w", err)
	}
	for _, group := range []struct{ kind, query string }{
		{"clients", `SELECT client_version AS name,'' AS encoding,COUNT(*) AS n FROM connections WHERE client_version<>'' GROUP BY client_version`},
		{"usernames", `SELECT username AS name,'' AS encoding,COUNT(*) AS n FROM auth_attempts GROUP BY username`},
		{"methods", `SELECT method AS name,'' AS encoding,COUNT(*) AS n FROM auth_attempts GROUP BY method`},
		{"passwords", `SELECT name,encoding,COUNT(*) AS n FROM (SELECT ` + passwordExpression("") + ` AS name,CASE WHEN method='password' THEN encoding ELSE 'utf8' END AS encoding FROM auth_attempts) WHERE name IS NOT NULL GROUP BY name,encoding`},
	} {
		if _, err = tx.Exec(`INSERT INTO analytics_groups(kind,key,name,encoding,n) SELECT ?,wzx_group_key(name,encoding),name,encoding,n FROM (`+group.query+`)`, group.kind); err != nil {
			return fmt.Errorf("analytics %s: %w", group.kind, err)
		}
	}

	for name, body := range analyticsTriggers() {
		if _, err = tx.Exec("CREATE TRIGGER wzx_" + name + " " + body); err != nil {
			return fmt.Errorf("analytics trigger %s: %w", name, err)
		}
	}
	if _, err = tx.Exec(`PRAGMA user_version=4`); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.initTrafficAnalytics()
}

func analyticsTriggers() map[string]string {
	return map[string]string{
		"connections_ai":   "AFTER INSERT ON connections BEGIN\n" + connectionInsert("NEW") + "END",
		"connections_ad":   "AFTER DELETE ON connections BEGIN\n" + connectionDelete("OLD") + "END",
		"connections_move": "AFTER UPDATE OF ip,started_at ON connections WHEN NEW.ip<>OLD.ip OR NEW.started_at<>OLD.started_at BEGIN\n" + connectionDelete("OLD") + connectionInsert("NEW") + "END",
		"connections_au": `AFTER UPDATE OF status,ended_at,auth_attempts,duration_ms,delayed_ms,client_version,bytes_sent,bytes_received ON connections WHEN NEW.ip=OLD.ip AND NEW.started_at=OLD.started_at BEGIN
 ` + totalChange("active", "(NEW.ended_at IS NULL)-(OLD.ended_at IS NULL)") +
			totalChange("rejected", "(NEW.status='rejected')-(OLD.status='rejected')") +
			totalChange("probe_blocked", "(NEW.status='probe_blocked')-(OLD.status='probe_blocked')") +
			totalChange("delayed_ms", "MAX(0,NEW.delayed_ms)-MAX(0,OLD.delayed_ms)") +
			totalChange("bytes_sent", "MAX(0,NEW.bytes_sent)-MAX(0,OLD.bytes_sent)") +
			totalChange("bytes_received", "MAX(0,NEW.bytes_received)-MAX(0,OLD.bytes_received)") +
			`UPDATE analytics_ips SET active=active+(NEW.ended_at IS NULL)-(OLD.ended_at IS NULL),attempts=attempts+NEW.auth_attempts-OLD.auth_attempts,delayed_ms=delayed_ms+MAX(0,NEW.delayed_ms)-MAX(0,OLD.delayed_ms) WHERE ip=NEW.ip AND (NEW.ended_at IS NOT OLD.ended_at OR NEW.auth_attempts<>OLD.auth_attempts OR NEW.delayed_ms<>OLD.delayed_ms);
 UPDATE analytics_ips SET longest_ms=(SELECT MAX(0,duration_ms) FROM connections WHERE ip=NEW.ip ORDER BY duration_ms DESC,id DESC LIMIT 1),longest_id=(SELECT id FROM connections WHERE ip=NEW.ip ORDER BY duration_ms DESC,id DESC LIMIT 1) WHERE ip=NEW.ip AND NEW.duration_ms<>OLD.duration_ms;
 UPDATE analytics_ips SET max_delayed_ms=(SELECT MAX(0,delayed_ms) FROM connections WHERE ip=NEW.ip ORDER BY delayed_ms DESC,id DESC LIMIT 1) WHERE ip=NEW.ip AND NEW.delayed_ms<>OLD.delayed_ms;
 END`,
		"connections_version": "AFTER UPDATE OF client_version ON connections WHEN NEW.client_version<>OLD.client_version AND NEW.ip=OLD.ip AND NEW.started_at=OLD.started_at BEGIN\n" + groupChange("clients", "OLD.client_version", "''", "OLD.client_version<>''", -1) + groupChange("clients", "NEW.client_version", "''", "NEW.client_version<>''", 1) + "END",
		"auth_ai":             "AFTER INSERT ON auth_attempts BEGIN\n" + totalChange("attempts", "1") + totalChange("auth_rejected", "NEW.result='rejected'") + dayChange("NEW", "at", "attempts", 1) + authGroups("NEW", 1) + "END",
		"auth_ad":             "AFTER DELETE ON auth_attempts BEGIN\n" + totalChange("attempts", "-1") + totalChange("auth_rejected", "-(OLD.result='rejected')") + dayChange("OLD", "at", "attempts", -1) + authGroups("OLD", -1) + "END",
		"auth_result":         "AFTER UPDATE OF result ON auth_attempts WHEN NEW.result<>OLD.result BEGIN\n" + totalChange("auth_rejected", "(NEW.result='rejected')-(OLD.result='rejected')") + "END",
		"auth_fields":         "AFTER UPDATE OF username,method,credential,encoding,truncated,at ON auth_attempts BEGIN\n" + dayChange("OLD", "at", "attempts", -1) + dayChange("NEW", "at", "attempts", 1) + authGroups("OLD", -1) + authGroups("NEW", 1) + "END",
		"sessions_ai":         "AFTER INSERT ON simulated_sessions BEGIN\n" + totalChange("verified_ms", verifiedExpression("NEW")) + "END",
		"sessions_ad":         "AFTER DELETE ON simulated_sessions BEGIN\n" + totalChange("verified_ms", "-("+verifiedExpression("OLD")+")") + "END",
		"sessions_au":         "AFTER UPDATE OF trace ON simulated_sessions BEGIN\n" + totalChange("verified_ms", "("+verifiedExpression("NEW")+")-("+verifiedExpression("OLD")+")") + "END",
	}
}

const statusCoverIndex = `CREATE INDEX IF NOT EXISTS connections_status_cover ON connections(status,ip,started_at DESC,id DESC,ended_at,auth_attempts,duration_ms)`
