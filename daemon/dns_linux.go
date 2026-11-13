//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	dnsChain   = "NORRSKEN_DNS"
	dnsFwdPort = "53535"
)

// dnsGuard makes the TV resolve names through NordVPN's DNS while connected.
//
// Preferred ("redirect"): an iptables nat rule sends every outgoing port-53
// query (from any app, to any server, including the router) to a small
// forwarder in this daemon, which asks NordVPN DNS through the tunnel.
// Fallback ("resolv"): bind-mount a resolv.conf naming NordVPN DNS over the
// system one. Original files are never modified.
type dnsGuard struct {
	mode    string
	fwd     *dnsForwarder
	ipt     string
	resolv  string
	servers []string
}

func findIPTables() string {
	for _, p := range []string{"iptables", "/usr/sbin/iptables", "/sbin/iptables", "/usr/bin/iptables", "iptables-legacy"} {
		path, err := exec.LookPath(p)
		if err != nil {
			continue
		}
		if exec.Command(path, "-t", "nat", "-S", "OUTPUT").Run() == nil {
			return path
		}
	}
	return ""
}

func ipt(bin string, args ...string) error {
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func startDNS(servers []string, u *undoLog) *dnsGuard {
	g := &dnsGuard{mode: "none", servers: servers}
	if os.Getenv("NORRSKEN_DNS_MODE") != "resolv" {
		if bin := findIPTables(); bin != "" {
			if err := g.startRedirect(bin, u); err == nil {
				return g
			} else {
				logf("dns redirect unavailable: %v", err)
			}
		}
	}
	if err := g.startResolv(u); err != nil {
		logf("dns override unavailable: %v (queries keep using the TV's DNS)", err)
	}
	return g
}

func (g *dnsGuard) startRedirect(bin string, u *undoLog) error {
	f, err := startForwarder("127.0.0.1:"+dnsFwdPort, g.servers)
	if err != nil {
		return err
	}
	removeDNSChain(bin)
	u.IPTables = bin
	u.save()
	rules := [][]string{
		{"-t", "nat", "-N", dnsChain},
		{"-t", "nat", "-A", dnsChain, "-d", "127.0.0.0/8", "-j", "RETURN"},
	}
	for _, s := range allNordDNS {
		rules = append(rules, []string{"-t", "nat", "-A", dnsChain, "-d", s, "-j", "RETURN"})
	}
	rules = append(rules,
		[]string{"-t", "nat", "-A", dnsChain, "-p", "udp", "--dport", "53", "-j", "REDIRECT", "--to-ports", dnsFwdPort},
		[]string{"-t", "nat", "-A", dnsChain, "-p", "tcp", "--dport", "53", "-j", "REDIRECT", "--to-ports", dnsFwdPort},
		[]string{"-t", "nat", "-I", "OUTPUT", "1", "-j", dnsChain},
	)
	for _, r := range rules {
		if err := ipt(bin, r...); err != nil {
			f.close()
			removeDNSChain(bin)
			u.IPTables = ""
			u.save()
			return err
		}
	}
	g.mode, g.fwd, g.ipt = "redirect", f, bin
	return nil
}

func removeDNSChain(bin string) {
	for exec.Command(bin, "-t", "nat", "-D", "OUTPUT", "-j", dnsChain).Run() == nil {
	}
	exec.Command(bin, "-t", "nat", "-F", dnsChain).Run()
	exec.Command(bin, "-t", "nat", "-X", dnsChain).Run()
}

func resolvContent(servers []string) []byte {
	var b strings.Builder
	b.WriteString("# written by norrsken while the VPN is connected\n")
	for _, s := range servers {
		b.WriteString("nameserver " + s + "\n")
	}
	b.WriteString("options timeout:2 attempts:2\n")
	return []byte(b.String())
}

func (g *dnsGuard) startResolv(u *undoLog) error {
	target, err := filepath.EvalSymlinks("/etc/resolv.conf")
	if err != nil {
		return err
	}
	src := filepath.Join(runDir, "resolv.conf")
	if err := os.WriteFile(src, resolvContent(g.servers), 0o644); err != nil {
		return err
	}
	// If we were started inside a private mount namespace, a mount made here
	// would be invisible to the TV's apps; make it in init's namespace.
	if !sameMountNS() {
		if ns, err := exec.LookPath("nsenter"); err == nil {
			out, err := exec.Command(ns, "-t", "1", "-m", "--", "mount", "--bind", src, target).CombinedOutput()
			if err == nil {
				exec.Command(ns, "-t", "1", "-m", "--", "mount", "-o", "remount,bind,ro", target).Run()
				u.Resolv, u.ResolvInit = target, true
				u.save()
				g.mode, g.resolv = "resolv", target
				return nil
			}
			logf("nsenter mount failed: %v %s", err, strings.TrimSpace(string(out)))
		} else {
			logf("warning: separate mount namespace and no nsenter; DNS override may only apply to norrsken")
		}
	}
	if err := unix.Mount(src, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind mount over %s: %w", target, err)
	}
	unix.Mount("", target, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, "")
	u.Resolv = target
	u.save()
	g.mode, g.resolv = "resolv", target
	return nil
}

func sameMountNS() bool {
	a, err1 := os.Readlink("/proc/1/ns/mnt")
	b, err2 := os.Readlink("/proc/self/ns/mnt")
	return err1 != nil || err2 != nil || a == b
}

func unmountResolv(target string, inInit bool) {
	if inInit {
		if ns, err := exec.LookPath("nsenter"); err == nil {
			exec.Command(ns, "-t", "1", "-m", "--", "umount", "-l", target).Run()
			return
		}
	}
	unix.Unmount(target, unix.MNT_DETACH)
}

func (g *dnsGuard) update(servers []string) {
	g.servers = servers
	switch g.mode {
	case "redirect":
		g.fwd.setUpstreams(servers)
	case "resolv":
		// The mount source is our own file; rewrite it in place.
		os.WriteFile(filepath.Join(runDir, "resolv.conf"), resolvContent(servers), 0o644)
	}
}

func (g *dnsGuard) stop() {
	if g.fwd != nil {
		g.fwd.close()
	}
	// iptables chain and mount are reverted by undoAll from the journal.
}
