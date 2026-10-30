package main

import (
	"testing"
	"time"
)

func TestComposeStrategyEnvironment(t *testing.T) {
	lookup := func(values map[string]string) func(string) string { return func(k string) string { return values[k] } }
	base := DefaultConfig()
	base.ConnectionsPerMinute = 37
	base.Delay = 3 * time.Second
	c, err := configEnvironment(base, lookup(map[string]string{"WZXJDSSH_CONNECTIONS_PER_MINUTE": "2048", "WZXJDSSH_MAX_CLIENTS": "8192", "WZXJDSSH_MAX_CLIENTS_PER_IP": "512", "WZXJDSSH_MAX_PENDING_HANDSHAKES": "256", "WZXJDSSH_PARALLEL_CONNECTIONS": "3", "WZXJDSSH_PARALLEL_WINDOW_MS": "30000", "WZXJDSSH_PARALLEL_COOLDOWN_MS": "3600000", "WZXJDSSH_DELAY_MS": "15000", "WZXJDSSH_MAX_AUTH_TRIES": "0"}))
	if err != nil || c.ConnectionsPerMinute != 2048 || c.MaxClients != 8192 || c.MaxClientsPerIP != 512 || c.MaxPendingHandshakes != 256 || c.ParallelConnections != 3 || c.ParallelWindow != 30*time.Second || c.ParallelCooldown != time.Hour || c.Delay != 15*time.Second || c.MaxAuthTries != 0 {
		t.Fatal(c, err)
	}
	for _, v := range []string{"", "0"} {
		c, err = configEnvironment(base, lookup(map[string]string{"WZXJDSSH_CONNECTIONS_PER_MINUTE": v}))
		want := 0
		if v == "" {
			want = 37
		}
		if err != nil || c.ConnectionsPerMinute != want || c.Delay != base.Delay {
			t.Fatal(c, err)
		}
	}
	for key, values := range map[string][]string{
		"CONNECTIONS_PER_MINUTE": {"-1", "1000001", "abc", "9999999999999999999999999"},
		"MAX_CLIENTS":            {"0", "2147483648"}, "MAX_CLIENTS_PER_IP": {"-1", "100001"},
		"MAX_PENDING_HANDSHAKES": {"0", "100001"}, "MAX_AUTH_TRIES": {"-1", "1000001"},
		"PARALLEL_CONNECTIONS": {"1", "100001"}, "DELAY_MS": {"0", "86400001"},
		"PARALLEL_WINDOW_MS": {"0", "3600001"}, "PARALLEL_COOLDOWN_MS": {"0", "604800001", "18446744073709551616"},
		"HANDSHAKE_TIMEOUT_MS": {"0", "3600001"}, "IDLE_TIMEOUT_MS": {"0", "86400001"},
		"WRITE_TIMEOUT_MS": {"0", "60001"}, "MAX_CONNECTION_TIME_MS": {"-1", "604800001"},
		"MODE": {"invalid"}, "MAX_LINE_LENGTH": {"2", "256"}, "LOG_LEVEL": {"-1", "3"},
		"PROBE_FAILURE_THRESHOLD": {"0", "100001"}, "PROBE_COOLDOWN_MS": {"0", "86400001"},
	} {
		for _, v := range values {
			t.Run(key+"/"+v, func(t *testing.T) {
				if _, err := configEnvironment(base, lookup(map[string]string{"WZXJDSSH_" + key: v})); err == nil {
					t.Fatal("invalid Compose environment accepted")
				}
			})
		}
	}
	if DefaultConfig().ConnectionsPerMinute != 2048 {
		t.Fatal("unexpected default rate")
	}
}
