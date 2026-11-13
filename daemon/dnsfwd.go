package main

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var (
	nordDNS = []string{"103.86.96.100", "103.86.99.100"}
	// Threat Protection Lite: same resolvers, but they refuse ad, tracker
	// and malware domains.
	nordDNSFiltered = []string{"103.86.96.96", "103.86.99.99"}
	allNordDNS      = append(append([]string{}, nordDNS...), nordDNSFiltered...)
)

func dnsServers(filtered bool) []string {
	if filtered {
		return nordDNSFiltered
	}
	return nordDNS
}

// dnsForwarder relays DNS queries (UDP and TCP) to a list of upstream
// resolvers, trying the next one when a resolver does not answer.
type dnsForwarder struct {
	udp     *net.UDPConn
	tcp     net.Listener
	mu      sync.RWMutex
	ups     []string
	pref    atomic.Int32
	Queries atomic.Uint64
	sem     chan struct{}
	done    chan struct{}
	timeout time.Duration
}

func startForwarder(addr string, upstreams []string) (*dnsForwarder, error) {
	if len(upstreams) == 0 {
		return nil, errors.New("no upstream DNS servers")
	}
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	uc, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	tl, err := net.Listen("tcp", uc.LocalAddr().String())
	if err != nil {
		uc.Close()
		return nil, err
	}
	f := &dnsForwarder{udp: uc, tcp: tl, ups: upstreams, sem: make(chan struct{}, 64), done: make(chan struct{}), timeout: 2 * time.Second}
	go f.serveUDP()
	go f.serveTCP()
	return f, nil
}

func (f *dnsForwarder) Addr() string { return f.udp.LocalAddr().String() }

// upAddr accepts "1.2.3.4" (port 53) or "host:port" (tests).
func upAddr(u string) string {
	if _, _, err := net.SplitHostPort(u); err == nil {
		return u
	}
	return net.JoinHostPort(u, "53")
}

func (f *dnsForwarder) setUpstreams(u []string) {
	f.mu.Lock()
	f.ups = u
	f.mu.Unlock()
	f.pref.Store(0)
}

func (f *dnsForwarder) upstreams() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	n := len(f.ups)
	out := make([]string, n)
	start := int(f.pref.Load()) % n
	for i := 0; i < n; i++ {
		out[i] = f.ups[(start+i)%n]
	}
	return out
}

func (f *dnsForwarder) markGood(up string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for i, u := range f.ups {
		if u == up {
			f.pref.Store(int32(i))
		}
	}
}

func (f *dnsForwarder) serveUDP() {
	buf := make([]byte, 65535)
	for {
		n, from, err := f.udp.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-f.done:
				return
			default:
				time.Sleep(20 * time.Millisecond)
				continue
			}
		}
		if n < 12 {
			continue
		}
		q := append([]byte(nil), buf[:n]...)
		select {
		case f.sem <- struct{}{}:
		default:
			continue // overloaded; the client will retry
		}
		go func() {
			defer func() { <-f.sem }()
			if resp := f.exchangeUDP(q); resp != nil {
				f.udp.WriteToUDP(resp, from)
			}
		}()
	}
}

func (f *dnsForwarder) exchangeUDP(q []byte) []byte {
	f.Queries.Add(1)
	for _, up := range f.upstreams() {
		c, err := net.DialTimeout("udp", upAddr(up), f.timeout)
		if err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(f.timeout))
		if _, err := c.Write(q); err != nil {
			c.Close()
			continue
		}
		resp := make([]byte, 65535)
		for {
			n, err := c.Read(resp)
			if err != nil {
				break
			}
			// Ignore stray datagrams that do not answer our query ID.
			if n >= 12 && resp[0] == q[0] && resp[1] == q[1] {
				c.Close()
				f.markGood(up)
				return resp[:n]
			}
		}
		c.Close()
	}
	return nil
}

func (f *dnsForwarder) serveTCP() {
	for {
		c, err := f.tcp.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
				time.Sleep(20 * time.Millisecond)
				continue
			}
		}
		go f.relayTCP(c)
	}
}

func (f *dnsForwarder) relayTCP(c net.Conn) {
	defer c.Close()
	f.Queries.Add(1)
	var up net.Conn
	for _, u := range f.upstreams() {
		x, err := net.DialTimeout("tcp", upAddr(u), f.timeout)
		if err == nil {
			up = x
			break
		}
	}
	if up == nil {
		return
	}
	defer up.Close()
	deadline := time.Now().Add(15 * time.Second)
	c.SetDeadline(deadline)
	up.SetDeadline(deadline)
	go io.Copy(up, c)
	io.Copy(c, up)
}

func (f *dnsForwarder) close() {
	select {
	case <-f.done:
		return
	default:
	}
	close(f.done)
	f.udp.Close()
	f.tcp.Close()
}
