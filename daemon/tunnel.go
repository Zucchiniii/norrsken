package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tunnelAPI is what the manager needs from a running tunnel. The real one is
// Linux-only; fakeTunnel lets the whole daemon run on a laptop for UI work.
type tunnelAPI interface {
	SwitchServer(Server) error
	Stats() tunStats
	Maintain() bool
	SetDNS([]string)
	SetKillSwitch(bool)
	DNSMode() string
	Close()
}

type tunStats struct {
	RX, TX        uint64
	LastHandshake time.Time
}

func parseStats(out string) tunStats {
	var st tunStats
	var sec, nsec int64
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "rx_bytes":
			n, _ := strconv.ParseUint(v, 10, 64)
			st.RX += n
		case "tx_bytes":
			n, _ := strconv.ParseUint(v, 10, 64)
			st.TX += n
		case "last_handshake_time_sec":
			sec, _ = strconv.ParseInt(v, 10, 64)
		case "last_handshake_time_nsec":
			nsec, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if sec > 0 {
		st.LastHandshake = time.Unix(sec, nsec)
	}
	return st
}

func fakeTunnelEnabled() bool { return os.Getenv("NORRSKEN_FAKE_TUNNEL") != "" }

// NORRSKEN_FAKE_TUNNEL=dead simulates servers that never answer.
func fakeTunnelDead() bool { return os.Getenv("NORRSKEN_FAKE_TUNNEL") == "dead" }

type fakeTunnel struct {
	mu      sync.Mutex
	started time.Time
	server  Server
	closed  bool
}

func newFakeTunnel(s Server) *fakeTunnel {
	logf("fake tunnel up: %s", s.Hostname)
	return &fakeTunnel{started: time.Now(), server: s}
}

func (f *fakeTunnel) SwitchServer(s Server) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("closed")
	}
	f.server, f.started = s, time.Now()
	return nil
}

func (f *fakeTunnel) Stats() tunStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	el := time.Since(f.started)
	st := tunStats{RX: uint64(el.Seconds() * 2_400_000), TX: uint64(el.Seconds() * 90_000)}
	if el > 1200*time.Millisecond && !fakeTunnelDead() {
		st.LastHandshake = f.started.Add(time.Second)
	}
	return st
}

func (f *fakeTunnel) Maintain() bool     { return true }
func (f *fakeTunnel) SetDNS([]string)    {}
func (f *fakeTunnel) SetKillSwitch(bool) {}
func (f *fakeTunnel) DNSMode() string    { return "fake" }
func (f *fakeTunnel) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	logf("fake tunnel down")
}

func wgConfig(privHex string, s Server, includePrivate bool) (string, error) {
	pub, err := keyToHex(s.PublicKey)
	if err != nil {
		return "", fmt.Errorf("server %s has a bad public key", s.Hostname)
	}
	var b strings.Builder
	if includePrivate {
		fmt.Fprintf(&b, "private_key=%s\n", privHex)
	}
	b.WriteString("replace_peers=true\n")
	fmt.Fprintf(&b, "public_key=%s\n", pub)
	fmt.Fprintf(&b, "endpoint=%s\n", net.JoinHostPort(s.IP, "51820"))
	b.WriteString("persistent_keepalive_interval=25\n")
	b.WriteString("replace_allowed_ips=true\n")
	b.WriteString("allowed_ip=0.0.0.0/0\n")
	return b.String(), nil
}
