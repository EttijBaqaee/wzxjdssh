package main

import (
	"sort"
	"time"
)

type LiveConnection struct {
	ConnectionID int64  `json:"connection_id"`
	IP           string `json:"ip"`
	Peer         string `json:"peer"`
	Mode         string `json:"mode"`
	StartedAt    int64  `json:"started_at"`
	DurationMS   int64  `json:"duration_ms"`
	DelayedMS    int64  `json:"delayed_ms"`
	Delaying     bool   `json:"delaying"`
}

type LiveIP struct {
	IP                  string `json:"ip"`
	Connections         int    `json:"connections"`
	DelayingConnections int    `json:"delaying_connections"`
}

type LiveStats struct {
	ActiveIPs           int              `json:"active_ips"`
	HeldIPs             int              `json:"held_ips"`
	HeldConnections     int              `json:"held_connections"`
	DelayingIPs         int              `json:"delaying_ips"`
	DelayingConnections int              `json:"delaying_connections"`
	LongestConnections  []LiveConnection `json:"longest_connections"`
	IPConcurrency       []LiveIP         `json:"ip_concurrency"`
	measured            []liveDelay
}

// Account for actual server waits, including the unfinished wait at snapshot time.
// Client think time and SSH key exchange are not counted as imposed delay.
func (c *clientConn) pause() error {
	return c.trackPause(func() error { return c.server.pause(c.ctx) })
}

func (c *clientConn) trackPause(wait func() error) error {
	c.server.mu.Lock()
	c.delayStarted = time.Now()
	c.server.mu.Unlock()
	err := wait()
	c.server.mu.Lock()
	c.delayed += time.Since(c.delayStarted)
	c.delayStarted = time.Time{}
	c.server.mu.Unlock()
	return err
}

// The caller holds s.mu, so counts and both rankings describe one snapshot.
func (s *Server) liveSnapshotLocked(now time.Time) LiveStats {
	live := LiveStats{LongestConnections: []LiveConnection{}, IPConcurrency: []LiveIP{}}
	ips := make(map[string]LiveIP)
	heldIPs := make(map[string]bool)
	for c := range s.clients {
		elapsed := c.delayed
		delaying := !c.delayStarted.IsZero()
		if delaying {
			elapsed += now.Sub(c.delayStarted)
			live.DelayingConnections++
		}
		if c.probeUntil == 0 && (elapsed > 0 || c.simulated.Load()) {
			live.HeldConnections++
			heldIPs[c.ip] = true
		}
		ip := ips[c.ip]
		ip.IP = c.ip
		ip.Connections++
		if delaying {
			ip.DelayingConnections++
		}
		ips[c.ip] = ip
		if c.recordID != 0 {
			live.measured = append(live.measured, liveDelay{ID: c.recordID, DelayedMS: elapsed.Milliseconds()})
		}
		live.LongestConnections = append(live.LongestConnections, LiveConnection{ConnectionID: c.recordID, IP: c.ip, Peer: c.RemoteAddr().String(), Mode: c.cfg.Mode, StartedAt: c.started.UnixMilli(), DurationMS: now.Sub(c.started).Milliseconds(), DelayedMS: elapsed.Milliseconds(), Delaying: delaying})
	}
	live.ActiveIPs = len(ips)
	live.HeldIPs = len(heldIPs)
	for _, ip := range ips {
		if ip.DelayingConnections > 0 {
			live.DelayingIPs++
		}
		live.IPConcurrency = append(live.IPConcurrency, ip)
	}
	sort.Slice(live.LongestConnections, func(i, j int) bool {
		a, b := live.LongestConnections[i], live.LongestConnections[j]
		if a.DelayedMS != b.DelayedMS {
			return a.DelayedMS > b.DelayedMS
		}
		if a.DurationMS != b.DurationMS {
			return a.DurationMS > b.DurationMS
		}
		if a.Peer != b.Peer {
			return a.Peer < b.Peer
		}
		return a.ConnectionID < b.ConnectionID
	})
	sort.Slice(live.IPConcurrency, func(i, j int) bool {
		a, b := live.IPConcurrency[i], live.IPConcurrency[j]
		if a.Connections != b.Connections {
			return a.Connections > b.Connections
		}
		return a.IP < b.IP
	})
	if len(live.LongestConnections) > 10 {
		live.LongestConnections = live.LongestConnections[:10]
	}
	if len(live.IPConcurrency) > 10 {
		live.IPConcurrency = live.IPConcurrency[:10]
	}
	return live
}
