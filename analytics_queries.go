package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"time"
)

func (s *Store) storedStats(ctx context.Context) (StoredStats, error) {
	out := StoredStats{Sources: []Ranking{}, Usernames: []Ranking{}, Passwords: []Ranking{}, Methods: []Ranking{}, ClientVersions: []Ranking{}, Daily: []DailyPoint{}}
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM analytics_totals`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var key string
		var value int64
		if err = rows.Scan(&key, &value); err != nil {
			rows.Close()
			return out, err
		}
		switch key {
		case "connections":
			out.Connections = value
		case "attempts":
			out.Attempts = value
		case "active":
			out.Active = value
		case "rejected":
			out.Rejected = value
		case "auth_rejected":
			out.AuthRejected = value
		case "probe_blocked":
			out.ProbeBlocked = value
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_ips`).Scan(&out.UniqueIPs); err != nil {
		return out, err
	}
	for _, group := range []struct {
		kind string
		dst  *[]Ranking
	}{{"sources", &out.Sources}, {"usernames", &out.Usernames}, {"passwords", &out.Passwords}, {"methods", &out.Methods}, {"clients", &out.ClientVersions}} {
		query := `SELECT CASE WHEN encoding='base64' THEN '[Base64] '||name ELSE name END,n FROM analytics_groups WHERE kind=? ORDER BY n DESC,id LIMIT 10`
		var args = []any{group.kind}
		if group.kind == "sources" {
			query = `SELECT ip,connections FROM analytics_ips ORDER BY connections DESC,ip LIMIT 10`
			args = nil
		}
		rows, err = tx.QueryContext(ctx, query, args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var r Ranking
			if err = rows.Scan(&r.Name, &r.Count); err != nil {
				rows.Close()
				return out, err
			}
			*group.dst = append(*group.dst, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	since := time.Now().UTC().Truncate(24 * time.Hour).Add(-29 * 24 * time.Hour)
	rows, err = tx.QueryContext(ctx, `SELECT day,connections,attempts FROM analytics_days WHERE day>=? ORDER BY day`, since.Format("2006-01-02"))
	if err != nil {
		return out, err
	}
	days := map[string]DailyPoint{}
	for rows.Next() {
		var point DailyPoint
		if err = rows.Scan(&point.Day, &point.Connections, &point.Attempts); err != nil {
			rows.Close()
			return out, err
		}
		days[point.Day] = point
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for i := 0; i < 30; i++ {
		day := since.Add(time.Duration(i) * 24 * time.Hour).Format("2006-01-02")
		point := days[day]
		point.Day = day
		out.Daily = append(out.Daily, point)
	}
	out.Today = out.Daily[29].Connections
	return out, tx.Commit()
}

func (s *Store) fastIPs(ctx context.Context, f RecordFilter) (IPRecordPage, error) {
	if f.Limit < 1 || f.Limit > 100 {
		f.Limit = 25
	}
	out := IPRecordPage{Items: []IPRecord{}, Limit: f.Limit}
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	where, args := "1=1", []any{}
	if f.IP != "" {
		where = "g.ip=?"
		args = append(args, f.IP)
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_ips g WHERE `+where, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	out.Page = recordPage(f, out.Total)
	values := append([]any{time.Now().UnixMilli()}, args...)
	values = append(values, f.Limit, (out.Page-1)*f.Limit)
	rows, err := tx.QueryContext(ctx, `SELECT g.ip,g.connections,g.active,g.attempts,g.first_seen,g.last_seen,
 MAX(g.longest_ms,COALESCE(?-(SELECT MIN(started_at) FROM connections a WHERE a.ip=g.ip AND a.ended_at IS NULL),0)),
 g.latest_id,c.status,COALESCE(m.label,'') FROM analytics_ips g JOIN connections c ON c.id=g.latest_id LEFT JOIN connection_marks m ON m.connection_id=g.latest_id
 WHERE `+where+` ORDER BY g.last_seen DESC,g.latest_id DESC,g.ip LIMIT ? OFFSET ?`, values...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var item IPRecord
		if err = rows.Scan(&item.IP, &item.Connections, &item.Active, &item.AuthAttempts, &item.FirstSeen, &item.LastSeen, &item.LongestMS, &item.LatestID, &item.LatestStatus, &item.ClientMark); err != nil {
			rows.Close()
			return out, err
		}
		out.Items = append(out.Items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func sampleJSON(active []liveDelay) string {
	data, _ := json.Marshal(active)
	if active == nil {
		return "[]"
	}
	return string(data)
}

func (s *Store) overlayTotal(ctx context.Context, key string, active []liveDelay) (int64, error) {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var total int64
	if err = tx.QueryRowContext(ctx, `SELECT value FROM analytics_totals WHERE key=?`, key).Scan(&total); err != nil {
		return 0, err
	}
	if len(active) > 0 {
		query := `WITH live AS (SELECT CAST(json_extract(value,'$.id') AS INTEGER) AS id,MAX(MAX(0,CAST(json_extract(value,'$.delayed_ms') AS INTEGER))) AS value FROM json_each(?) GROUP BY 1)
 SELECT COALESCE(SUM(MAX(0,l.value-MAX(0,c.delayed_ms))),0) FROM live l JOIN connections c ON c.id=l.id`
		if key == "verified_ms" {
			query = `WITH live AS (SELECT CAST(json_extract(value,'$.id') AS INTEGER) AS id,MAX(MAX(0,COALESCE(CAST(json_extract(value,'$.verified_post_auth_ms') AS INTEGER),0))) AS value FROM json_each(?) GROUP BY 1)
 SELECT COALESCE(SUM(MAX(0,l.value-CASE WHEN json_extract(t.trace,'$.verification_version')=1 THEN MAX(0,COALESCE(json_extract(t.trace,'$.verified_post_auth_ms'),0)) ELSE 0 END)),0)
 FROM live l JOIN connections c ON c.id=l.id LEFT JOIN simulated_sessions t ON t.connection_id=l.id`
		}
		var delta int64
		if err = tx.QueryRowContext(ctx, query, sampleJSON(active)).Scan(&delta); err != nil {
			return 0, err
		}
		total += delta
	}
	return total, tx.Commit()
}

type delayRecord struct {
	id                int64
	ip                string
	started           int64
	ended             sql.NullInt64
	duration, delayed int64
}

func (s *Store) fastDelayRanking(ctx context.Context, active []liveDelay) ([]IPDelay, error) {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	candidates := map[string]*IPDelay{}
	rows, err := tx.QueryContext(ctx, `SELECT ip,delayed_ms,longest_ms,longest_id,max_delayed_ms FROM analytics_ips WHERE delayed_ms>0 ORDER BY longest_ms DESC,delayed_ms DESC,ip LIMIT 10`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var item IPDelay
		if err = rows.Scan(&item.IP, &item.DelayedMS, &item.LongestMS, &item.LongestConnectionID, &item.MaxDelayedMS); err != nil {
			rows.Close()
			return nil, err
		}
		candidates[item.IP] = &item
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	records := map[int64]delayRecord{}
	for _, query := range []struct {
		text string
		args []any
	}{
		{`SELECT id,ip,started_at,ended_at,duration_ms,delayed_ms FROM connections WHERE ended_at IS NULL`, nil},
		{`SELECT c.id,c.ip,c.started_at,c.ended_at,c.duration_ms,c.delayed_ms FROM json_each(?) j JOIN connections c ON c.id=CAST(json_extract(j.value,'$.id') AS INTEGER)`, []any{sampleJSON(active)}},
	} {
		rows, err = tx.QueryContext(ctx, query.text, query.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var record delayRecord
			if err = rows.Scan(&record.id, &record.ip, &record.started, &record.ended, &record.duration, &record.delayed); err != nil {
				rows.Close()
				return nil, err
			}
			records[record.id] = record
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	missing := map[string]bool{}
	for _, record := range records {
		if candidates[record.ip] == nil {
			missing[record.ip] = true
		}
	}
	if len(missing) > 0 {
		ips := make([]string, 0, len(missing))
		for ip := range missing {
			ips = append(ips, ip)
		}
		data, _ := json.Marshal(ips)
		rows, err = tx.QueryContext(ctx, `SELECT a.ip,a.delayed_ms,a.longest_ms,a.longest_id,a.max_delayed_ms FROM json_each(?) j JOIN analytics_ips a ON a.ip=j.value`, string(data))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var item IPDelay
			if err = rows.Scan(&item.IP, &item.DelayedMS, &item.LongestMS, &item.LongestConnectionID, &item.MaxDelayedMS); err != nil {
				rows.Close()
				return nil, err
			}
			candidates[item.IP] = &item
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	live := map[int64]liveDelay{}
	for _, sample := range active {
		old := live[sample.ID]
		sample.DelayedMS = max(sample.DelayedMS, old.DelayedMS)
		sample.DurationMS = max(sample.DurationMS, old.DurationMS)
		live[sample.ID] = sample
	}
	now := time.Now().UnixMilli()
	for _, record := range records {
		item := candidates[record.ip]
		if item == nil {
			continue
		}
		sample := live[record.id]
		item.DelayedMS += max(int64(0), sample.DelayedMS-max(int64(0), record.delayed))
		item.MaxDelayedMS = max(item.MaxDelayedMS, sample.DelayedMS)
		duration := record.duration
		if !record.ended.Valid {
			duration = sample.DurationMS
			if duration <= 0 {
				duration = max(int64(0), now-record.started)
			}
		}
		if duration > item.LongestMS || (duration == item.LongestMS && record.id > item.LongestConnectionID) {
			item.LongestMS = duration
			item.LongestConnectionID = record.id
		}
	}
	out := []IPDelay{}
	for _, item := range candidates {
		if item.DelayedMS > 0 {
			out = append(out, *item)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.LongestMS != b.LongestMS {
			return a.LongestMS > b.LongestMS
		}
		if a.DelayedMS != b.DelayedMS {
			return a.DelayedMS > b.DelayedMS
		}
		return a.IP < b.IP
	})
	if len(out) > 10 {
		out = out[:10]
	}
	return out, tx.Commit()
}

// Read the covering index to select IPs, then aggregate only the visible page.
// This avoids reading every full connection row for a common status filter.
func (s *Store) statusIPs(ctx context.Context, f RecordFilter) (IPRecordPage, error) {
	if f.Limit < 1 || f.Limit > 100 {
		f.Limit = 25
	}
	out := IPRecordPage{Items: []IPRecord{}, Limit: f.Limit}
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	where, args := "status=?", []any{f.Status}
	if f.IP != "" {
		where += " AND ip=?"
		args = append(args, f.IP)
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT ip) FROM connections INDEXED BY connections_status_cover WHERE `+where, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	out.Page = recordPage(f, out.Total)
	values := append([]any{}, args...)
	values = append(values, f.Limit, (out.Page-1)*f.Limit)
	rows, err := tx.QueryContext(ctx, `SELECT ip,COUNT(*),MAX(started_at),MAX(id) FROM connections INDEXED BY connections_status_cover WHERE `+where+` GROUP BY ip ORDER BY MAX(started_at) DESC,MAX(id) DESC,ip LIMIT ? OFFSET ?`, values...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		item := IPRecord{LatestStatus: f.Status}
		if err = rows.Scan(&item.IP, &item.Connections, &item.LastSeen, &item.LatestID); err != nil {
			rows.Close()
			return out, err
		}
		out.Items = append(out.Items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	now := time.Now().UnixMilli()
	for i := range out.Items {
		item := &out.Items[i]
		err = tx.QueryRowContext(ctx, `SELECT SUM(ended_at IS NULL),SUM(auth_attempts),MIN(started_at),MAX(CASE WHEN ended_at IS NULL THEN MAX(0,?-started_at) ELSE MAX(0,duration_ms) END),COALESCE((SELECT label FROM connection_marks WHERE connection_id=?),'') FROM connections INDEXED BY connections_status_cover WHERE status=? AND ip=?`, now, item.LatestID, f.Status, item.IP).Scan(&item.Active, &item.AuthAttempts, &item.FirstSeen, &item.LongestMS, &item.ClientMark)
		if err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}
