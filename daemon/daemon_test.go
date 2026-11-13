package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testKey = "EdKGlWFPgosHt/9vGBfcIv39umAsMrvbOxw9c3CZMw4="

// fakeNord serves the handful of NordVPN API endpoints the daemon uses.
func fakeNord(t *testing.T) *httptest.Server {
	t.Helper()
	recs, _ := os.ReadFile("testdata/recommendations.json")
	countries, _ := os.ReadFile("testdata/countries.json")
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/users/services/credentials", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "token" || p != strings.Repeat("ab", 32) {
			w.WriteHeader(401)
			return
		}
		fmt.Fprintf(w, `{"username":"x","password":"y","nordlynx_private_key":%q}`, testKey)
	})
	mux.HandleFunc("/v1/servers/recommendations", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "filters[servers_technologies][identifier]=wireguard_udp") {
			t.Errorf("recommendations query lacks the NordLynx filter: %s", r.URL.RawQuery)
		}
		w.Write(recs)
	})
	mux.HandleFunc("/v1/servers/countries", func(w http.ResponseWriter, r *http.Request) { w.Write(countries) })
	mux.HandleFunc("/v1/helpers/ips/insights", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ip":"86.106.103.115","country":"Sweden","city":"Stockholm","isp":"Nord","protected":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func useTempState(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	stateDir, runDir, initDir = filepath.Join(d, "state"), filepath.Join(d, "run"), filepath.Join(d, "init.d")
}

func TestRecommendationsParsing(t *testing.T) {
	nordAPI = fakeNord(t).URL
	ss, err := recommendServers(context.Background(), Target{Kind: "country", CountryID: 208}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 {
		t.Fatalf("want 2 online servers (offline one dropped), got %d", len(ss))
	}
	s := ss[0]
	if s.Hostname != "se434.nordvpn.com" || s.IP != "86.106.103.115" || s.Country != "Sweden" || s.City != "Stockholm" || s.CountryCode != "SE" {
		t.Fatalf("bad parse: %+v", s)
	}
	if _, err := keyToHex(s.PublicKey); err != nil {
		t.Fatalf("public key: %v", err)
	}
}

func TestWGConfig(t *testing.T) {
	priv, _ := keyToHex(testKey)
	cfg, err := wgConfig(priv, Server{Hostname: "x", IP: "1.2.3.4", PublicKey: testKey}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"private_key=" + priv, "endpoint=1.2.3.4:51820", "allowed_ip=0.0.0.0/0", "persistent_keepalive_interval=25"} {
		if !strings.Contains(cfg, want+"\n") {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
	cfg, _ = wgConfig("", Server{IP: "1.2.3.4", PublicKey: testKey}, false)
	if strings.Contains(cfg, "private_key") {
		t.Error("switch config must not resend the private key")
	}
}

func TestParseStats(t *testing.T) {
	st := parseStats("public_key=ab\nrx_bytes=100\ntx_bytes=50\nlast_handshake_time_sec=1700000000\nlast_handshake_time_nsec=5\n")
	if st.RX != 100 || st.TX != 50 || st.LastHandshake.Unix() != 1700000000 {
		t.Fatalf("%+v", st)
	}
}

func TestCleanToken(t *testing.T) {
	if _, err := cleanToken("  " + strings.Repeat("ab", 32) + "\n"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "short", "has spaces in the middle of it ok", "semi;colon" + strings.Repeat("a", 30)} {
		if _, err := cleanToken(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// upstreamDNS answers every query by echoing it back with the QR bit set.
func upstreamDNS(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, a, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			buf[2] |= 0x80
			pc.WriteTo(buf[:n], a)
		}
	}()
	tl, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tl.Close() })
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 1500)
				n, _ := c.Read(b)
				if n > 4 {
					b[4] |= 0x80
				}
				c.Write(b[:n])
			}()
		}
	}()
	return pc.LocalAddr().String()
}

func TestForwarderFailover(t *testing.T) {
	good := upstreamDNS(t)
	dead, _ := net.ListenPacket("udp", "127.0.0.1:0") // never answers
	defer dead.Close()
	f, err := startForwarder("127.0.0.1:0", []string{dead.LocalAddr().String(), good})
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	f.timeout = 300 * time.Millisecond

	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 3, 'f', 'o', 'o', 0, 0, 1, 0, 1}
	ask := func() []byte {
		c, err := net.Dial("udp", f.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write(q)
		b := make([]byte, 512)
		n, err := c.Read(b)
		if err != nil {
			t.Fatalf("no answer through forwarder: %v", err)
		}
		return b[:n]
	}
	r := ask()
	if r[0] != 0x12 || r[1] != 0x34 || r[2]&0x80 == 0 {
		t.Fatalf("bad reply %x", r)
	}
	// The good upstream is now preferred, so the next answer is fast.
	start := time.Now()
	ask()
	if time.Since(start) > 250*time.Millisecond {
		t.Errorf("forwarder did not remember the working upstream (%s)", time.Since(start))
	}

	// TCP path.
	c, err := net.Dial("tcp", f.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	msg := append([]byte{0, byte(len(q))}, q...)
	c.Write(msg)
	b := make([]byte, 512)
	n, err := c.Read(b)
	if err != nil || n != len(msg) || b[4]&0x80 == 0 {
		t.Fatalf("tcp relay: n=%d err=%v %x", n, err, b[:n])
	}
}

func TestManagerFlow(t *testing.T) {
	useTempState(t)
	nordAPI = fakeNord(t).URL
	t.Setenv("NORRSKEN_FAKE_TUNNEL", "1")
	m := &Manager{cfg: loadConfig(), state: "idle", lastActivity: time.Now(), quit: make(chan struct{})}

	if r := m.handle(request{Cmd: "connect"}); r.OK {
		t.Fatal("connect without an account must fail")
	}
	if r := m.handle(request{Cmd: "setToken", Token: "wrong" + strings.Repeat("0", 40)}); r.OK || !strings.Contains(r.Error, "rejected") {
		t.Fatalf("bad token accepted: %+v", r)
	}
	if r := m.handle(request{Cmd: "setToken", Token: strings.Repeat("ab", 32)}); !r.OK {
		t.Fatalf("setToken: %+v", r)
	}
	if b, _ := os.ReadFile(configPath()); !bytes.Contains(b, []byte(testKey)) {
		t.Fatal("key not persisted")
	}
	if fi, _ := os.Stat(configPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode %v", fi.Mode().Perm())
	}

	if r := m.handle(request{Cmd: "connect", Target: &Target{Kind: "country", CountryID: 208, Label: "Sweden"}}); !r.OK {
		t.Fatalf("connect: %+v", r)
	}
	waitState(t, m, "connected", 6*time.Second)
	st := m.status()
	if st.Server == nil || st.Server.Hostname != "se434.nordvpn.com" || st.ConnectedAt == 0 || st.DNS != "fake" {
		t.Fatalf("status after connect: %+v", st)
	}
	deadline := time.Now().Add(8 * time.Second)
	for m.status().Insight == nil && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if in := m.status().Insight; in == nil || !in.Protected {
		t.Fatalf("insight: %+v", in)
	}

	// Switching location keeps the tunnel and goes through "reconnecting".
	m.handle(request{Cmd: "connect", Target: &Target{Kind: "quick", Label: "Fastest"}})
	if s := m.status().State; s != "reconnecting" {
		t.Fatalf("switch state %s", s)
	}
	waitState(t, m, "connected", 6*time.Second)
	if len(m.status().Recents) != 2 {
		t.Fatalf("recents: %+v", m.status().Recents)
	}

	// Settings: auto-connect writes an init.d hook that points at us.
	m.handle(request{Cmd: "setSettings", Settings: &Settings{AutoConnect: boolp(true), ThreatProtection: boolp(true)}})
	hook, err := os.ReadFile(autostartPath())
	if err != nil || !bytes.Contains(hook, []byte(" boot ")) {
		t.Fatalf("autostart hook: %v %s", err, hook)
	}
	if fi, _ := os.Stat(autostartPath()); fi.Mode().Perm()&0o100 == 0 {
		t.Fatal("hook not executable")
	}
	m.handle(request{Cmd: "setSettings", Settings: &Settings{AutoConnect: boolp(false)}})
	if _, err := os.Stat(autostartPath()); !os.IsNotExist(err) {
		t.Fatal("hook not removed")
	}

	m.handle(request{Cmd: "disconnect"})
	if st := m.status(); st.State != "idle" || st.Server != nil {
		t.Fatalf("after disconnect: %+v", st)
	}

	// Countries come from the API once, then from the disk cache.
	r := m.handle(request{Cmd: "countries"})
	b, _ := json.Marshal(r.Data)
	if !r.OK || !bytes.Contains(b, []byte(`"Stockholm"`)) {
		t.Fatalf("countries: %s", b)
	}
	m2 := &Manager{}
	m2.loadCountryCache()
	if len(m2.countries) != 3 {
		t.Fatalf("country cache: %d", len(m2.countries))
	}

	m.handle(request{Cmd: "logout"})
	if m.status().Linked {
		t.Fatal("still linked after logout")
	}
}

func waitState(t *testing.T, m *Manager, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		m.monitor()
		if s := m.status(); s.State == want {
			return
		} else if s.State == "error" {
			t.Fatalf("error while waiting for %s: %s", want, s.Error)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("state %q never reached (now %q, %s)", want, m.status().State, m.status().Message)
}

func TestPairing(t *testing.T) {
	useTempState(t)
	nordAPI = fakeNord(t).URL
	m := &Manager{cfg: loadConfig(), state: "idle", quit: make(chan struct{})}
	p, err := m.startPairing()
	if err != nil {
		t.Skipf("no LAN address on this machine: %v", err)
	}
	defer m.stopPairing()
	if len(p.PIN) != 6 || !strings.HasSuffix(p.URL, "#"+p.PIN) || len(p.QR) < 21 {
		t.Fatalf("pair info: %+v", p)
	}
	base := strings.Replace(p.Display, lanIP(), "127.0.0.1", 1)
	if r, err := http.Get(base + "/"); err != nil || r.StatusCode != 200 {
		t.Fatalf("page: %v", err)
	}
	post := func(pin, tok string) (int, string) {
		body, _ := json.Marshal(map[string]string{"pin": pin, "token": tok})
		r, err := http.Post(base+"/link", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var j map[string]any
		json.NewDecoder(r.Body).Decode(&j)
		return r.StatusCode, fmt.Sprint(j["message"])
	}
	wrong := "000000"
	if p.PIN == wrong {
		wrong = "111111"
	}
	if code, _ := post(wrong, strings.Repeat("ab", 32)); code != 403 {
		t.Fatalf("wrong pin accepted (%d)", code)
	}
	if code, msg := post(p.PIN, "nope"+strings.Repeat("1", 30)); code != 400 || !strings.Contains(msg, "rejected") {
		t.Fatalf("bad token: %d %s", code, msg)
	}
	if code, msg := post(p.PIN, strings.Repeat("ab", 32)); code != 200 {
		t.Fatalf("link: %d %s", code, msg)
	}
	if !m.status().Linked || !m.status().Pairing.Linked {
		t.Fatal("not linked after pairing")
	}
}

func TestRedaction(t *testing.T) {
	addSecret("supersecretvalue123")
	logf("token is supersecretvalue123")
	tail := logTail(1)
	if strings.Contains(tail[0], "supersecret") {
		t.Fatal("secret leaked into the log")
	}
}

func TestKillSwitchNeverGivesUp(t *testing.T) {
	useTempState(t)
	nordAPI = fakeNord(t).URL
	t.Setenv("NORRSKEN_FAKE_TUNNEL", "dead")
	m := &Manager{cfg: loadConfig(), state: "idle", lastActivity: time.Now(), quit: make(chan struct{})}
	m.handle(request{Cmd: "setToken", Token: strings.Repeat("ab", 32)})
	m.handle(request{Cmd: "setSettings", Settings: &Settings{KillSwitch: boolp(true)}})
	m.handle(request{Cmd: "connect"})
	deadline := time.Now().Add(5 * time.Second)
	for m.status().Server == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	seen := map[string]bool{}
	for i := 0; i < 12; i++ { // many rounds through both servers
		m.mu.Lock()
		m.since = time.Now().Add(-handshakeWait - time.Second)
		m.mu.Unlock()
		m.monitor()
		st := m.status()
		if st.State == "error" || st.State == "idle" {
			t.Fatalf("round %d: kill switch gave up (%s: %s)", i, st.State, st.Error)
		}
		seen[st.Server.Hostname] = true
	}
	if len(seen) != 2 {
		t.Fatalf("should keep rotating through known servers, saw %v", seen)
	}
	m.mu.Lock()
	alive := m.tun != nil
	m.mu.Unlock()
	if !alive {
		t.Fatal("tunnel was torn down (that would lift the block)")
	}
	m.handle(request{Cmd: "disconnect"})
}
