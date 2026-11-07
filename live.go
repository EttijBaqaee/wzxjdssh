package main

import (
	"sort"
	"time"
)

type LiveConnection struct {
	VerifiedPostAuthMS   int64  `json:"verified_post_auth_ms"`
	UnverifiedPostAuthMS int64  `json:"unverified_post_auth_ms"`
	Simulated            bool   `json:"simulated"`
	ConnectionID         int64  `json:"connection_id"`
	IP                   string `json:"ip"`
	Peer                 string `json:"peer"`
	Mode                 string `json:"mode"`
	StartedAt            int64  `json:"started_at"`
	DurationMS           int64  `json:"duration_ms"`
	DelayedMS            int64  `json:"delayed_ms"`
	Delaying             bool   `json:"delaying"`
}

type LiveIP struct {
	IP                  string `json:"ip"`
	Connections         int    `json:"connections"`
	DelayingConnections int    `json:"delaying_connections"`
}

type LiveStats struct {
	SimulatedVerifiedConnections   int              `json:"simulated_verified_connections"`
	SimulatedUnverifiedConnections int              `json:"simulated_unverified_connections"`
	SimulatedPendingChecks         int              `json:"simulated_pending_checks"`
	ActiveIPs                      int              `json:"active_ips"`
	HeldIPs                        int              `json:"held_ips"`
	HeldConnections                int              `json:"held_connections"`
	DelayingIPs                    int              `json:"delaying_ips"`
	DelayingConnections            int              `json:"delaying_connections"`
	LongestConnections             []LiveConnection `json:"longest_connections"`
	IPConcurrency                  []LiveIP         `json:"ip_concurrency"`
	measured                       []liveDelay
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
		verified, unverified := int64(0), int64(0)
		if c.simulated.Load() {
			if c.peer.verifiedAt.Load() > 0 {
				live.SimulatedVerifiedConnections++
			} else {
				live.SimulatedUnverifiedConnections++
			}
			if c.peer.pending.Load() {
				live.SimulatedPendingChecks++
			}
			// Session state has its own lock and may hold it while saving to DB;
			// never acquire that lock while holding s.mu.
			authenticated := c.simulatedAt.Load()
			if authenticated > 0 {
				post := max(int64(0), now.UnixMilli()-authenticated)
				verified = max(int64(0), min(post, c.peer.verifiedAt.Load()-authenticated))
				unverified = post - verified
			}
		}
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
			live.measured = append(live.measured, liveDelay{ID: c.recordID, DelayedMS: elapsed.Milliseconds(), DurationMS: now.Sub(c.started).Milliseconds(), VerifiedPostAuthMS: verified, BytesSent: int64(c.bytesSent.Load()), BytesReceived: int64(c.bytesReceived.Load())})
		}
		live.LongestConnections = append(live.LongestConnections, LiveConnection{ConnectionID: c.recordID, IP: c.ip, Peer: c.RemoteAddr().String(), Mode: c.cfg.Mode, StartedAt: c.started.UnixMilli(), DurationMS: now.Sub(c.started).Milliseconds(), DelayedMS: elapsed.Milliseconds(), Delaying: delaying, Simulated: c.simulated.Load(), VerifiedPostAuthMS: verified, UnverifiedPostAuthMS: unverified})
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
