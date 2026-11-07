package main

import (
	"context"
	"database/sql"
)

type TrafficTotals struct {
	BytesSent     int64 `json:"bytes_sent"`
	BytesReceived int64 `json:"bytes_received"`
}

// Existing schema-4 databases need one backfill. The counters and replacement
// triggers become visible together; subsequent starts do not rescan history.
func (s *Store) initTrafficAnalytics() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var keys int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM analytics_totals WHERE key IN ('bytes_sent','bytes_received')`).Scan(&keys); err != nil {
		return err
	}
	if keys == 2 {
		return tx.Commit()
	}
	var totals TrafficTotals
	if err = tx.QueryRow(`SELECT COALESCE(SUM(MAX(0,bytes_sent)),0),COALESCE(SUM(MAX(0,bytes_received)),0) FROM connections`).Scan(&totals.BytesSent, &totals.BytesReceived); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO analytics_totals(key,value) VALUES ('bytes_sent',?),('bytes_received',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, totals.BytesSent, totals.BytesReceived); err != nil {
		return err
	}
	for name, body := range analyticsTriggers() {
		if _, err = tx.Exec(`DROP TRIGGER IF EXISTS wzx_` + name); err != nil {
			return err
		}
		if _, err = tx.Exec(`CREATE TRIGGER wzx_` + name + ` ` + body); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Overlay only the unsaved part of each live connection on durable totals.
// A connection finishing after the live snapshot is counted once. Missing
// records (already pruned or cleared) must not be added back from that snapshot.
func (s *Store) TotalTraffic(ctx context.Context, active []liveDelay) (TrafficTotals, error) {
	var totals TrafficTotals
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return totals, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT
 COALESCE(MAX(CASE WHEN key='bytes_sent' THEN value END),0),
 COALESCE(MAX(CASE WHEN key='bytes_received' THEN value END),0)
 FROM analytics_totals WHERE key IN ('bytes_sent','bytes_received')`).Scan(&totals.BytesSent, &totals.BytesReceived); err != nil {
		return totals, err
	}
	if len(active) > 0 {
		var sent, received int64
		err = tx.QueryRowContext(ctx, `WITH live AS (
 SELECT CAST(json_extract(value,'$.id') AS INTEGER) AS id,
 MAX(MAX(0,COALESCE(CAST(json_extract(value,'$.bytes_sent') AS INTEGER),0))) AS sent,
 MAX(MAX(0,COALESCE(CAST(json_extract(value,'$.bytes_received') AS INTEGER),0))) AS received
 FROM json_each(?) GROUP BY 1)
 SELECT COALESCE(SUM(MAX(0,l.sent-MAX(0,c.bytes_sent))),0),
 COALESCE(SUM(MAX(0,l.received-MAX(0,c.bytes_received))),0)
 FROM live l JOIN connections c ON c.id=l.id`, sampleJSON(active)).Scan(&sent, &received)
		if err != nil {
			return totals, err
		}
		totals.BytesSent += sent
		totals.BytesReceived += received
	}
	return totals, tx.Commit()
}
