package main

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type IPRecord struct {
	Geo          IPAttribution `json:"geo"`
	IP           string        `json:"ip"`
	Connections  int64         `json:"connections"`
	Active       int64         `json:"active"`
	AuthAttempts int64         `json:"auth_attempts"`
	FirstSeen    int64         `json:"first_seen"`
	LastSeen     int64         `json:"last_seen"`
	LongestMS    int64         `json:"longest_ms"`
	LatestID     int64         `json:"latest_id"`
	LatestStatus string        `json:"latest_status"`
	ClientMark   string        `json:"client_mark"`
}

type IPRecordPage struct {
	Items []IPRecord `json:"items"`
	Total int64      `json:"total"`
	Page  int        `json:"page"`
	Limit int        `json:"limit"`
}

type IPDelay struct {
	IP                  string `json:"ip"`
	DelayedMS           int64  `json:"delayed_ms"`
	LongestMS           int64  `json:"longest_ms"`
	LongestConnectionID int64  `json:"longest_connection_id"`
	MaxDelayedMS        int64  `json:"max_delayed_ms"`
}

type liveDelay struct {
	VerifiedPostAuthMS int64 `json:"verified_post_auth_ms"`
	ID                 int64 `json:"id"`
	DelayedMS          int64 `json:"delayed_ms"`
	DurationMS         int64 `json:"duration_ms"`
}

func recordWhere(f RecordFilter) (string, []any) {
	clauses, args := []string{"1=1"}, []any{}
	if f.IP != "" {
		clauses = append(clauses, "ip=?")
		args = append(args, f.IP)
	}
	if f.Status != "" {
		clauses = append(clauses, "status=?")
		args = append(args, f.Status)
	}
	if f.Username != "" {
		clauses = append(clauses, "id IN (SELECT id FROM connections WHERE username=? UNION SELECT connection_id FROM auth_attempts WHERE username=?)")
		args = append(args, f.Username, f.Username)
	}
	return strings.Join(clauses, " AND "), args
}

func recordPage(f RecordFilter, total int64) int {
	pages := int((total + int64(f.Limit) - 1) / int64(f.Limit))
	if pages < 1 {
		pages = 1
	}
	if f.Last || f.Page > pages {
		return pages
	}
	if f.Page < 1 {
		return 1
	}
	return f.Page
}

// Filters apply to individual connections, then matching records group by IP.
// Port-level data remains available in the paginated per-IP drill-down.
func (s *Store) ListIPs(ctx context.Context, f RecordFilter) (IPRecordPage, error) {
	if f.Username == "" {
		if f.Status == "" {
			return s.fastIPs(ctx, f)
		}
		return s.statusIPs(ctx, f)
	}
	if f.Limit < 1 || f.Limit > 100 {
		f.Limit = 25
	}
	out := IPRecordPage{Items: []IPRecord{}, Limit: f.Limit}
	where, args := recordWhere(f)
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT ip) FROM connections WHERE `+where, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	out.Page = recordPage(f, out.Total)
	args = append(args, time.Now().UnixMilli(), f.Limit, (out.Page-1)*f.Limit)
	rows, err := tx.QueryContext(ctx, `WITH matched AS (SELECT * FROM connections WHERE `+where+`), grouped AS (
	 SELECT ip,COUNT(*) AS n,SUM(ended_at IS NULL) AS active,SUM(auth_attempts) AS attempts,
	 MIN(started_at) AS first_seen,MAX(started_at) AS last_seen,
	 MAX(CASE WHEN ended_at IS NULL THEN MAX(0,?-started_at) ELSE duration_ms END) AS longest,MAX(id) AS latest_id
	 FROM matched GROUP BY ip)
	 SELECT g.ip,g.n,g.active,g.attempts,g.first_seen,g.last_seen,g.longest,g.latest_id,c.status,COALESCE(m.label,'')
	 FROM grouped g JOIN connections c ON c.id=g.latest_id LEFT JOIN connection_marks m ON m.connection_id=g.latest_id
	 ORDER BY g.last_seen DESC,g.latest_id DESC,g.ip LIMIT ? OFFSET ?`, args...)
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

// Overlay by connection ID rather than adding live totals. If a connection
// finishes between the snapshot and query, its delay is counted only once.
func (s *Store) DelayRanking(ctx context.Context, active []liveDelay) ([]IPDelay, error) {
	return s.fastDelayRanking(ctx, active)
}
