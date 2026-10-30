package main

import (
	"sort"
	"time"
)

// A legacy positive lifetime only protects connections before SSH transport is
// established. Slow banner/authentication/virtual sessions have no age limit.
func (c *clientConn) startLifetime() {
	if c.cfg.Mode == "banner" {
		c.retainSlowConnection()
		return
	}
	if c.cfg.MaxConnectionTime == 0 {
		return
	}
	c.lifetimeMu.Lock()
	c.lifetimeTimer = time.AfterFunc(c.cfg.MaxConnectionTime, func() {
		c.lifetimeMu.Lock()
		defer c.lifetimeMu.Unlock()
		if !c.longLived.Load() && c.ctx.Err() == nil {
			c.lifetimeExpired.Store(true)
			c.cancel()
		}
	})
	c.lifetimeMu.Unlock()
}

func (c *clientConn) retainSlowConnection() {
	c.lifetimeMu.Lock()
	defer c.lifetimeMu.Unlock()
	c.longLived.Store(true)
	if c.lifetimeTimer != nil {
		c.lifetimeTimer.Stop()
	}
}

func (c *clientConn) stopLifetime() {
	c.lifetimeMu.Lock()
	defer c.lifetimeMu.Unlock()
	if c.lifetimeTimer != nil {
		c.lifetimeTimer.Stop()
	}
}

// Called under server.mu. Do not retire anything unless all required slots can
// be freed, and do not retire pending handshakes or already closing connections.
func (s *Server) capacityVictimsLocked(ip string, cfg Config) ([]*clientConn, string) {
	globalNeed := len(s.clients) - cfg.MaxClients + 1
	ipNeed := 0
	if cfg.MaxClientsPerIP > 0 {
		ipNeed = s.ipCount[ip] - cfg.MaxClientsPerIP + 1
	}
	if globalNeed <= 0 && ipNeed <= 0 {
		return nil, ""
	}
	eligible := []*clientConn{}
	for c := range s.clients {
		if c.longLived.Load() && !c.capacityEvicted.Load() && c.ctx.Err() == nil {
			eligible = append(eligible, c)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if !eligible[i].started.Equal(eligible[j].started) {
			return eligible[i].started.Before(eligible[j].started)
		}
		return eligible[i].RemoteAddr().String() < eligible[j].RemoteAddr().String()
	})
	selected := []*clientConn{}
	seen := map[*clientConn]bool{}
	for _, c := range eligible {
		if ipNeed > 0 && c.ip == ip {
			selected = append(selected, c)
			seen[c] = true
			ipNeed--
			globalNeed--
		}
	}
	if ipNeed > 0 {
		return nil, "per_ip_limit"
	}
	for _, c := range eligible {
		if globalNeed > 0 && !seen[c] {
			selected = append(selected, c)
			globalNeed--
		}
	}
	if globalNeed > 0 {
		return nil, "max_clients"
	}
	return selected, ""
}
