//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

const (
	ifaceName = "nordlynx"
	tunMTU    = 1420
	// NordLynx hands every client the same inner address.
	tunAddr = "10.5.0.2/32"
	// Kill-switch blackholes sit under the tunnel routes with a worse metric,
	// so they only take effect if the tunnel interface disappears.
	killMetric = 4242
)

var tunSrc = net.IPv4(10, 5, 0, 2)

// undoLog records every system change so it can be reverted, even by a later
// daemon after a crash. It is persisted to restore.json after each change.
type undoLog struct {
	Routes   []savedRoute      `json:"routes,omitempty"`
	IPv6     map[string]string `json:"ipv6,omitempty"`
	IPTables string            `json:"iptables,omitempty"` // binary used for the DNS chain
	Resolv   string            `json:"resolv,omitempty"`   // bind-mounted target
	// ResolvInit: the mount was made in PID 1's mount namespace via nsenter.
	ResolvInit bool `json:"resolvInit,omitempty"`
}

type savedRoute struct {
	Dst      string `json:"dst"`
	Gw       string `json:"gw,omitempty"`
	Link     string `json:"link,omitempty"`
	Type     int    `json:"type,omitempty"`
	Priority int    `json:"prio,omitempty"`
	Scope    int    `json:"scope,omitempty"`
}

func restorePath() string { return filepath.Join(stateDir, "restore.json") }

func (u *undoLog) save() { writeFileAtomic(restorePath(), u, 0o600) }

func (s savedRoute) route() (netlink.Route, bool) {
	_, dst, err := net.ParseCIDR(s.Dst)
	if err != nil {
		return netlink.Route{}, false
	}
	r := netlink.Route{Dst: dst, Type: s.Type, Priority: s.Priority, Scope: netlink.Scope(s.Scope), Table: unix.RT_TABLE_MAIN}
	if s.Gw != "" {
		r.Gw = net.ParseIP(s.Gw)
	}
	if s.Link != "" {
		l, err := netlink.LinkByName(s.Link)
		if err != nil {
			return r, false // link is gone and took its routes with it
		}
		r.LinkIndex = l.Attrs().Index
	}
	return r, true
}

func toSaved(r netlink.Route) savedRoute {
	s := savedRoute{Dst: r.Dst.String(), Type: r.Type, Priority: r.Priority, Scope: int(r.Scope)}
	if r.Gw != nil {
		s.Gw = r.Gw.String()
	}
	if r.LinkIndex > 0 {
		if l, err := netlink.LinkByIndex(r.LinkIndex); err == nil {
			s.Link = l.Attrs().Name
		}
	}
	return s
}

type physRoute struct {
	LinkIndex int
	LinkName  string
	Gw        net.IP
}

func (p physRoute) String() string {
	if p.Gw == nil {
		return "dev " + p.LinkName
	}
	return "via " + p.Gw.String() + " dev " + p.LinkName
}

func (p physRoute) same(o physRoute) bool { return p.LinkIndex == o.LinkIndex && p.Gw.Equal(o.Gw) }

// defaultRoute finds the TV's real IPv4 default route, ignoring our tunnel.
func defaultRoute(exclude int) (physRoute, error) {
	for _, table := range []int{unix.RT_TABLE_MAIN, unix.RT_TABLE_UNSPEC} {
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return physRoute{}, err
		}
		var best *netlink.Route
		for i := range routes {
			r := &routes[i]
			if r.Dst != nil {
				if ones, _ := r.Dst.Mask.Size(); ones != 0 {
					continue
				}
			}
			if r.LinkIndex <= 0 || r.LinkIndex == exclude || (r.Type != 0 && r.Type != unix.RTN_UNICAST) {
				continue
			}
			if best == nil || r.Priority < best.Priority {
				best = r
			}
		}
		if best != nil {
			p := physRoute{LinkIndex: best.LinkIndex, Gw: best.Gw}
			if l, err := netlink.LinkByIndex(best.LinkIndex); err == nil {
				p.LinkName = l.Attrs().Name
			}
			return p, nil
		}
	}
	return physRoute{}, errors.New("the TV has no internet connection (no default route)")
}

type tunnelOpts struct {
	PrivateKeyHex string
	KillSwitch    bool
	DNS           []string
}

type Tunnel struct {
	mu     sync.Mutex
	dev    *device.Device
	tdev   tun.Device
	index  int
	phys   physRoute
	server Server
	epIP   net.IP
	undo   *undoLog
	dns    *dnsGuard
	opts   tunnelOpts
	closed bool
}

func ensureTunNode() error {
	if _, err := os.Stat("/dev/net/tun"); err == nil {
		return nil
	}
	exec.Command("modprobe", "tun").Run()
	if _, err := os.Stat("/dev/net/tun"); err == nil {
		return nil
	}
	os.MkdirAll("/dev/net", 0o755)
	if err := unix.Mknod("/dev/net/tun", unix.S_IFCHR|0o666, int(unix.Mkdev(10, 200))); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("this TV has no /dev/net/tun and it could not be created: %w", err)
	}
	return nil
}

// bringUp creates the NordLynx interface and routes all IPv4 traffic into it.
func bringUp(s Server, o tunnelOpts) (t *Tunnel, err error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("norrsken is not running as root — is the TV rooted with Homebrew Channel?")
	}
	if err := ensureTunNode(); err != nil {
		return nil, err
	}
	phys, err := defaultRoute(0)
	if err != nil {
		return nil, err
	}
	if l, err := netlink.LinkByName(ifaceName); err == nil {
		netlink.LinkDel(l)
	}

	t = &Tunnel{server: s, phys: phys, opts: o, undo: &undoLog{}}
	defer func() {
		if err != nil {
			t.Close()
			t = nil
		}
	}()

	tdev, err := tun.CreateTUN(ifaceName, tunMTU)
	if err != nil {
		return t, fmt.Errorf("cannot create the tunnel interface: %w", err)
	}
	t.tdev = tdev
	logger := &device.Logger{
		Verbosef: device.DiscardLogf,
		Errorf:   func(f string, a ...any) { logf("wireguard: "+f, a...) },
	}
	t.dev = device.NewDevice(tdev, conn.NewDefaultBind(), logger)
	cfg, err := wgConfig(o.PrivateKeyHex, s, true)
	if err != nil {
		return t, err
	}
	if err := t.dev.IpcSet(cfg); err != nil {
		return t, fmt.Errorf("wireguard config rejected: %w", err)
	}
	if err := t.dev.Up(); err != nil {
		return t, err
	}

	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return t, err
	}
	t.index = link.Attrs().Index
	addr, _ := netlink.ParseAddr(tunAddr)
	if err := netlink.AddrAdd(link, addr); err != nil && !errors.Is(err, unix.EEXIST) {
		return t, fmt.Errorf("cannot set tunnel address: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return t, err
	}

	// 1. Keep the encrypted UDP flow on the real network.
	t.epIP = net.ParseIP(s.IP).To4()
	if err := t.pinEndpoint(t.epIP, phys); err != nil {
		return t, fmt.Errorf("cannot route to the VPN server: %w", err)
	}
	// 2. Everything else goes into the tunnel. Two /1 routes beat the default
	//    route without replacing it, and LAN routes (more specific) still win,
	//    so casting and the ThinQ app keep working.
	for _, d := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(d)
		r := netlink.Route{Dst: dst, LinkIndex: t.index, Scope: netlink.SCOPE_LINK, Src: tunSrc, Table: unix.RT_TABLE_MAIN}
		if err := netlink.RouteReplace(&r); err != nil {
			return t, fmt.Errorf("cannot add tunnel route: %w", err)
		}
		// Routes on our own link vanish with it; no need to journal them.
	}
	if o.KillSwitch {
		t.addKillSwitch()
	}
	t.blockIPv6()
	t.dns = startDNS(o.DNS, t.undo)
	t.undo.save()
	logf("tunnel up: %s (%s) %s, dns=%s", s.Hostname, s.IP, phys, t.dns.mode)
	return t, nil
}

func (t *Tunnel) journalRoute(r netlink.Route) {
	s := toSaved(r)
	for _, x := range t.undo.Routes {
		if x == s {
			return
		}
	}
	t.undo.Routes = append(t.undo.Routes, s)
	t.undo.save()
}

func (t *Tunnel) forgetRoute(dst string) {
	out := t.undo.Routes[:0]
	for _, x := range t.undo.Routes {
		if x.Dst != dst {
			out = append(out, x)
		}
	}
	t.undo.Routes = out
	t.undo.save()
}

func hostNet(ip net.IP) *net.IPNet { return &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)} }

func (t *Tunnel) pinEndpoint(ip net.IP, p physRoute) error {
	r := netlink.Route{Dst: hostNet(ip), LinkIndex: p.LinkIndex, Gw: p.Gw, Table: unix.RT_TABLE_MAIN}
	if p.Gw == nil {
		r.Scope = netlink.SCOPE_LINK
	}
	if err := netlink.RouteReplace(&r); err != nil {
		return err
	}
	t.forgetRoute(hostNet(ip).String())
	t.journalRoute(r)
	return nil
}

// unreachableEndpoint stops a routing loop (WireGuard sending its own packets
// into the tunnel) while the TV has no network at all.
func (t *Tunnel) unreachableEndpoint(ip net.IP) {
	r := netlink.Route{Dst: hostNet(ip), Type: unix.RTN_UNREACHABLE, Table: unix.RT_TABLE_MAIN}
	if err := netlink.RouteReplace(&r); err == nil {
		t.forgetRoute(hostNet(ip).String())
		t.journalRoute(r)
	}
}

func (t *Tunnel) addKillSwitch() {
	for _, d := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(d)
		r := netlink.Route{Dst: dst, Type: unix.RTN_BLACKHOLE, Priority: killMetric, Table: unix.RT_TABLE_MAIN}
		if err := netlink.RouteAdd(&r); err != nil && !errors.Is(err, unix.EEXIST) {
			logf("kill switch: %v", err)
			continue
		}
		t.journalRoute(r)
	}
}

func (t *Tunnel) removeKillSwitch() {
	for _, d := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(d)
		r := netlink.Route{Dst: dst, Type: unix.RTN_BLACKHOLE, Priority: killMetric, Table: unix.RT_TABLE_MAIN}
		netlink.RouteDel(&r)
		out := t.undo.Routes[:0]
		for _, x := range t.undo.Routes {
			if !(x.Dst == d && x.Type == unix.RTN_BLACKHOLE) {
				out = append(out, x)
			}
		}
		t.undo.Routes = out
	}
	t.undo.save()
}

func (t *Tunnel) SetKillSwitch(on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.opts.KillSwitch = on
	if on {
		t.addKillSwitch()
	} else {
		t.removeKillSwitch()
	}
}

// blockIPv6 disables IPv6 on every real interface. NordLynx is IPv4-only, so
// without this, IPv6-capable apps would bypass the tunnel.
func (t *Tunnel) blockIPv6() {
	if t.undo.IPv6 == nil {
		t.undo.IPv6 = map[string]string{}
	}
	ents, err := os.ReadDir("/proc/sys/net/ipv6/conf")
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if n == "all" || n == "lo" || n == ifaceName {
			continue
		}
		if _, seen := t.undo.IPv6[n]; seen {
			continue
		}
		p := filepath.Join("/proc/sys/net/ipv6/conf", n, "disable_ipv6")
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		cur := strings.TrimSpace(string(b))
		if cur == "1" {
			continue
		}
		if err := os.WriteFile(p, []byte("1"), 0o644); err == nil {
			t.undo.IPv6[n] = cur
		}
	}
	t.undo.save()
}

func restoreIPv6(m map[string]string) {
	for n, v := range m {
		os.WriteFile(filepath.Join("/proc/sys/net/ipv6/conf", n, "disable_ipv6"), []byte(v), 0o644)
	}
}

// SwitchServer moves the peer to a new server without tearing the interface
// down, so there is no window where traffic could leak.
func (t *Tunnel) SwitchServer(s Server) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("tunnel closed")
	}
	newIP := net.ParseIP(s.IP).To4()
	if err := t.pinEndpoint(newIP, t.phys); err != nil {
		return err
	}
	cfg, err := wgConfig("", s, false)
	if err != nil {
		return err
	}
	if err := t.dev.IpcSet(cfg); err != nil {
		return err
	}
	if !newIP.Equal(t.epIP) {
		old := netlink.Route{Dst: hostNet(t.epIP), Table: unix.RT_TABLE_MAIN}
		netlink.RouteDel(&old)
		t.forgetRoute(hostNet(t.epIP).String())
	}
	t.epIP, t.server = newIP, s
	logf("switched to %s (%s)", s.Hostname, s.IP)
	return nil
}

func (t *Tunnel) Stats() tunStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	var st tunStats
	if t.closed || t.dev == nil {
		return st
	}
	out, err := t.dev.IpcGet()
	if err != nil {
		return st
	}
	return parseStats(out)
}

// Maintain runs every second. It follows network changes (Wi-Fi/Ethernet
// switch, new gateway, DHCP renew) and repairs routes that something else
// removed. It reports whether the TV currently has an uplink.
func (t *Tunnel) Maintain() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	p, err := defaultRoute(t.index)
	if err != nil {
		t.unreachableEndpoint(t.epIP)
		t.phys = physRoute{}
		return false
	}
	if !p.same(t.phys) {
		logf("network changed: %s -> %s", t.phys, p)
		t.phys = p
		t.pinEndpoint(t.epIP, p)
		t.blockIPv6()
	} else if !hasRoute(hostNet(t.epIP), p.LinkIndex, p.Gw) {
		logf("endpoint route was lost, restoring")
		t.pinEndpoint(t.epIP, p)
	}
	for _, d := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(d)
		if !hasRoute(dst, t.index, nil) {
			netlink.RouteReplace(&netlink.Route{Dst: dst, LinkIndex: t.index, Scope: netlink.SCOPE_LINK, Src: tunSrc, Table: unix.RT_TABLE_MAIN})
			logf("tunnel route %s was missing, restored", d)
		}
	}
	return true
}

// hasRoute reports whether the main table holds dst via that link/gateway.
// On lookup errors it answers true so a flaky netlink never causes churn.
func hasRoute(dst *net.IPNet, link int, gw net.IP) bool {
	rs, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Dst: dst, Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_DST|netlink.RT_FILTER_TABLE)
	if err != nil {
		return true
	}
	for _, r := range rs {
		if r.LinkIndex == link && r.Gw.Equal(gw) {
			return true
		}
	}
	return false
}

func (t *Tunnel) SetDNS(servers []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dns != nil {
		t.dns.update(servers)
	}
}

func (t *Tunnel) DNSMode() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dns == nil {
		return "none"
	}
	return t.dns.mode
}

// Close removes everything bringUp did.
func (t *Tunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	if t.dns != nil {
		t.dns.stop()
	}
	undoAll(t.undo)
	if t.dev != nil {
		t.dev.Close() // also closes the TUN device
	} else if t.tdev != nil {
		t.tdev.Close()
	}
	if l, err := netlink.LinkByName(ifaceName); err == nil {
		netlink.LinkDel(l)
	}
	os.Remove(restorePath())
	logf("tunnel down")
}

func undoAll(u *undoLog) {
	if u == nil {
		return
	}
	for _, s := range u.Routes {
		if r, ok := s.route(); ok {
			netlink.RouteDel(&r)
		}
	}
	restoreIPv6(u.IPv6)
	if u.IPTables != "" {
		removeDNSChain(u.IPTables)
	}
	if u.Resolv != "" {
		unmountResolv(u.Resolv, u.ResolvInit)
	}
}

// recoverLeftovers reverts changes left behind by a daemon that crashed or
// was killed (including kill-switch blackholes).
func recoverLeftovers() {
	b, err := os.ReadFile(restorePath())
	if err == nil {
		var u undoLog
		if json.Unmarshal(b, &u) == nil {
			logf("recovering from an unclean shutdown")
			undoAll(&u)
		}
		os.Remove(restorePath())
	}
	if l, err := netlink.LinkByName(ifaceName); err == nil {
		netlink.LinkDel(l)
	}
}
