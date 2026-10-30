package main

import (
	"context"
	"encoding/json"
)

// Merge the complete live snapshot with stored records by connection ID.
// A connection which finished after the snapshot is counted once at its larger
// persisted value. Never derive this sum from the truncated TOP 10 ranking.
func (s *Store) TotalDelay(ctx context.Context, active []liveDelay) (int64, error) {
	data, err := json.Marshal(active)
	if err != nil {
		return 0, err
	}
	var total int64
	err = s.db.QueryRowContext(ctx, `WITH live AS (
	 SELECT CAST(json_extract(value,'$.id') AS INTEGER) AS id, MAX(0,CAST(json_extract(value,'$.delayed_ms') AS INTEGER)) AS delayed
	 FROM json_each(?)
	), samples AS (
	 SELECT id,MAX(0,delayed_ms) AS delayed FROM connections
	 UNION ALL SELECT l.id,l.delayed FROM live l WHERE EXISTS(SELECT 1 FROM connections c WHERE c.id=l.id)
	), per_connection AS (SELECT id,MAX(delayed) AS delayed FROM samples GROUP BY id)
	 SELECT COALESCE(SUM(delayed),0) FROM per_connection`, string(data)).Scan(&total)
	return total, err
}
