package main

import (
	"fmt"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestLiveRankingsDeduplicateSortAndRemove(t *testing.T) {
	cfg := testConfig(t)
	s, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var first *clientConn
	for i := 0; i < 15; i++ {
		local, remote := net.Pipe()
		t.Cleanup(func() { local.Close(); remote.Close() })
		ip := "192.0.2.1"
		if i >= 3 {
			ip = fmt.Sprint("192.0.2.", i)
		}
		c := &clientConn{Conn: local, server: s, cfg: cfg, ip: ip, started: now.Add(-time.Hour), recordID: int64(i + 1), delayed: time.Duration(i) * time.Second}
		if i < 2 {
			c.delayStarted = now.Add(-time.Second)
		}
		s.clients[c] = struct{}{}
		if i == 0 {
			first = c
		}
	}
	live := s.liveSnapshotLocked(now)
	if live.ActiveIPs != 13 || live.DelayingIPs != 1 || live.DelayingConnections != 2 {
		t.Fatal("wrong unique-IP accounting", live)
	}
	if len(live.LongestConnections) != 10 || len(live.IPConcurrency) != 10 {
		t.Fatal("rankings not bounded", live)
	}
	if live.IPConcurrency[0].IP != "192.0.2.1" || live.IPConcurrency[0].Connections != 3 || live.IPConcurrency[0].DelayingConnections != 2 {
		t.Fatal("concurrency ranking incorrect", live.IPConcurrency)
	}
	if live.LongestConnections[0].ConnectionID != 15 || live.LongestConnections[0].DelayedMS != 14000 || live.LongestConnections[0].DurationMS != 3600000 {
		t.Fatal("sorted by online age instead of actual delay", live.LongestConnections)
	}
	for i := 1; i < len(live.LongestConnections); i++ {
		if live.LongestConnections[i].DelayedMS > live.LongestConnections[i-1].DelayedMS {
			t.Fatal("delay ranking out of order")
		}
	}
	delete(s.clients, first)
	live = s.liveSnapshotLocked(now)
	if live.DelayingIPs != 1 || live.DelayingConnections != 1 || live.IPConcurrency[0].Connections != 2 {
		t.Fatal("removed connection retained", live)
	}
	clear(s.clients)
	live = s.liveSnapshotLocked(now)
	if live.ActiveIPs != 0 || live.DelayingIPs != 0 || len(live.LongestConnections) != 0 || len(live.IPConcurrency) != 0 {
		t.Fatal("empty snapshot retained connections", live)
	}
}

func TestLiveBannerDelayAndDisconnect(t *testing.T) {
	cfg := testConfig(t)
	cfg.Mode = "banner"
	cfg.Delay = 300 * time.Millisecond
	s := startTest(t, cfg)
	first := connect(t, cfg)
	second := connect(t, cfg)
	defer first.Close()
	defer second.Close()
	snapshot := func() LiveStats { return s.Snapshot()["live"].(LiveStats) }
	eventually(t, func() bool {
		v := snapshot()
		return v.DelayingIPs == 1 && v.DelayingConnections == 2 && v.LongestConnections[0].DelayedMS > 5
	})
	before := snapshot().LongestConnections[0].DelayedMS
	time.Sleep(25 * time.Millisecond)
	if after := snapshot().LongestConnections[0].DelayedMS; after <= before {
		t.Fatal("ongoing wait does not advance", before, after)
	}
	first.Close()
	eventually(t, func() bool { v := snapshot(); return v.DelayingConnections == 1 && v.IPConcurrency[0].Connections == 1 })
	second.Close()
	eventually(t, func() bool { v := snapshot(); return v.DelayingIPs == 0 && len(v.LongestConnections) == 0 })
}

func TestLiveSSHCountsOnlyImposedDelay(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 200 * time.Millisecond
	s := startTest(t, cfg)
	raw := connect(t, cfg)
	defer raw.Close()
	bannerLine(t, raw)
	snapshot := func() LiveStats { return s.Snapshot()["live"].(LiveStats) }
	if v := snapshot(); v.DelayingIPs != 0 || v.LongestConnections[0].DelayedMS != 0 {
		t.Fatal("handshake counted as delay", v)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := ssh.Dial("tcp", address(cfg), &ssh.ClientConfig{User: "live-test", Auth: []ssh.AuthMethod{ssh.Password("live-test")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second})
		if conn != nil {
			conn.Close()
		}
		done <- err
	}()
	eventually(t, func() bool {
		v := snapshot()
		return v.DelayingIPs == 1 && v.DelayingConnections == 1 && v.LongestConnections[0].DelayedMS > 0
	})
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("authentication accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("authentication did not finish")
	}
	eventually(t, func() bool { return snapshot().DelayingConnections == 0 })
	raw.Close()
	eventually(t, func() bool { return len(snapshot().LongestConnections) == 0 })
}

func TestLiveIPv6ConcurrentConnections(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal("IPv6 loopback unavailable in test container", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cfg := testConfig(t)
	cfg.BindAddress = "::1"
	cfg.BindFamily = 6
	cfg.Port = port
	cfg.Mode = "banner"
	cfg.Delay = time.Second
	s := startTest(t, cfg)
	target := net.JoinHostPort("::1", fmt.Sprint(port))
	var connections []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.DialTimeout("tcp6", target, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, c)
		defer c.Close()
	}
	eventually(t, func() bool {
		v := s.Snapshot()["live"].(LiveStats)
		return v.DelayingIPs == 1 && v.DelayingConnections == 2 && len(v.IPConcurrency) == 1 && v.IPConcurrency[0].IP == "::1" && v.IPConcurrency[0].Connections == 2
	})
	v := s.Snapshot()["live"].(LiveStats)
	for _, c := range v.LongestConnections {
		ip, _, err := net.SplitHostPort(c.Peer)
		if err != nil || ip != "::1" {
			t.Fatal("IPv6 peer formatting", c.Peer, err)
		}
	}
	for _, c := range connections {
		c.Close()
	}
	eventually(t, func() bool { return len(s.Snapshot()["live"].(LiveStats).LongestConnections) == 0 })
}
