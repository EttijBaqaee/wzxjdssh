package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

var version = "development"

func runtimeHealth(c Config) error {
	if os.Getenv("WZXJDSSH_WEB_ENABLED") != "false" {
		opts, err := runtimeConfig()
		if err != nil {
			return err
		}
		if opts.WebEnabled {
			host, port, _ := net.SplitHostPort(opts.WebListen)
			if host == "" || host == "0.0.0.0" {
				host = "127.0.0.1"
			}
			if host == "::" {
				host = "::1"
			}
			client := &http.Client{Timeout: 2 * time.Second}
			response, err := client.Get("http://" + net.JoinHostPort(host, port) + opts.WebBasePath + "/healthz")
			if err != nil {
				return err
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				return fmt.Errorf("service not ready")
			}
			return nil
		}
	}
	return health(c)
}

func health(c Config) error {
	host := c.BindAddress
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" || (c.BindAddress == "" && c.BindFamily == 6) {
		host = "::1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(c.Port)), time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if c.Mode == "banner" {
		return nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	verified := false
	sshConn, _, _, err := ssh.NewClientConn(conn, net.JoinHostPort(host, fmt.Sprint(c.Port)), &ssh.ClientConfig{User: "wzxjdssh-health", Timeout: time.Second, HostKeyCallback: func(string, net.Addr, ssh.PublicKey) error { verified = true; return nil }})
	if sshConn != nil {
		sshConn.Close()
	}
	if !verified {
		return fmt.Errorf("SSH health handshake failed: %v", err)
	}
	return nil
}

func run() int {
	flags := flag.NewFlagSet("wzxjdssh", flag.ContinueOnError)
	path := os.Getenv("WZXJDSSH_CONFIG")
	flags.StringVar(&path, "f", path, "configuration file (required when specified)")
	flags.StringVar(&path, "config", path, "configuration file")
	var port, delay, line, max int
	var mode string
	var ipv4, ipv6, verbose, debug, showVersion, check, probe bool
	flags.IntVar(&port, "p", 0, "listening port")
	flags.IntVar(&delay, "d", 0, "delay in milliseconds")
	flags.IntVar(&line, "l", 0, "maximum banner line length")
	flags.IntVar(&max, "m", 0, "maximum admitted clients")
	flags.StringVar(&mode, "mode", "", "ssh or banner")
	flags.BoolVar(&ipv4, "4", false, "IPv4 only")
	flags.BoolVar(&ipv6, "6", false, "IPv6 only")
	flags.BoolVar(&verbose, "v", false, "connection logs")
	flags.BoolVar(&debug, "vv", false, "debug logs")
	flags.BoolVar(&showVersion, "V", false, "print version")
	flags.BoolVar(&check, "check", false, "validate configuration and exit")
	flags.BoolVar(&probe, "health", false, "probe configured listening socket")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if showVersion {
		fmt.Println("wzxjdssh " + version)
		return 0
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		return 2
	}
	if ipv4 && ipv6 {
		fmt.Fprintln(os.Stderr, "-4 and -6 are mutually exclusive")
		return 2
	}
	specified := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { specified[f.Name] = true })
	load := func() (Config, error) {
		c, err := LoadConfig(path)
		if err != nil {
			return c, err
		}
		c, err = configEnvironment(c, os.Getenv)
		if err != nil {
			return c, err
		}
		if specified["p"] {
			c.Port = port
		}
		if specified["d"] {
			if delay < 1 || delay > 86400000 {
				return c, fmt.Errorf("Delay must be 1..86400000 milliseconds")
			}
			c.Delay = time.Duration(delay) * time.Millisecond
		}
		if specified["l"] {
			c.MaxLineLength = line
		}
		if specified["m"] {
			c.MaxClients = max
		}
		if specified["mode"] {
			c.Mode = mode
		}
		if ipv4 {
			c.BindFamily = 4
		}
		if ipv6 {
			c.BindFamily = 6
		}
		if verbose {
			c.LogLevel = 1
		}
		if debug {
			c.LogLevel = 2
		}
		return c, c.Validate()
	}
	c, err := load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration:", err)
		return 2
	}
	if check {
		fmt.Println("configuration valid")
		return 0
	}
	if probe {
		if err := runtimeHealth(c); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	opts, err := runtimeConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "runtime configuration:", err)
		return 2
	}
	store, err := OpenStore(opts.DatabasePath, c.ParallelCooldown)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database startup failed")
		return 1
	}
	defer store.Close()
	if err = store.SetDatabaseBudget(opts.DatabaseMaxMB * 1024 * 1024); err != nil {
		fmt.Fprintln(os.Stderr, "database capacity cleanup failed")
		return 1
	}
	if err = store.Prune(time.Now(), opts.RetentionDays); err != nil {
		fmt.Fprintln(os.Stderr, "database retention cleanup failed")
		return 1
	}
	s, err := NewServer(c, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup:", err)
		return 1
	}
	s.store = store
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGUSR1)
	defer signal.Stop(signals)
	if err = s.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 1
	}
	defer s.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	var maintenance sync.WaitGroup
	maintenance.Add(1)
	go func() { defer maintenance.Done(); store.Maintain(ctx, opts.RetentionDays, s.storageError) }()
	defer func() { cancel(); maintenance.Wait() }()
	var webErrors <-chan error
	if opts.WebEnabled {
		web, err := NewWeb(opts, store, s.Snapshot, s.ConnectionDetail)
		if err != nil {
			fmt.Fprintln(os.Stderr, "web configuration failed")
			return 2
		}
		web.emergency = s
		if err = web.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "web listen:", err)
			return 1
		}
		defer web.Close()
		webErrors = web.errors
		s.log(0, "web_listening", map[string]any{"address": opts.WebListen, "base_path": opts.WebBasePath + "/", "retention_days": opts.RetentionDays})
	}
	for {
		var sig os.Signal
		select {
		case sig = <-signals:
		case <-webErrors:
			fmt.Fprintln(os.Stderr, "web server stopped unexpectedly")
			return 1
		}
		switch sig {
		case syscall.SIGHUP:
			next, err := load()
			if err == nil {
				err = s.Reload(next)
			}
			if err != nil {
				s.log(0, "reload_rejected", map[string]any{"error": err.Error()})
			}
		case syscall.SIGUSR1:
			s.log(0, "totals", s.Snapshot())
		default:
			return 0
		}
	}
}

func main() { os.Exit(run()) }
