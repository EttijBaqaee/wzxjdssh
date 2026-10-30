package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

type Store struct {
	db              *sql.DB
	path            string
	writeMu         sync.Mutex
	maxBytes        atomic.Int64
	capacityDeleted atomic.Uint64
}
type ConnectionRecord struct {
	ID            int64         `json:"id"`
	Peer          string        `json:"peer"`
	IP            string        `json:"ip"`
	Mode          string        `json:"mode"`
	StartedAt     int64         `json:"started_at"`
	EndedAt       *int64        `json:"ended_at"`
	DurationMS    int64         `json:"duration_ms"`
	DelayedMS     int64         `json:"delayed_ms"`
	Status        string        `json:"status"`
	Reason        string        `json:"reason"`
	ClientVersion string        `json:"client_version"`
	Username      string        `json:"username"`
	BytesSent     int64         `json:"bytes_sent"`
	BytesReceived int64         `json:"bytes_received"`
	AuthAttempts  int64         `json:"auth_attempts"`
	ClientMark    string        `json:"client_mark"`
	MarkExpiresAt int64         `json:"mark_expires_at"`
	Liveness      *PeerLiveness `json:"liveness,omitempty"`
	Session       *SessionTrace `json:"session,omitempty"`
}
type AuthRecord struct {
	ID           int64  `json:"id"`
	ConnectionID int64  `json:"connection_id"`
	At           int64  `json:"at"`
	Username     string `json:"username"`
	Method       string `json:"method"`
	Credential   string `json:"credential"`
	Encoding     string `json:"encoding"`
	PublicKey    string `json:"public_key"`
	Fingerprint  string `json:"fingerprint"`
	Truncated    bool   `json:"truncated"`
	Result       string `json:"result"`
}
type RecordFilter struct {
	Page, Limit          int
	Last                 bool
	IP, Username, Status string
}
type RecordPage struct {
	Items []ConnectionRecord `json:"items"`
	Total int64              `json:"total"`
	Page  int                `json:"page"`
	Limit int                `json:"limit"`
}
type Ranking struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}
type DailyPoint struct {
	Day         string `json:"day"`
	Connections int64  `json:"connections"`
	Attempts    int64  `json:"attempts"`
}
type StoredStats struct {
	Connections  int64        `json:"connections"`
	Active       int64        `json:"active"`
	Attempts     int64        `json:"attempts"`
	UniqueIPs    int64        `json:"unique_ips"`
	Rejected     int64        `json:"rejected"`
	AuthRejected int64        `json:"auth_rejected"`
	ProbeBlocked int64        `json:"probe_blocked"`
	Today        int64        `json:"today"`
	Sources      []Ranking    `json:"sources"`
	Usernames    []Ranking    `json:"usernames"`
	Passwords    []Ranking    `json:"passwords"`
	Methods      []Ranking    `json:"methods"`
	Daily        []DailyPoint `json:"daily"`
}

func OpenStore(path string, parallelCooldown ...time.Duration) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path cannot be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// Create with restrictive permissions before SQLite opens its files.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path}
	var schemaVersion int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&schemaVersion); err != nil {
		db.Close()
		return nil, err
	}
	if schemaVersion > 3 {
		db.Close()
		return nil, errors.New("database schema is newer than this application")
	}
	schema := `PRAGMA busy_timeout=5000;
 PRAGMA foreign_keys=ON;
 PRAGMA journal_mode=WAL;
 PRAGMA wal_autocheckpoint=128;
 PRAGMA journal_size_limit=0;
 PRAGMA synchronous=FULL;
 PRAGMA secure_delete=ON;
 PRAGMA auto_vacuum=INCREMENTAL;
 CREATE TABLE IF NOT EXISTS connections (
 id INTEGER PRIMARY KEY, peer TEXT NOT NULL, ip TEXT NOT NULL, mode TEXT NOT NULL,
 started_at INTEGER NOT NULL, ended_at INTEGER, duration_ms INTEGER NOT NULL DEFAULT 0, delayed_ms INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', client_version TEXT NOT NULL DEFAULT '', username TEXT NOT NULL DEFAULT '',
 bytes_sent INTEGER NOT NULL DEFAULT 0, bytes_received INTEGER NOT NULL DEFAULT 0,
 auth_attempts INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS auth_attempts (
 id INTEGER PRIMARY KEY, connection_id INTEGER NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
 at INTEGER NOT NULL, username TEXT NOT NULL, method TEXT NOT NULL, credential TEXT NOT NULL DEFAULT '',
 encoding TEXT NOT NULL DEFAULT 'utf8', public_key TEXT NOT NULL DEFAULT '', fingerprint TEXT NOT NULL DEFAULT '',
 truncated INTEGER NOT NULL DEFAULT 0, result TEXT NOT NULL DEFAULT 'pending');
 CREATE INDEX IF NOT EXISTS connections_time ON connections(started_at DESC,id DESC);
 CREATE INDEX IF NOT EXISTS connections_ip_time ON connections(ip,started_at DESC);
 CREATE INDEX IF NOT EXISTS auth_connection ON auth_attempts(connection_id,id);
 CREATE INDEX IF NOT EXISTS auth_time ON auth_attempts(at);
 CREATE INDEX IF NOT EXISTS auth_username ON auth_attempts(username,connection_id);
	CREATE TABLE IF NOT EXISTS web_preferences (key TEXT PRIMARY KEY,value INTEGER NOT NULL);
	INSERT OR IGNORE INTO web_preferences(key,value) VALUES('idle_minutes',60);
	CREATE TABLE IF NOT EXISTS client_cooldowns (ip TEXT NOT NULL,client_version TEXT NOT NULL,expires_at INTEGER NOT NULL,PRIMARY KEY(ip,client_version));
	CREATE INDEX IF NOT EXISTS client_cooldowns_expiry ON client_cooldowns(expires_at);
	CREATE TABLE IF NOT EXISTS connection_marks (connection_id INTEGER PRIMARY KEY REFERENCES connections(id) ON DELETE CASCADE,label TEXT NOT NULL,expires_at INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS simulated_sessions (connection_id INTEGER PRIMARY KEY REFERENCES connections(id) ON DELETE CASCADE,trace TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS ip_slowdowns (ip TEXT PRIMARY KEY, expires_at INTEGER NOT NULL);
	CREATE INDEX IF NOT EXISTS ip_slowdowns_expiry ON ip_slowdowns(expires_at);
	CREATE TABLE IF NOT EXISTS ip_probes (ip TEXT PRIMARY KEY,streak INTEGER NOT NULL,expires_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
	CREATE INDEX IF NOT EXISTS ip_probes_expiry ON ip_probes(expires_at);
	`
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("database schema: %w", err)
	}
	// Inspect columns too, so an interrupted or previously partial migration is safe.
	columns := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(connections)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	for rows.Next() {
		var cid, required, primary int
		var name, kind string
		var def any
		if err = rows.Scan(&cid, &name, &kind, &required, &def, &primary); err != nil {
			rows.Close()
			db.Close()
			return nil, err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, column := range []struct{ name, definition string }{{"username", "TEXT NOT NULL DEFAULT ''"}, {"delayed_ms", "INTEGER NOT NULL DEFAULT 0"}} {
		if !columns[column.name] {
			if _, err = db.Exec(`ALTER TABLE connections ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	if _, err = db.Exec(`PRAGMA user_version=3`); err != nil {
		db.Close()
		return nil, err
	}
	// A previous process cannot still own these connections after an ordinary restart.
	now := time.Now().UnixMilli()
	cooldown := time.Hour
	if len(parallelCooldown) > 0 {
		cooldown = parallelCooldown[0]
	}
	// Crash recovery treats the lost online connections as ending now.
	if _, err = db.Exec(`UPDATE ip_slowdowns SET expires_at=MAX(expires_at,?) WHERE EXISTS(SELECT 1 FROM connections c JOIN connection_marks m ON m.connection_id=c.id WHERE c.ip=ip_slowdowns.ip AND c.ended_at IS NULL AND m.label='parallel_slow')`, now+cooldown.Milliseconds()); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`UPDATE connections SET ended_at=?,duration_ms=MAX(0,?-started_at),status='interrupted',reason='process_restart' WHERE ended_at IS NULL`, now, now); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`UPDATE auth_attempts SET result='interrupted' WHERE result='pending'`); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) WebIdleMinutes() (int, error) {
	var minutes int
	err := s.db.QueryRow(`SELECT value FROM web_preferences WHERE key='idle_minutes'`).Scan(&minutes)
	if err != nil {
		return 0, err
	}
	if minutes < 1 || minutes > 1440 {
		return 0, errors.New("invalid idle timeout preference")
	}
	return minutes, nil
}
func (s *Store) SetWebIdleMinutes(ctx context.Context, minutes int) error {
	if minutes < 1 || minutes > 1440 {
		return errors.New("idle timeout must be 1..1440 minutes")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE web_preferences SET value=? WHERE key='idle_minutes'`, minutes)
	return err
}
func (s *Store) Start(peer, ip, mode string, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	r, err := s.db.Exec(`INSERT INTO connections(peer,ip,mode,started_at,status) VALUES(?,?,?,?,'connected')`, peer, ip, mode, at.UnixMilli())
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = s.enforceBudgetLocked(); err != nil {
		// Admission failed before a handler received its record ID.
		_, _ = s.db.Exec(`DELETE FROM connections WHERE id=?`, id)
		return 0, err
	}
	return id, nil
}
func (s *Store) Reject(peer, ip, mode, reason string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO connections(peer,ip,mode,started_at,ended_at,status,reason) VALUES(?,?,?,?,?,'rejected',?)`, peer, ip, mode, now, now, reason)
	if err != nil {
		return err
	}
	return s.enforceBudgetLocked()
}
func (s *Store) Transport(id int64, client string) error {
	_, err := s.db.Exec(`UPDATE connections SET status='authenticating',client_version=? WHERE id=?`, client, id)
	return err
}
func (s *Store) Username(id int64, name string) error {
	name, _ = boundedText(name, 4096)
	_, err := s.db.Exec(`UPDATE connections SET username=? WHERE id=?`, name, id)
	return err
}
func boundedText(v string, max int) (string, bool) {
	if len(v) <= max {
		return v, false
	}
	v = v[:max]
	for !utf8.ValidString(v) && len(v) > 0 {
		v = v[:len(v)-1]
	}
	return v, true
}
func credentialBytes(v []byte) (string, string, bool) {
	truncated := len(v) > 65536
	if truncated {
		v = v[:65536]
	}
	if utf8.Valid(v) {
		return string(v), "utf8", truncated
	}
	return base64.StdEncoding.EncodeToString(v), "base64", truncated
}
func (s *Store) Attempt(a AuthRecord) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var cut bool
	a.Username, cut = boundedText(a.Username, 4096)
	a.Truncated = a.Truncated || cut
	a.PublicKey, cut = boundedText(a.PublicKey, 65536)
	a.Truncated = a.Truncated || cut
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`INSERT INTO auth_attempts(connection_id,at,username,method,credential,encoding,public_key,fingerprint,truncated) VALUES(?,?,?,?,?,?,?,?,?)`, a.ConnectionID, a.At, a.Username, a.Method, a.Credential, a.Encoding, a.PublicKey, a.Fingerprint, a.Truncated)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE connections SET auth_attempts=auth_attempts+1 WHERE id=?`, a.ConnectionID); err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, s.enforceBudgetLocked()
}
func (s *Store) AttemptResult(id int64, result string) error {
	_, err := s.db.Exec(`UPDATE auth_attempts SET result=? WHERE id=?`, result, id)
	return err
}
func (s *Store) Finish(id int64, duration time.Duration, status, reason string, sent, received uint64) error {
	return s.finish(id, duration, status, reason, sent, received, 0)
}
func (s *Store) finish(id int64, duration time.Duration, status, reason string, sent, received uint64, delayed time.Duration) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE connections SET ended_at=?,duration_ms=?,status=?,reason=?,bytes_sent=?,bytes_received=?,delayed_ms=? WHERE id=?`, time.Now().UnixMilli(), duration.Milliseconds(), status, reason, sent, received, delayed.Milliseconds(), id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE auth_attempts SET result='interrupted' WHERE connection_id=? AND result='pending'`, id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.enforceBudgetLocked()
}
func (s *Store) Prune(now time.Time, days int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE connections SET auth_attempts=(SELECT COUNT(*) FROM auth_attempts WHERE connection_id=connections.id AND at>=?) WHERE id IN (SELECT connection_id FROM auth_attempts WHERE at<?)`, cutoff, cutoff); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM auth_attempts WHERE at < ?`, cutoff); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM connections WHERE started_at < ? AND ended_at IS NOT NULL`, cutoff); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM client_cooldowns WHERE expires_at<=?`, now.UnixMilli()); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM ip_slowdowns WHERE expires_at<=? AND NOT EXISTS(SELECT 1 FROM connections c JOIN connection_marks m ON m.connection_id=c.id WHERE c.ip=ip_slowdowns.ip AND c.ended_at IS NULL AND m.label='parallel_slow')`, now.UnixMilli()); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM ip_probes WHERE (expires_at>0 AND expires_at<=?) OR (expires_at=0 AND updated_at<?)`, now.UnixMilli(), cutoff); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	_, err = s.db.Exec(`PRAGMA incremental_vacuum(128)`)
	return err
}
func (s *Store) Maintain(ctx context.Context, days int, onError func(error)) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	budgetTicker := time.NewTicker(time.Second)
	defer budgetTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-budgetTicker.C:
			if err := s.EnforceBudget(); err != nil {
				onError(err)
			}
		case now := <-ticker.C:
			if err := s.Prune(now, days); err != nil {
				onError(err)
			}
		}
	}
}

const connectionColumns = `id,peer,ip,mode,started_at,ended_at,duration_ms,delayed_ms,status,reason,client_version,username,bytes_sent,bytes_received,auth_attempts,COALESCE((SELECT label FROM connection_marks WHERE connection_id=connections.id),''),COALESCE((SELECT expires_at FROM connection_marks WHERE connection_id=connections.id),0)`

func scanConnection(row interface{ Scan(...any) error }) (ConnectionRecord, error) {
	var c ConnectionRecord
	err := row.Scan(&c.ID, &c.Peer, &c.IP, &c.Mode, &c.StartedAt, &c.EndedAt, &c.DurationMS, &c.DelayedMS, &c.Status, &c.Reason, &c.ClientVersion, &c.Username, &c.BytesSent, &c.BytesReceived, &c.AuthAttempts, &c.ClientMark, &c.MarkExpiresAt)
	if c.EndedAt == nil {
		c.DurationMS = time.Now().UnixMilli() - c.StartedAt
	}
	if c.DurationMS < 0 {
		c.DurationMS = 0
	}
	return c, err
}
func (s *Store) List(ctx context.Context, f RecordFilter) (RecordPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Limit < 1 || f.Limit > 100 {
		f.Limit = 25
	}
	out := RecordPage{Items: []ConnectionRecord{}, Page: f.Page, Limit: f.Limit}
	where, args := recordWhere(f)
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM connections WHERE `+where, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	out.Page = recordPage(f, out.Total)
	args = append(args, f.Limit, (out.Page-1)*f.Limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+connectionColumns+` FROM connections WHERE `+where+` ORDER BY started_at DESC,id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, c)
	}
	return out, rows.Err()
}
func (s *Store) Detail(ctx context.Context, id int64) (ConnectionRecord, []AuthRecord, error) {
	c, err := scanConnection(s.db.QueryRowContext(ctx, `SELECT `+connectionColumns+` FROM connections WHERE id=?`, id))
	if err != nil {
		return c, nil, err
	}
	c.Session, err = s.loadSessionTrace(ctx, id)
	if err != nil {
		return c, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,connection_id,at,username,method,credential,encoding,public_key,fingerprint,truncated,result FROM auth_attempts WHERE connection_id=? ORDER BY id`, id)
	if err != nil {
		return c, nil, err
	}
	defer rows.Close()
	attempts := []AuthRecord{}
	for rows.Next() {
		var a AuthRecord
		if err := rows.Scan(&a.ID, &a.ConnectionID, &a.At, &a.Username, &a.Method, &a.Credential, &a.Encoding, &a.PublicKey, &a.Fingerprint, &a.Truncated, &a.Result); err != nil {
			return c, nil, err
		}
		attempts = append(attempts, a)
	}
	return c, attempts, rows.Err()
}
func (s *Store) Stats(ctx context.Context) (StoredStats, error) {
	out := StoredStats{Sources: []Ranking{}, Usernames: []Ranking{}, Passwords: []Ranking{}, Methods: []Ranking{}, Daily: []DailyPoint{}}
	today := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(ended_at IS NULL),0),COUNT(DISTINCT ip),COALESCE(SUM(status='rejected'),0),COALESCE(SUM(started_at>=?),0) FROM connections`, today).Scan(&out.Connections, &out.Active, &out.UniqueIPs, &out.Rejected, &out.Today)
	if err != nil {
		return out, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_attempts`).Scan(&out.Attempts); err != nil {
		return out, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_attempts WHERE result='rejected'`).Scan(&out.AuthRejected); err != nil {
		return out, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM connections WHERE status='probe_blocked'`).Scan(&out.ProbeBlocked); err != nil {
		return out, err
	}
	for _, item := range []struct {
		query string
		dst   *[]Ranking
	}{
		{`SELECT ip,COUNT(*) AS n FROM connections GROUP BY ip ORDER BY n DESC,ip LIMIT 10`, &out.Sources},
		{`SELECT username,COUNT(*) AS n FROM auth_attempts GROUP BY username ORDER BY n DESC,username LIMIT 10`, &out.Usernames},
		{`SELECT method,COUNT(*) AS n FROM auth_attempts GROUP BY method ORDER BY n DESC,method LIMIT 10`, &out.Methods},
		{`WITH passwords AS (
		 SELECT credential AS password,encoding FROM auth_attempts WHERE method='password' AND truncated=0
		 UNION ALL
		 SELECT json_extract(CASE WHEN json_valid(credential) THEN credential ELSE 'null' END,'$[0]'),'utf8' FROM auth_attempts
		 WHERE method='keyboard-interactive' AND encoding='utf8' AND truncated=0
		 AND json_array_length(CASE WHEN json_valid(credential) THEN credential ELSE 'null' END)=1
		 AND json_type(CASE WHEN json_valid(credential) THEN credential ELSE 'null' END,'$[0]')='text'
		 ) SELECT CASE WHEN encoding='base64' THEN '[Base64] '||password ELSE password END,COUNT(*) AS n
		 FROM passwords GROUP BY password,encoding ORDER BY n DESC,password,encoding LIMIT 10`, &out.Passwords},
	} {
		rows, err := s.db.QueryContext(ctx, item.query)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var r Ranking
			if err = rows.Scan(&r.Name, &r.Count); err != nil {
				rows.Close()
				return out, err
			}
			*item.dst = append(*item.dst, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	since := time.Now().UTC().Truncate(24 * time.Hour).Add(-29 * 24 * time.Hour)
	days := map[string]*DailyPoint{}
	for i := 0; i < 30; i++ {
		day := since.Add(time.Duration(i) * 24 * time.Hour).Format("2006-01-02")
		out.Daily = append(out.Daily, DailyPoint{Day: day})
	}
	for i := range out.Daily {
		days[out.Daily[i].Day] = &out.Daily[i]
	}
	for _, table := range []string{"connections", "auth_attempts"} {
		col := "started_at"
		if table == "auth_attempts" {
			col = "at"
		}
		rows, err := s.db.QueryContext(ctx, `SELECT strftime('%Y-%m-%d',`+col+`/1000,'unixepoch'),COUNT(*) FROM `+table+` WHERE `+col+`>=? GROUP BY 1`, since.UnixMilli())
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var day string
			var n int64
			if err = rows.Scan(&day, &n); err != nil {
				rows.Close()
				return out, err
			}
			if p := days[day]; p != nil {
				if table == "connections" {
					p.Connections = n
				} else {
					p.Attempts = n
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
