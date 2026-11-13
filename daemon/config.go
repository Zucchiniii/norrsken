package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Target is what the user asked to connect to. Kind is "quick" (NordVPN's
// recommendation for this location), "country" or "city".
type Target struct {
	Kind      string `json:"kind"`
	CountryID int    `json:"countryId,omitempty"`
	CityID    int    `json:"cityId,omitempty"`
	Label     string `json:"label,omitempty"`
	Code      string `json:"code,omitempty"`
}

func (t Target) key() string { return fmt.Sprintf("%s/%d/%d", t.Kind, t.CountryID, t.CityID) }

type Settings struct {
	AutoConnect      *bool `json:"autoConnect,omitempty"`
	KillSwitch       *bool `json:"killSwitch,omitempty"`
	ThreatProtection *bool `json:"threatProtection,omitempty"`
}

func boolp(b bool) *bool { return &b }

func (s Settings) autoConnect() bool      { return s.AutoConnect != nil && *s.AutoConnect }
func (s Settings) killSwitch() bool       { return s.KillSwitch != nil && *s.KillSwitch }
func (s Settings) threatProtection() bool { return s.ThreatProtection != nil && *s.ThreatProtection }

func (s *Settings) merge(o Settings) {
	if o.AutoConnect != nil {
		s.AutoConnect = o.AutoConnect
	}
	if o.KillSwitch != nil {
		s.KillSwitch = o.KillSwitch
	}
	if o.ThreatProtection != nil {
		s.ThreatProtection = o.ThreatProtection
	}
}

type Config struct {
	Token      string   `json:"token,omitempty"`
	PrivateKey string   `json:"privateKey,omitempty"`
	LinkedAt   int64    `json:"linkedAt,omitempty"`
	Settings   Settings `json:"settings"`
	Last       *Target  `json:"last,omitempty"`
	Recents    []Target `json:"recents,omitempty"`
	// Country NordVPN placed us in when we last asked from outside the
	// tunnel; used so "fastest" keeps meaning "fastest near home".
	HomeCountry int `json:"homeCountry,omitempty"`
}

func (c Config) homeCountry() int { return c.HomeCountry }

func configPath() string { return filepath.Join(stateDir, "config.json") }

func loadConfig() Config {
	var c Config
	b, err := os.ReadFile(configPath())
	if err == nil {
		json.Unmarshal(b, &c)
	}
	if c.Settings.ThreatProtection == nil {
		c.Settings.ThreatProtection = boolp(false)
	}
	return c
}

func saveConfig(c Config) error {
	return writeFileAtomic(configPath(), c, 0o600)
}

func writeFileAtomic(path string, v any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) remember(t Target) {
	c.Last = &t
	out := []Target{t}
	for _, r := range c.Recents {
		if r.key() != t.key() && len(out) < 6 {
			out = append(out, r)
		}
	}
	c.Recents = out
}

// ---- logging: stderr (the log file) plus an in-memory tail for the UI ----

var logRing = struct {
	sync.Mutex
	lines []string
}{}

func logf(format string, args ...any) {
	line := time.Now().Format("01-02 15:04:05 ") + fmt.Sprintf(format, args...)
	line = redact(line)
	fmt.Fprintln(os.Stderr, line)
	logRing.Lock()
	logRing.lines = append(logRing.lines, line)
	if len(logRing.lines) > 400 {
		logRing.lines = logRing.lines[len(logRing.lines)-400:]
	}
	logRing.Unlock()
}

func logTail(n int) []string {
	logRing.Lock()
	defer logRing.Unlock()
	if n <= 0 || n > len(logRing.lines) {
		n = len(logRing.lines)
	}
	out := make([]string, n)
	copy(out, logRing.lines[len(logRing.lines)-n:])
	return out
}

var secrets = struct {
	sync.Mutex
	list []string
}{}

func addSecret(s string) {
	if len(s) < 8 {
		return
	}
	secrets.Lock()
	secrets.list = append(secrets.list, s)
	secrets.Unlock()
}

func redact(s string) string {
	secrets.Lock()
	defer secrets.Unlock()
	for _, x := range secrets.list {
		s = strings.ReplaceAll(s, x, "[redacted]")
	}
	return s
}
