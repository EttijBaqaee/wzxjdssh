package main

import (
	"sync"
	"time"
)

type authDelayKey struct{ ip, version string }
type authDelayEntry struct {
	base   time.Duration
	until  int64
	seeded bool
}

// Seed once per cooldown, using only the interrupted authentication wait.
// One learning step is reserved as headroom before the observed departure.
func (l *authDelayLearning) seed(key authDelayKey, until int64, now time.Time, elapsed time.Duration, cfg Config) {
	if elapsed <= 0 || until <= now.UnixMilli() {
		return
	}
	l.base(key, until, now, cfg)
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[key]
	if !ok || entry.until != until || entry.seeded {
		return
	}
	value := elapsed - cfg.CooldownAuthStep
	if value < entry.base {
		entry.base = clampAuthDelay(value, cfg)
	}
	entry.seeded = true
	l.entries[key] = entry
}

// Learning is process-local and belongs to one fixed cooldown generation.
// Expired entries are discarded; reconnects never extend their expiry.
type authDelayLearning struct {
	mu        sync.Mutex
	entries   map[authDelayKey]authDelayEntry
	lastPrune time.Time
}

func clampAuthDelay(value time.Duration, cfg Config) time.Duration {
	if value < cfg.CooldownAuthMin {
		return cfg.CooldownAuthMin
	}
	if value > cfg.CooldownAuthMax {
		return cfg.CooldownAuthMax
	}
	return value
}

func (l *authDelayLearning) base(key authDelayKey, until int64, now time.Time, cfg Config) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[authDelayKey]authDelayEntry)
	}
	if now.Sub(l.lastPrune) >= time.Minute {
		for k, entry := range l.entries {
			if entry.until <= now.UnixMilli() {
				delete(l.entries, k)
			}
		}
		l.lastPrune = now
	}
	if until <= now.UnixMilli() {
		delete(l.entries, key)
		return cfg.CooldownAuthDelay
	}
	entry, ok := l.entries[key]
	if !ok || entry.until != until {
		entry = authDelayEntry{base: cfg.CooldownAuthDelay, until: until}
	}
	entry.base = clampAuthDelay(entry.base, cfg)
	// Bound memory even when many unique marked sources reconnect.
	if ok || len(l.entries) < 100000 {
		l.entries[key] = entry
	}
	return entry.base
}

func (l *authDelayLearning) observe(key authDelayKey, until int64, now time.Time, caught bool, cfg Config) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[key]
	if !ok || entry.until != until {
		return
	}
	if until <= now.UnixMilli() {
		delete(l.entries, key)
		return
	}
	if caught {
		entry.base += cfg.CooldownAuthStep
	} else {
		entry.base -= cfg.CooldownAuthStep
	}
	entry.base = clampAuthDelay(entry.base, cfg)
	l.entries[key] = entry
}

func jitterAuthDelay(base time.Duration, cfg Config) (time.Duration, error) {
	base = clampAuthDelay(base, cfg)
	spread := base * time.Duration(cfg.CooldownAuthJitter) / 100
	if spread == 0 {
		return base, nil
	}
	return randomDelay(clampAuthDelay(base-spread, cfg), clampAuthDelay(base+spread, cfg))
}

func (c *clientConn) learnAuthDelay(caught bool) {
	if c.forceSlow.Load() || c.capacityEvicted.Load() || c.ctx.Err() != nil && caught || c.markedUntil <= time.Now().UnixMilli() || !c.authDelayTried.Load() {
		return
	}
	if !c.authLearned.CompareAndSwap(false, true) {
		return
	}
	c.server.authDelays.observe(authDelayKey{c.ip, c.clientVersion}, c.markedUntil, time.Now(), caught, *c.server.cfg.Load())
}

func (c *clientConn) cooldownAuthPause() error {
	cfg := *c.server.cfg.Load()
	base := c.server.authDelays.base(authDelayKey{c.ip, c.clientVersion}, c.markedUntil, time.Now(), cfg)
	delay, err := jitterAuthDelay(base, cfg)
	if err != nil {
		return err
	}
	c.authDelayTried.Store(true)
	return c.trackPause(func() error {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	})
}
