package main

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

func networkDelayBounds(base time.Duration) (shortMin, shortMax, stallMin, stallMax time.Duration) {
	if base < 20*time.Millisecond {
		base = 20 * time.Millisecond
	}
	return base / 20, base / 4, base / 2, base + base/2
}

func networkDelayPolicy(base time.Duration) map[string]any {
	a, b, c, d := networkDelayBounds(base)
	return map[string]any{"short_min_ms": a.Milliseconds(), "short_max_ms": b.Milliseconds(), "stall_min_ms": c.Milliseconds(), "stall_max_ms": d.Milliseconds(), "stall_percent": 25, "max_sessions": 1}
}

func (c *clientConn) networkPause() error {
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint64(seed[:])
	a, b, slowA, slowB := networkDelayBounds(c.server.cfg.Load().Delay)
	if n&3 == 0 {
		a, b = slowA, slowB
	}
	delay := a + time.Duration((n>>2)%uint64(b-a+1))
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

// This is a protocol sink, not a shell emulator. Only one inert session channel
// is accepted; no process, filesystem, SFTP server or forwarding target exists.
// Transport writes are serialized and delayed by clientConn.Write, including
// channel confirmations, request replies and SSH flow-control packets.
func (s *Server) serveSimulatedNetwork(c *clientConn, conn *ssh.ServerConn, channels <-chan ssh.NewChannel, requests <-chan *ssh.Request) {
	defer conn.Close()
	var workers sync.WaitGroup
	var sessionOpen atomic.Bool
	workers.Add(1)
	go func() {
		defer workers.Done()
		for req := range requests {
			c.learnAuthDelay(true)
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()
	for next := range channels {
		c.learnAuthDelay(true)
		if next.ChannelType() != "session" || len(next.ExtraData()) != 0 || !sessionOpen.CompareAndSwap(false, true) {
			_ = next.Reject(ssh.Prohibited, "administratively prohibited")
			continue
		}
		channel, reqs, err := next.Accept()
		if err != nil {
			sessionOpen.Store(false)
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer sessionOpen.Store(false)
			var readers sync.WaitGroup
			readers.Add(2)
			for _, input := range []io.Reader{channel, channel.Stderr()} {
				go func() { defer readers.Done(); _, _ = io.Copy(io.Discard, input) }()
			}
			started := false
			pty := false
			for req := range reqs {
				ok := false
				switch req.Type {
				case "env":
					var env struct{ Name, Value string }
					ok = !started && ssh.Unmarshal(req.Payload, &env) == nil
				case "pty-req":
					var terminal struct {
						Term                         string
						Columns, Rows, Width, Height uint32
						Modes                        string
					}
					ok = !started && !pty && ssh.Unmarshal(req.Payload, &terminal) == nil
					pty = pty || ok
				case "window-change":
					var size struct{ Columns, Rows, Width, Height uint32 }
					ok = pty && ssh.Unmarshal(req.Payload, &size) == nil
				case "shell":
					ok = !started && len(req.Payload) == 0
					started = started || ok
				case "exec":
					var command struct{ Command string }
					ok = !started && ssh.Unmarshal(req.Payload, &command) == nil
					started = started || ok
				}
				if req.WantReply {
					_ = req.Reply(ok, nil)
				}
			}
			_ = channel.Close()
			readers.Wait()
		}()
	}
	workers.Wait()
}
