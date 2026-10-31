package main

import "net/http"

func (w *Web) geoState(rw http.ResponseWriter, r *http.Request) { sendJSON(rw, 200, w.geo.Status()) }
func (w *Web) geoUpdate(rw http.ResponseWriter, r *http.Request) {
	if w.geo == nil || !w.geo.cfg.Enabled {
		sendJSON(rw, 409, map[string]string{"error": "IP 归属查询未启用"})
		return
	}
	w.geo.RequestUpdate()
	sendJSON(rw, http.StatusAccepted, w.geo.Status())
}
func (w *Web) statsGeo(stats StoredStats, live map[string]any, delays []IPDelay) map[string]IPAttribution {
	out := map[string]IPAttribution{}
	add := func(ip string) {
		if _, ok := out[ip]; !ok {
			out[ip] = w.geo.Lookup(ip)
		}
	}
	for _, item := range stats.Sources {
		add(item.Name)
	}
	for _, item := range delays {
		add(item.IP)
	}
	if snapshot, ok := live["live"].(LiveStats); ok {
		for _, item := range snapshot.LongestConnections {
			add(item.IP)
		}
		for _, item := range snapshot.IPConcurrency {
			add(item.IP)
		}
	}
	return out
}
