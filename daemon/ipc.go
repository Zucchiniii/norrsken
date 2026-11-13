package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type reply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Data  any    `json:"data,omitempty"`
}

type request struct {
	Cmd string `json:"cmd"`
	// Arguments; only the ones a command uses are set.
	Token     string    `json:"token,omitempty"`
	Target    *Target   `json:"target,omitempty"`
	Settings  *Settings `json:"settings,omitempty"`
	LogLines  int       `json:"lines,omitempty"`
	ForceSync bool      `json:"refresh,omitempty"`
	Resume    bool      `json:"resume,omitempty"`
}

// clientRequest sends one request to the daemon. When autostart is set and
// no daemon answers, it launches one and retries.
func clientRequest(raw []byte, autostart bool) reply {
	var probe request
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Cmd == "" {
		return reply{Error: "bad request"}
	}
	if autostart && probe.Cmd != "shutdown" {
		replaceOutdatedDaemon()
	}
	r, err := roundTrip(raw, 45*time.Second)
	if err == nil {
		return r
	}
	if !autostart || probe.Cmd == "shutdown" {
		return reply{Error: "daemon not running"}
	}
	if err := spawnDaemon(false); err != nil {
		return reply{Error: "could not start daemon: " + err.Error()}
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		if r, err := roundTrip(raw, 45*time.Second); err == nil {
			return r
		}
	}
	return reply{Error: "daemon did not start; see " + logPath()}
}

// replaceOutdatedDaemon stops a running daemon that was started from an older
// build. Installing an update swaps the binary on disk, but the daemon keeps
// running the old code (it never idles out while the app polls it), so
// without this the TV would keep serving old behaviour until a reboot.
// Daemons from before build IDs existed report none and are replaced too.
func replaceOutdatedDaemon() {
	r, err := roundTrip([]byte(`{"cmd":"ping"}`), 3*time.Second)
	if err != nil || !r.OK {
		return // not running; the normal path starts the current build
	}
	if m, ok := r.Data.(map[string]any); ok {
		if b, _ := m["build"].(string); b == buildID {
			return
		}
	}
	roundTrip([]byte(`{"cmd":"shutdown","resume":true}`), 10*time.Second)
	for i := 0; i < 100; i++ { // wait for it to let go of the socket
		if _, err := roundTrip([]byte(`{"cmd":"ping"}`), 300*time.Millisecond); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func roundTrip(raw []byte, timeout time.Duration) (reply, error) {
	c, err := net.DialTimeout("unix", socketPath(), 2*time.Second)
	if err != nil {
		return reply{}, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write(append(raw, '\n')); err != nil {
		return reply{}, err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	if !sc.Scan() {
		if sc.Err() != nil {
			return reply{}, sc.Err()
		}
		return reply{}, errors.New("empty reply")
	}
	var r reply
	if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
		return reply{}, err
	}
	return r, nil
}

// spawnDaemon starts "norrsken daemon" fully detached from the caller.
//
// The Homebrew Channel exec service runs our command through a shell that
// inherits Luna bus sockets. A daemon that keeps those descriptors open can
// wedge the service, so every inherited descriptor above stderr is marked
// close-on-exec before we fork, and stdio goes to the log file.
func spawnDaemon(boot bool) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	rotateLog()
	if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
		for _, e := range ents {
			if fd, err := strconv.Atoi(e.Name()); err == nil && fd > 2 {
				syscall.CloseOnExec(fd)
			}
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	args := []string{"daemon"}
	if boot {
		args = append(args, "--boot")
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = "/"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func rotateLog() {
	p := logPath()
	if fi, err := os.Stat(p); err == nil && fi.Size() > 1<<20 {
		os.Rename(p, p+".old")
	}
}

// serveControl accepts newline-delimited JSON requests on the unix socket.
func serveControl(ln net.Listener, handle func(request) reply) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			sc := bufio.NewScanner(c)
			sc.Buffer(make([]byte, 4096), 1<<20)
			if !sc.Scan() {
				return
			}
			var req request
			var r reply
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				r = reply{Error: "bad request"}
			} else {
				r = safeHandle(handle, req)
			}
			b, _ := json.Marshal(r)
			c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			c.Write(append(b, '\n'))
		}(c)
	}
}

func safeHandle(handle func(request) reply, req request) (r reply) {
	defer func() {
		if p := recover(); p != nil {
			logf("panic in %s: %v", req.Cmd, p)
			r = reply{Error: "internal error"}
		}
	}()
	return handle(req)
}

func listenControl() (net.Listener, error) {
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, err
	}
	p := socketPath()
	os.Remove(p)
	ln, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	os.Chmod(p, 0o600)
	return ln, nil
}

// lockSingleInstance makes sure only one daemon owns the tunnel.
func lockSingleInstance() (*os.File, error) {
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(runDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// A daemon that is shutting down for an update may still hold the lock
	// for a moment, so wait a little before giving up.
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		if time.Now().After(deadline) {
			f.Close()
			return nil, errors.New("another norrsken daemon is running")
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.Truncate(0)
	f.WriteString(strconv.Itoa(os.Getpid()))
	return f, nil
}
