package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"rsc.io/qr"
)

// Pairing lets the user paste their NordVPN access token from a phone or
// laptop instead of typing 64 characters with a TV remote. While the TV
// shows the pairing screen, a small page is served on the LAN. It only
// accepts a token together with the 6-digit code shown on the TV, locks
// after repeated wrong codes, and shuts itself down after 10 minutes.

const (
	pairPort     = 8765
	pairLifetime = 10 * time.Minute
	pairMaxFails = 8
)

type pairing struct {
	m       *Manager
	srv     *http.Server
	pin     string
	url     string
	display string
	qr      []string
	expires time.Time

	mu     sync.Mutex
	fails  int
	linked bool
}

type pairInfo struct {
	URL     string   `json:"url"`
	Display string   `json:"display"`
	PIN     string   `json:"pin"`
	QR      []string `json:"qr"`
	Expires int64    `json:"expires"`
	Linked  bool     `json:"linked"`
}

func (p *pairing) info() *pairInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &pairInfo{URL: p.url, Display: p.display, PIN: p.pin, QR: p.qr, Expires: p.expires.Unix(), Linked: p.linked}
}

func randomPIN() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("%06d", n.Int64())
}

func (m *Manager) startPairing() (*pairInfo, error) {
	m.mu.Lock()
	if m.pair != nil && time.Now().Before(m.pair.expires) {
		p := m.pair
		m.mu.Unlock()
		return p.info(), nil
	}
	m.mu.Unlock()

	ip := lanIP()
	if ip == "" {
		return nil, errors.New("the TV has no local network address")
	}
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", pairPort))
	if err != nil {
		ln, err = net.Listen("tcp4", ":0")
		if err != nil {
			return nil, err
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	p := &pairing{m: m, pin: randomPIN(), expires: time.Now().Add(pairLifetime)}
	p.display = fmt.Sprintf("http://%s:%d", ip, port)
	p.url = p.display + "/#" + p.pin
	if code, err := qr.Encode(p.url, qr.M); err == nil {
		for y := 0; y < code.Size; y++ {
			var row strings.Builder
			for x := 0; x < code.Size; x++ {
				if code.Black(x, y) {
					row.WriteByte('1')
				} else {
					row.WriteByte('0')
				}
			}
			p.qr = append(p.qr, row.String())
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", p.page)
	mux.HandleFunc("/link", p.link)
	p.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 40 * time.Second}
	go p.srv.Serve(ln)
	go func() {
		time.Sleep(pairLifetime)
		m.stopPairingIf(p)
	}()

	m.mu.Lock()
	if m.pair != nil {
		m.pair.srv.Close()
	}
	m.pair = p
	m.mu.Unlock()
	logf("pairing page open at %s", p.display)
	return p.info(), nil
}

func (m *Manager) stopPairing() {
	m.mu.Lock()
	p := m.pair
	m.pair = nil
	m.mu.Unlock()
	if p != nil {
		p.srv.Close()
		logf("pairing page closed")
	}
}

func (m *Manager) stopPairingIf(p *pairing) {
	m.mu.Lock()
	same := m.pair == p
	m.mu.Unlock()
	if same {
		m.stopPairing()
	}
}

func (p *pairing) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; form-action 'self'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	io.WriteString(w, pairHTML)
}

func (p *pairing) link(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	respond := func(code int, ok bool, msg string) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"ok": ok, "message": msg})
	}
	if r.Method != http.MethodPost {
		respond(405, false, "POST only")
		return
	}
	var in struct {
		PIN   string `json:"pin"`
		Token string `json:"token"`
	}
	body := http.MaxBytesReader(w, r.Body, 8192)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(body).Decode(&in); err != nil {
			respond(400, false, "Bad request")
			return
		}
	} else {
		r.Body = body
		r.ParseForm()
		in.PIN, in.Token = r.PostFormValue("pin"), r.PostFormValue("token")
	}

	p.mu.Lock()
	if p.linked {
		p.mu.Unlock()
		respond(409, false, "This TV is already linked. Close the pairing screen on the TV.")
		return
	}
	if time.Now().After(p.expires) || p.fails >= pairMaxFails {
		p.mu.Unlock()
		respond(410, false, "This pairing session has ended. Start a new one on the TV.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(in.PIN)), []byte(p.pin)) != 1 {
		p.fails++
		left := pairMaxFails - p.fails
		p.mu.Unlock()
		logf("pairing: wrong code from %s", r.RemoteAddr)
		if left <= 0 {
			go p.m.stopPairingIf(p)
			respond(403, false, "Too many wrong codes. Pairing was stopped on the TV.")
			return
		}
		respond(403, false, fmt.Sprintf("Wrong code. Check the 6 digits on the TV (%d tries left).", left))
		return
	}
	p.mu.Unlock()

	if err := p.m.setToken(in.Token); err != nil {
		respond(400, false, sentence(err.Error()))
		return
	}
	p.mu.Lock()
	p.linked = true
	p.mu.Unlock()
	go func() {
		time.Sleep(20 * time.Second)
		p.m.stopPairingIf(p)
	}()
	respond(200, true, "Linked! Your TV is ready — pick a country on the TV.")
}

func sentence(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// lanIP returns the TV's private IPv4 address on its real network.
func lanIP() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	best := ""
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || i.Name == "nordlynx" {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil || !n.IP.IsPrivate() {
				continue
			}
			if n.IP.Equal(net.IPv4(10, 5, 0, 2)) {
				continue
			}
			if strings.HasPrefix(i.Name, "eth") || strings.HasPrefix(i.Name, "wlan") || best == "" {
				best = n.IP.String()
			}
		}
	}
	return best
}

const pairHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Link your TV · Norrsken</title>
<style>
:root{color-scheme:dark;--bg:#060912;--card:#0e1424;--line:#1e2840;--text:#eef2fb;--dim:#93a0bd;--accent:#4ff0b4;--err:#ff7a8a}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Inter,Roboto,sans-serif;color:var(--text);
background:radial-gradient(120% 60% at 20% -10%,rgba(79,240,180,.22),transparent 60%),radial-gradient(90% 50% at 90% 0%,rgba(140,110,255,.22),transparent 60%),var(--bg)}
main{max-width:520px;margin:0 auto;padding:40px 16px 56px}
h1{font-size:28px;line-height:1.2;margin:0 0 6px;letter-spacing:-.02em}
p.lead{color:var(--dim);margin:0 0 28px}
.card{background:var(--card);border:1px solid var(--line);border-radius:18px;padding:20px;margin-bottom:16px}
ol{margin:0;padding-left:20px;color:var(--dim)} ol li{margin:6px 0} ol b{color:var(--text);font-weight:600}
a{color:var(--accent)}
label{display:block;font-weight:600;margin:0 0 8px}
input,textarea{width:100%;background:#070b16;color:var(--text);border:1px solid var(--line);border-radius:12px;padding:14px;font:inherit}
input{font-size:28px;letter-spacing:.35em;text-align:center;font-variant-numeric:tabular-nums}
textarea{min-height:96px;resize:vertical;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:14px;word-break:break-all}
input:focus,textarea:focus{outline:2px solid var(--accent);outline-offset:1px}
.field{margin-bottom:18px}
button{width:100%;border:0;border-radius:999px;padding:16px;font:600 17px/1 inherit;font-family:inherit;color:#04110b;background:var(--accent);cursor:pointer}
button[disabled]{opacity:.5}
#msg{margin-top:16px;min-height:24px;font-weight:600}
#msg.err{color:var(--err)} #msg.ok{color:var(--accent)}
.note{color:var(--dim);font-size:13px;margin-top:20px}
</style></head><body><main>
<h1>Link NordVPN to your TV</h1>
<p class="lead">Norrsken uses a NordVPN access token. It stays on your TV and is only sent to NordVPN.</p>
<div class="card"><ol>
<li>Open <a href="https://my.nordaccount.com/" target="_blank" rel="noreferrer">Nord Account</a> and sign in.</li>
<li>Open <b>NordVPN</b> and scroll down to <b>Advanced settings</b>, below Manual configuration.</li>
<li>Create an access token there (pick a long expiry) and copy it.</li>
</ol></div>
<form class="card" id="f" autocomplete="off">
<div class="field"><label for="pin">Code shown on the TV</label>
<input id="pin" name="pin" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" required></div>
<div class="field"><label for="token">Access token</label>
<textarea id="token" name="token" required spellcheck="false" placeholder="Paste the token here"></textarea></div>
<button id="go" type="submit">Link TV</button>
<div id="msg" role="status"></div>
</form>
<p class="note">This page is served by your TV on your home network and closes after 10 minutes.</p>
</main><script>
var pin=document.getElementById('pin'),tok=document.getElementById('token'),msg=document.getElementById('msg'),go=document.getElementById('go');
var h=(location.hash||'').replace('#','');if(/^\d{6}$/.test(h)){pin.value=h;history.replaceState(null,'',location.pathname);}
document.getElementById('f').addEventListener('submit',function(e){e.preventDefault();
msg.className='';msg.textContent='Checking with NordVPN…';go.disabled=true;
fetch('/link',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({pin:pin.value,token:tok.value})})
.then(function(r){return r.json()}).then(function(j){msg.className=j.ok?'ok':'err';msg.textContent=j.message;if(j.ok){tok.value='';}else{go.disabled=false;}})
.catch(function(){msg.className='err';msg.textContent='Could not reach the TV. Is the pairing screen still open?';go.disabled=false;});});
</script></body></html>`
