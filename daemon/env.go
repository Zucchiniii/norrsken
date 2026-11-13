package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

type envReport struct {
	Version   string   `json:"version"`
	Build     string   `json:"build"`
	UID       int      `json:"uid"`
	Arch      string   `json:"arch"`
	Machine   string   `json:"machine"`
	Kernel    string   `json:"kernel"`
	WebOS     string   `json:"webos,omitempty"`
	Firmware  string   `json:"firmware,omitempty"`
	Model     string   `json:"model,omitempty"`
	TUN       bool     `json:"tun"`
	IPTables  string   `json:"iptables"`
	Resolv    string   `json:"resolv"`
	DNS       []string `json:"nameservers"`
	Uplink    bool     `json:"uplink"`
	LANIP     string   `json:"lanIp,omitempty"`
	Autostart bool     `json:"autostart"`
	MountNS   string   `json:"mountNs"`
	Binary    string   `json:"binary"`
	StateDir  string   `json:"stateDir"`
}

func collectEnv() envReport {
	e := envReport{Version: version, Build: buildID, UID: os.Geteuid(), Arch: runtime.GOARCH, StateDir: stateDir}
	var u unix.Utsname
	if unix.Uname(&u) == nil {
		e.Machine = unix.ByteSliceToString(u.Machine[:])
		e.Kernel = unix.ByteSliceToString(u.Release[:])
	}
	if _, err := os.Stat("/dev/net/tun"); err == nil {
		e.TUN = true
	}
	e.IPTables = "missing"
	if p, err := exec.LookPath("iptables"); err == nil {
		e.IPTables = p + " (no nat table)"
		if exec.Command(p, "-t", "nat", "-S", "OUTPUT").Run() == nil {
			e.IPTables = p
		}
	}
	if t, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil {
		e.Resolv = t
	}
	if f, err := os.Open("/etc/resolv.conf"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if fs := strings.Fields(sc.Text()); len(fs) >= 2 && fs[0] == "nameserver" {
				e.DNS = append(e.DNS, fs[1])
			}
		}
		f.Close()
	}
	e.Uplink = hasUplink()
	e.LANIP = lanIP()
	if _, err := os.Stat(autostartPath()); err == nil {
		e.Autostart = true
	}
	e.Binary, _ = os.Executable()
	e.MountNS = "init"
	if a, err := os.Readlink("/proc/1/ns/mnt"); err == nil {
		if b, err := os.Readlink("/proc/self/ns/mnt"); err == nil && a != b {
			e.MountNS = "separate"
		}
	} else {
		e.MountNS = "unknown"
	}

	// webOS reports its version through nyx; the file exists on most models.
	if b, err := os.ReadFile("/var/run/nyx/os_info.json"); err == nil {
		var oi map[string]any
		if json.Unmarshal(b, &oi) == nil {
			e.WebOS = str(oi["webos_release"])
			if n := str(oi["webos_name"]); n != "" && e.WebOS != "" {
				e.WebOS = n + " " + e.WebOS
			}
		}
	}
	if b, err := os.ReadFile("/var/run/nyx/device_info.json"); err == nil {
		var di map[string]any
		if json.Unmarshal(b, &di) == nil {
			e.Model = str(di["device_name"])
		}
	}
	if b, err := os.ReadFile("/var/run/nyx/os_info.json"); err == nil {
		var oi map[string]any
		if json.Unmarshal(b, &oi) == nil {
			e.Firmware = str(oi["core_os_release"])
		}
	}
	return e
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// hasUplink reports whether an IPv4 default route exists outside our tunnel.
func hasUplink() bool {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return runtime.GOOS != "linux"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) > 7 && fs[0] != "nordlynx" && fs[1] == "00000000" && fs[7] == "00000000" {
			return true
		}
	}
	return false
}
