package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is immutable after publication. Durations use Go's monotonic timers.
type Config struct {
	Port                 int
	BindAddress          string
	BindFamily           int
	Mode                 string
	Delay                time.Duration
	MaxLineLength        int
	MaxClients           int
	MaxClientsPerIP      int
	ConnectionsPerMinute int
	MaxPendingHandshakes int
	MaxAuthTries         int
	ParallelWindow       time.Duration
	ParallelConnections  int
	HandshakeTimeout     time.Duration
	IdleTimeout          time.Duration
	WriteTimeout         time.Duration
	MaxConnectionTime    time.Duration
	HostKeyDirectory     string
	ServerVersion        string
	LogLevel             int
}

func DefaultConfig() Config {
	return Config{Port: 2222, Mode: "ssh", Delay: 10 * time.Second, MaxLineLength: 32,
		MaxClients: 4096, MaxClientsPerIP: 64, ConnectionsPerMinute: 120,
		MaxPendingHandshakes: 128, MaxAuthTries: 0, ParallelWindow: time.Minute, ParallelConnections: 2, HandshakeTimeout: 20 * time.Second,
		IdleTimeout: 2 * time.Minute, WriteTimeout: 5 * time.Second, MaxConnectionTime: 0,
		HostKeyDirectory: "/data", ServerVersion: "SSH-2.0-OpenSSH_10.0p2 Debian-7+deb13u4", LogLevel: 0}
}

func (c Config) Validate() error {
	checks := []struct {
		ok      bool
		message string
	}{
		{c.Port >= 1 && c.Port <= 65535, "Port must be 1..65535"},
		{c.BindFamily == 0 || c.BindFamily == 4 || c.BindFamily == 6, "BindFamily must be exactly 0, 4 or 6"},
		{c.Mode == "ssh" || c.Mode == "banner", "Mode must be ssh or banner"},
		{c.Delay >= time.Millisecond && c.Delay <= 24*time.Hour, "Delay must be 1..86400000 milliseconds"},
		{c.MaxLineLength >= 3 && c.MaxLineLength <= 255, "MaxLineLength must be 3..255"},
		{c.MaxClients >= 1 && c.MaxClients <= 2147483647, "MaxClients must be 1..2147483647"},
		{c.MaxClientsPerIP >= 0 && c.MaxClientsPerIP <= 100000, "MaxClientsPerIP must be 0..100000"},
		{c.ConnectionsPerMinute >= 0 && c.ConnectionsPerMinute <= 1000000, "ConnectionsPerMinute must be 0..1000000"},
		{c.MaxPendingHandshakes >= 1 && c.MaxPendingHandshakes <= 100000, "MaxPendingHandshakes must be 1..100000"},
		{c.MaxAuthTries >= 0 && c.MaxAuthTries <= 1000000, "MaxAuthTries must be 0..1000000 (0 means unlimited)"},
		{c.ParallelWindow >= time.Millisecond && c.ParallelWindow <= time.Hour, "ParallelWindow must be 1..3600000 milliseconds"},
		{c.ParallelConnections >= 2 && c.ParallelConnections <= 100000, "ParallelConnections must be 2..100000"},
		{c.HandshakeTimeout >= time.Millisecond && c.HandshakeTimeout <= time.Hour, "HandshakeTimeout must be 1..3600000 milliseconds"},
		{c.IdleTimeout >= time.Millisecond && c.IdleTimeout <= 24*time.Hour, "IdleTimeout must be 1..86400000 milliseconds"},
		{c.WriteTimeout >= time.Millisecond && c.WriteTimeout <= time.Minute, "WriteTimeout must be 1..60000 milliseconds"},
		{c.MaxConnectionTime >= 0 && c.MaxConnectionTime <= 7*24*time.Hour, "MaxConnectionTime must be 0..604800000 milliseconds (0 means unlimited)"},
		{c.HostKeyDirectory != "", "HostKeyDirectory cannot be empty"},
		{c.LogLevel >= 0 && c.LogLevel <= 2, "LogLevel must be 0..2"},
		{strings.HasPrefix(c.ServerVersion, "SSH-2.0-") && len(c.ServerVersion) > 8 && len(c.ServerVersion) <= 253, "ServerVersion must be an SSH-2.0 identification of at most 253 bytes"},
	}
	for _, check := range checks {
		if !check.ok {
			return errors.New(check.message)
		}
	}
	for _, b := range []byte(c.ServerVersion) {
		if b < 32 || b > 126 {
			return errors.New("ServerVersion must contain printable ASCII without CR/LF")
		}
	}
	ident := strings.SplitN(c.ServerVersion, " ", 2)[0]
	if len(ident) <= 8 {
		return errors.New("ServerVersion software name cannot be empty")
	}
	if c.BindAddress != "" {
		ip := net.ParseIP(c.BindAddress)
		if ip == nil {
			return errors.New("BindAddress must be an IP literal")
		}
		if c.BindFamily == 4 && ip.To4() == nil {
			return errors.New("BindAddress conflicts with IPv4 BindFamily")
		}
		if c.BindFamily == 6 && ip.To4() != nil {
			return errors.New("BindAddress conflicts with IPv6 BindFamily")
		}
	}
	return nil
}

func parseConfig(r io.Reader) (Config, error) {
	c := DefaultConfig()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), 64*1024)
	seen := map[string]bool{}
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if text == "" {
			continue
		}
		parts := strings.Fields(text)
		if len(parts) < 2 {
			return c, fmt.Errorf("line %d: missing value", line)
		}
		key := parts[0]
		if seen[key] {
			return c, fmt.Errorf("line %d: duplicate option %s", line, key)
		}
		seen[key] = true
		value := strings.TrimSpace(text[len(key):])
		var err error
		setInt := func(dst *int) {
			if len(parts) != 2 {
				err = errors.New("expected one integer")
				return
			}
			*dst, err = strconv.Atoi(value)
		}
		setDuration := func(dst *time.Duration) {
			n, e := strconv.ParseInt(value, 10, 64)
			if e != nil || n < 1 || n > 604800000 {
				err = errors.New("invalid millisecond duration")
				return
			}
			*dst = time.Duration(n) * time.Millisecond
		}
		switch key {
		case "Port":
			setInt(&c.Port)
		case "BindAddress":
			c.BindAddress = value
		case "BindFamily":
			if value != "0" && value != "4" && value != "6" {
				err = errors.New("must be exactly 0, 4 or 6")
			} else {
				setInt(&c.BindFamily)
			}
		case "Mode":
			c.Mode = value
		case "Delay":
			setDuration(&c.Delay)
		case "MaxLineLength":
			setInt(&c.MaxLineLength)
		case "MaxClients":
			setInt(&c.MaxClients)
		case "MaxClientsPerIP":
			setInt(&c.MaxClientsPerIP)
		case "ConnectionsPerMinute":
			setInt(&c.ConnectionsPerMinute)
		case "MaxPendingHandshakes":
			setInt(&c.MaxPendingHandshakes)
		case "MaxAuthTries":
			setInt(&c.MaxAuthTries)
		case "ParallelWindow":
			setDuration(&c.ParallelWindow)
		case "ParallelConnections":
			setInt(&c.ParallelConnections)
		case "HandshakeTimeout":
			setDuration(&c.HandshakeTimeout)
		case "IdleTimeout":
			setDuration(&c.IdleTimeout)
		case "WriteTimeout":
			setDuration(&c.WriteTimeout)
		case "MaxConnectionTime":
			if value == "0" {
				c.MaxConnectionTime = 0
			} else {
				setDuration(&c.MaxConnectionTime)
			}
		case "HostKeyDirectory":
			c.HostKeyDirectory = value
		case "ServerVersion":
			c.ServerVersion = value
		case "LogLevel":
			setInt(&c.LogLevel)
		default:
			return c, fmt.Errorf("line %d: unknown option %q", line, key)
		}
		if err != nil {
			return c, fmt.Errorf("line %d: %s: %w", line, key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	return c, c.Validate()
}

func LoadConfig(path string) (Config, error) {
	if path == "" {
		return DefaultConfig(), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %q: %w", path, err)
	}
	defer f.Close()
	return parseConfig(f)
}
