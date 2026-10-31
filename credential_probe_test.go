package main

import (
	"context"
	"golang.org/x/crypto/ssh"
	"net"
	"testing"
	"time"
)

func TestCredentialDeadlineIsPerConnectionNotIP(t *testing.T) {
	cfg := testConfig(t)
	cfg.CredentialTimeout = 350 * time.Millisecond
	cfg.Delay = 600 * time.Millisecond
	cfg.ParallelConnections = 100
	store := testStore(t)
	s, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	probe := connect(t, cfg)
	defer probe.Close()
	bannerLine(t, probe)
	auth := connect(t, cfg)
	defer auth.Close()
	auth.SetDeadline(time.Now().Add(2 * time.Second))
	started := time.Now()
	_, _, _, err = ssh.NewClientConn(auth, address(cfg), &ssh.ClientConfig{User: "synthetic-user", Auth: []ssh.AuthMethod{ssh.Password("synthetic-password")}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err == nil || time.Since(started) < cfg.Delay {
		t.Fatal("credentialed connection did not survive the probe deadline", err, time.Since(started))
	}
	probe.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 1024)
	if n, err := probe.Read(b); n != 0 || err == nil {
		t.Fatal("expired probe received SSH data", n, err)
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("probe was not released")
	}
	eventually(t, func() bool {
		p, e := store.List(context.Background(), RecordFilter{})
		if e != nil || len(p.Items) != 2 {
			return false
		}
		probes, credentials := 0, 0
		for _, r := range p.Items {
			if r.EndedAt == nil {
				return false
			}
			if r.Reason == "no_credentials" && r.AuthAttempts == 0 && r.ClientMark == "" {
				probes++
			}
			if r.AuthAttempts == 1 && r.Reason != "no_credentials" {
				credentials++
			}
		}
		return probes == 1 && credentials == 1
	})
	if until, err := store.ProbeCooldown("127.0.0.1", time.Now()); err != nil || until != 0 {
		t.Fatal("probe affected its source IP", until, err)
	}
	next := connect(t, cfg)
	defer next.Close()
	bannerLine(t, next)
}

func TestCredentialStateCannotReviveExpiredConnection(t *testing.T) {
	c := &clientConn{}
	c.credentialState.Store(credentialExpired)
	if c.submittedCredential("user") == nil {
		t.Fatal("expired credential window revived")
	}
	c.credentialState.Store(credentialWaiting)
	if c.submittedCredential("user") != nil || c.credentialState.Load() != credentialSubmitted {
		t.Fatal("valid credentials not recognized")
	}
	if c.submittedCredential("user") != nil {
		t.Fatal("multiple auth methods rejected")
	}
}

func TestEarlyCredentialProbesNeverAccumulateIPCooldown(t *testing.T) {
	cfg := testConfig(t)
	cfg.ParallelConnections = 100
	store := testStore(t)
	s, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	for i := 0; i < cfg.ProbeFailureThreshold+1; i++ {
		c := connect(t, cfg)
		bannerLine(t, c)
		c.Close()
	}
	eventually(t, func() bool {
		p, e := store.List(context.Background(), RecordFilter{})
		if e != nil || len(p.Items) != cfg.ProbeFailureThreshold+1 {
			return false
		}
		for _, r := range p.Items {
			if r.EndedAt == nil || r.Reason != "no_credentials" || r.ClientMark != "" {
				return false
			}
		}
		return true
	})
	if until, e := store.ProbeCooldown("127.0.0.1", time.Now()); e != nil || until != 0 {
		t.Fatal("early probes accumulated IP cooldown", until, e)
	}
	next := connect(t, cfg)
	defer next.Close()
	bannerLine(t, next)
}
