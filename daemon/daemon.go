package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	handshakeWait  = 15 * time.Second  // give a new server this long to answer
	handshakeStale = 200 * time.Second // WireGuard rejects sessions after 180 s
	idleExit       = 15 * time.Minute
	countriesTTL   = 24 * time.Hour
)

type Manager struct {
	mu   sync.Mutex
	cfg  Config
	boot bool

	state   string // idle | connecting | connected | reconnecting | error
	message string
	lastErr string

	tun         tunnelAPI
	server      *Server
	alternates  []Server
	tried       []Server // servers that did not answer this round
	cycles      int
	target      *Target
	since       time.Time // when the current server was configured
	connectedAt time.Time
	stats       tunStats
	netOK       bool
	insight     *IPInsight
	gen         int
	cancel      context.CancelFunc

	countries   []Country
	countriesAt time.Time

	pair         *pairing
	lastActivity time.Time
	quit         chan struct{}
	quitOnce     sync.Once
}

func runDaemon(boot bool) error {
	lock, err := lockSingleInstance()
	if err != nil {
		return err
	}
	defer lock.Close()
	os.MkdirAll(stateDir, 0o700)
	logf("norrsken %s (build %s) starting (pid %d, boot=%v)", version, buildID, os.Getpid(), boot)
	recoverLeftovers()

	m := &Manager{cfg: loadConfig(), boot: boot, state: "idle", lastActivity: time.Now(), quit: make(chan struct{})}
	addSecret(m.cfg.Token)
	addSecret(m.cfg.PrivateKey)
	m.loadCountryCache()
	// Keep the start-up hook in sync with the setting (e.g. after the app
	// moved to a new install path).
	if m.cfg.Settings.autoConnect() {
		setAutostart(true)
	}
	if t := takeResume(); t != nil && m.cfg.PrivateKey != "" && !boot {
		logf("reconnecting to %s after an update", t.Label)
		m.connect(*t)
	}

	ln, err := listenControl()
	if err != nil {
		return err
	}
	go serveControl(ln, m.handle)

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)

	if boot {
		go m.bootConnect()
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if m.monitor() {
				logf("idle for %s, exiting", idleExit)
				ln.Close()
				return nil
			}
		case s := <-sig:
			logf("got %s, shutting down", s)
			m.disconnect()
			ln.Close()
			return nil
		case <-m.quit:
			m.disconnect()
			ln.Close()
			return nil
		}
	}
}

// ---- request handling ----

func (m *Manager) handle(req request) reply {
	m.mu.Lock()
	m.lastActivity = time.Now()
	m.mu.Unlock()

	switch req.Cmd {
	case "ping":
		return reply{OK: true, Data: map[string]any{"version": version, "build": buildID, "pid": os.Getpid()}}
	case "status":
		return reply{OK: true, Data: m.status()}
	case "countries":
		cs, err := m.getCountries(req.ForceSync)
		if err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true, Data: cs}
	case "connect":
		t := Target{Kind: "quick", Label: "Fastest server"}
		if req.Target != nil {
			t = *req.Target
		}
		if err := m.connect(t); err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true, Data: m.status()}
	case "disconnect":
		m.disconnect()
		return reply{OK: true, Data: m.status()}
	case "setToken":
		if err := m.setToken(req.Token); err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true, Data: m.status()}
	case "logout":
		m.logout()
		return reply{OK: true, Data: m.status()}
	case "setSettings":
		if req.Settings == nil {
			return reply{Error: "missing settings"}
		}
		if err := m.setSettings(*req.Settings); err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true, Data: m.status()}
	case "pairStart":
		p, err := m.startPairing()
		if err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true, Data: p}
	case "pairStop":
		m.stopPairing()
		return reply{OK: true}
	case "log":
		return reply{OK: true, Data: logTail(req.LogLines)}
	case "diag":
		return reply{OK: true, Data: collectEnv()}
	case "bootConnect":
		go m.bootConnect()
		return reply{OK: true}
	case "shutdown":
		if req.Resume {
			m.saveResume()
		}
		go func() { time.Sleep(100 * time.Millisecond); m.quitOnce.Do(func() { close(m.quit) }) }()
		return reply{OK: true}
	}
	return reply{Error: "unknown command " + req.Cmd}
}

type Status struct {
	Version     string     `json:"version"`
	Build       string     `json:"build"`
	State       string     `json:"state"`
	Message     string     `json:"message,omitempty"`
	Error       string     `json:"error,omitempty"`
	Linked      bool       `json:"linked"`
	Server      *Server    `json:"server,omitempty"`
	Target      *Target    `json:"target,omitempty"`
	ConnectedAt int64      `json:"connectedAt,omitempty"`
	Now         int64      `json:"now"`
	RX          uint64     `json:"rx"`
	TX          uint64     `json:"tx"`
	Handshake   int64      `json:"handshakeAgo"`
	Insight     *IPInsight `json:"insight,omitempty"`
	DNS         string     `json:"dns,omitempty"`
	Network     bool       `json:"network"`
	Settings    Settings   `json:"settings"`
	Recents     []Target   `json:"recents"`
	Pairing     *pairInfo  `json:"pairing,omitempty"`
	Root        bool       `json:"root"`
}

func (m *Manager) status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{
		Version: version, Build: buildID, State: m.state, Message: m.message, Error: m.lastErr,
		Linked: m.cfg.PrivateKey != "", Server: m.server, Target: m.target,
		Now: time.Now().Unix(), RX: m.stats.RX, TX: m.stats.TX, Handshake: -1,
		Insight: m.insight, Network: m.netOK, Recents: m.cfg.Recents, Root: os.Geteuid() == 0,
	}
	s.Settings = Settings{
		AutoConnect:      boolp(m.cfg.Settings.autoConnect()),
		KillSwitch:       boolp(m.cfg.Settings.killSwitch()),
		ThreatProtection: boolp(m.cfg.Settings.threatProtection()),
	}
	if s.Recents == nil {
		s.Recents = []Target{}
	}
	if !m.connectedAt.IsZero() {
		s.ConnectedAt = m.connectedAt.Unix()
	}
	if !m.stats.LastHandshake.IsZero() {
		s.Handshake = int64(time.Since(m.stats.LastHandshake).Seconds())
	}
	if m.tun != nil {
		s.DNS = m.tun.DNSMode()
	}
	if m.pair != nil {
		s.Pairing = m.pair.info()
	}
	return s
}

// ---- account ----

func (m *Manager) setToken(raw string) error {
	tok, err := cleanToken(raw)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	key, err := fetchPrivateKey(ctx, tok)
	if err != nil {
		return err
	}
	addSecret(tok)
	addSecret(key)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Token, m.cfg.PrivateKey, m.cfg.LinkedAt = tok, key, time.Now().Unix()
	if m.state == "error" && m.lastErr != "" {
		m.state, m.lastErr = "idle", ""
	}
	logf("NordVPN account linked")
	return saveConfig(m.cfg)
}

func (m *Manager) logout() {
	m.disconnect()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Token, m.cfg.PrivateKey, m.cfg.LinkedAt = "", "", 0
	m.cfg.Settings.AutoConnect = boolp(false)
	setAutostart(false)
	saveConfig(m.cfg)
	logf("NordVPN account unlinked")
}

func (m *Manager) setSettings(s Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.AutoConnect != nil {
		if err := setAutostart(*s.AutoConnect); err != nil {
			return fmt.Errorf("cannot change the start-up hook: %w", err)
		}
	}
	m.cfg.Settings.merge(s)
	if m.tun != nil {
		if s.KillSwitch != nil {
			m.tun.SetKillSwitch(*s.KillSwitch)
		}
		if s.ThreatProtection != nil {
			m.tun.SetDNS(dnsServers(*s.ThreatProtection))
		}
	}
	return saveConfig(m.cfg)
}

// ---- connecting ----

func (m *Manager) connect(t Target) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg.PrivateKey == "" {
		return errors.New("link your NordVPN account first")
	}
	if t.Kind != "quick" && t.Kind != "country" && t.Kind != "city" {
		return errors.New("unknown location type")
	}
	if m.cancel != nil {
		m.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.gen++
	m.target = &t
	m.lastErr, m.tried, m.cycles = "", nil, 0
	if m.tun == nil {
		m.state = "connecting"
		m.connectedAt = time.Time{}
	} else {
		m.state = "reconnecting"
	}
	m.message = "Finding the best server…"
	m.cfg.remember(t)
	saveConfig(m.cfg)
	go m.runConnect(ctx, m.gen, t)
	return nil
}

func (m *Manager) bootConnect() {
	m.mu.Lock()
	ok := m.cfg.Settings.autoConnect() && m.cfg.PrivateKey != "" && m.tun == nil && m.state != "connecting"
	t := Target{Kind: "quick", Label: "Fastest server"}
	if m.cfg.Last != nil {
		t = *m.cfg.Last
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	// Networking often comes up well after our init.d hook runs.
	for i := 0; i < 90 && !hasUplink(); i++ {
		time.Sleep(2 * time.Second)
	}
	logf("auto-connecting to %s", t.Label)
	m.connect(t)
}

func (m *Manager) runConnect(ctx context.Context, gen int, t Target) {
	m.mu.Lock()
	connected := m.tun != nil
	home := m.cfg.homeCountry()
	m.mu.Unlock()

	// While connected, "fastest" would be judged from the VPN exit, so pin
	// it to the country we were in when we last asked from outside.
	query := t
	if t.Kind == "quick" && connected && home > 0 {
		query = Target{Kind: "country", CountryID: home}
	}

	var servers []Server
	var err error
	attempts := 3
	if m.boot {
		attempts = 12 // clock, DNS and Wi-Fi may still be settling
	}
	for i := 0; i < attempts; i++ {
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		servers, err = recommendServers(rctx, query, 10)
		cancel()
		if err == nil || ctx.Err() != nil {
			break
		}
		logf("server lookup failed (%d/%d): %v", i+1, attempts, err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(2+3*i) * time.Second):
		}
	}
	if ctx.Err() != nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if gen != m.gen {
		return
	}
	if err != nil {
		m.failLocked(err)
		return
	}
	if t.Kind == "quick" && !connected {
		m.cfg.HomeCountry = servers[0].CountryID
		saveConfig(m.cfg)
	}
	if err := m.useServerLocked(servers[0]); err != nil {
		m.failLocked(err)
		return
	}
	m.alternates = servers[1:]
}

// useServerLocked points the tunnel at s, creating it if needed.
func (m *Manager) useServerLocked(s Server) error {
	if m.tun == nil {
		priv, err := keyToHex(m.cfg.PrivateKey)
		if err != nil {
			return errors.New("the stored NordLynx key is invalid; link your account again")
		}
		tun, err := openTunnel(s, tunnelOpts{
			PrivateKeyHex: priv,
			KillSwitch:    m.cfg.Settings.killSwitch(),
			DNS:           dnsServers(m.cfg.Settings.threatProtection()),
		})
		if err != nil {
			return err
		}
		m.tun = tun
	} else if err := m.tun.SwitchServer(s); err != nil {
		return err
	}
	srv := s
	m.server = &srv
	m.since = time.Now()
	m.stats = tunStats{}
	m.insight = nil
	m.message = "Connecting to " + s.Name + "…"
	logf("using %s (%s, %s, load %d%%)", s.Hostname, s.City, s.Country, s.Load)
	return nil
}

func (m *Manager) failLocked(err error) {
	logf("connect failed: %v", err)
	if m.tun != nil {
		m.tun.Close()
		m.tun = nil
	}
	m.state, m.lastErr, m.message = "error", err.Error(), ""
	m.server, m.connectedAt = nil, time.Time{}
}

func (m *Manager) disconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.gen++
	if m.tun != nil {
		m.tun.Close()
		m.tun = nil
		logf("disconnected")
	}
	m.state, m.message, m.lastErr = "idle", "", ""
	m.server, m.alternates, m.insight = nil, nil, nil
	m.connectedAt, m.stats = time.Time{}, tunStats{}
}

// monitor runs once a second. It returns true when the daemon should exit.
func (m *Manager) monitor() bool {
	m.mu.Lock()
	tun := m.tun
	if tun == nil {
		idle := m.pair == nil && m.state != "connecting" && time.Since(m.lastActivity) > idleExit && !m.cfg.Settings.autoConnect()
		m.mu.Unlock()
		return idle
	}
	m.mu.Unlock()

	netOK := tun.Maintain()
	st := tun.Stats()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tun != tun {
		return false
	}
	m.stats, m.netOK = st, netOK
	fresh := !st.LastHandshake.IsZero() && time.Since(st.LastHandshake) < handshakeStale && st.LastHandshake.After(m.since.Add(-time.Second))

	switch m.state {
	case "connecting", "reconnecting":
		if fresh {
			if m.connectedAt.IsZero() || m.state == "connecting" {
				m.connectedAt = time.Now()
			}
			m.state, m.message, m.tried, m.cycles = "connected", "", nil, 0
			logf("connected to %s", m.server.Hostname)
			go m.refreshInsight(m.gen)
		} else if !netOK {
			m.message = "Waiting for the TV's network…"
			m.since = time.Now()
		} else if time.Since(m.since) > handshakeWait && m.server != nil {
			m.tryNextLocked(m.server.Name + " did not answer")
		}
	case "connected":
		if !netOK {
			m.state, m.message = "reconnecting", "Network lost — waiting for it to come back…"
			m.since = time.Now()
		} else if !fresh && time.Since(m.since) > handshakeStale {
			m.state = "reconnecting"
			m.tryNextLocked("lost contact with " + m.server.Name)
		}
	}
	return false
}

func (m *Manager) tryNextLocked(reason string) {
	logf("%s", reason)
	if len(m.alternates) == 0 {
		m.cycles++
		if m.cfg.Settings.killSwitch() {
			// Never give up while the kill switch is on: tearing the tunnel
			// down would lift the block. Keep cycling through the servers we
			// already know (the API is unreachable without the tunnel) until
			// one answers or the user disconnects.
			m.since = time.Now()
			if m.server != nil {
				m.tried = append(m.tried, *m.server)
			}
			if len(m.tried) > 0 {
				m.alternates = append([]Server(nil), m.tried...)
			}
			m.tried = nil
			if len(m.alternates) > 0 {
				next := m.alternates[0]
				m.alternates = m.alternates[1:]
				m.useServerLocked(next)
			}
			m.message = "Can't reach NordVPN — internet stays blocked by the kill switch until it reconnects"
			return
		}
		if m.cycles > 3 || m.target == nil {
			m.failLocked(errors.New("no NordVPN server answered; check the TV's internet connection"))
			return
		}
		// Start over from the API with the tunnel down.
		m.tun.Close()
		m.tun = nil
		m.state = "connecting"
		m.message = "Looking for another server…"
		if m.cancel != nil {
			m.cancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		m.gen++
		go m.runConnect(ctx, m.gen, *m.target)
		return
	}
	if m.server != nil {
		m.tried = append(m.tried, *m.server)
	}
	next := m.alternates[0]
	m.alternates = m.alternates[1:]
	if err := m.useServerLocked(next); err != nil {
		m.failLocked(err)
		return
	}
	m.message = reason + " — trying " + next.Name + "…"
}

func (m *Manager) refreshInsight(gen int) {
	for i := 0; i < 3; i++ {
		time.Sleep(time.Duration(1+i*3) * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		in, err := fetchInsight(ctx)
		cancel()
		if err == nil {
			m.mu.Lock()
			if gen == m.gen {
				m.insight = in
			}
			m.mu.Unlock()
			return
		}
	}
}

// ---- resuming after an update ----

func resumePath() string { return filepath.Join(stateDir, "resume.json") }

type resumeNote struct {
	Target Target `json:"target"`
	At     int64  `json:"at"`
}

// saveResume notes where we were connected, so the daemon from the updated
// build reconnects there instead of leaving the TV unprotected.
func (m *Manager) saveResume() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tun == nil || m.target == nil {
		return
	}
	writeFileAtomic(resumePath(), resumeNote{Target: *m.target, At: time.Now().Unix()}, 0o600)
}

func takeResume() *Target {
	b, err := os.ReadFile(resumePath())
	if err != nil {
		return nil
	}
	os.Remove(resumePath())
	var n resumeNote
	if json.Unmarshal(b, &n) != nil || time.Since(time.Unix(n.At, 0)) > 3*time.Minute {
		return nil
	}
	return &n.Target
}

// ---- countries (cached on disk; the list barely changes) ----

func countryCachePath() string { return filepath.Join(stateDir, "countries.json") }

func (m *Manager) loadCountryCache() {
	b, err := os.ReadFile(countryCachePath())
	if err != nil {
		return
	}
	var c struct {
		At        int64     `json:"at"`
		Countries []Country `json:"countries"`
	}
	if json.Unmarshal(b, &c) == nil && len(c.Countries) > 0 {
		m.countries, m.countriesAt = c.Countries, time.Unix(c.At, 0)
	}
}

func (m *Manager) getCountries(force bool) ([]Country, error) {
	m.mu.Lock()
	cached, at := m.countries, m.countriesAt
	m.mu.Unlock()
	if len(cached) > 0 && !force && time.Since(at) < countriesTTL {
		return cached, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cs, err := fetchCountries(ctx)
	if err != nil {
		if len(cached) > 0 {
			return cached, nil
		}
		return nil, err
	}
	m.mu.Lock()
	m.countries, m.countriesAt = cs, time.Now()
	m.mu.Unlock()
	writeFileAtomic(countryCachePath(), map[string]any{"at": time.Now().Unix(), "countries": cs}, 0o600)
	return cs, nil
}

// ---- start-up hook ----

func autostartPath() string { return filepath.Join(initDir, "60-norrsken") }

func setAutostart(on bool) error {
	p := autostartPath()
	if !on {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(initDir, 0o755); err != nil {
		return err
	}
	script := fmt.Sprintf(`#!/bin/sh
# Norrsken (NordVPN): connect when the TV starts.
# Removes itself if the app has been uninstalled.
B=%q
if [ ! -x "$B" ]; then rm -f "$0"; exit 0; fi
"$B" boot >/dev/null 2>&1 &
`, exe)
	return os.WriteFile(p, []byte(script), 0o755)
}
