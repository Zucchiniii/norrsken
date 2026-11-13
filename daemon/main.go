// Command norrsken is the root-side half of the Norrsken webOS app: a
// NordVPN (NordLynx/WireGuard) client for rooted LG TVs.
//
// The TV app never touches the network stack itself. It runs
//
//	norrsken ctl <base64 JSON request>
//
// through the Homebrew Channel root exec service. ctl forwards the request to
// a long-running "norrsken daemon" over a unix socket (starting the daemon
// first if needed) and prints the JSON reply on stdout.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

var version = "0.1.0"

// buildID is stamped by build.sh. A ctl client that finds a daemon from a
// different build (the app was updated while it ran) restarts it.
var buildID = "dev"

const appID = "com.zucchiniii.norrsken"

// Paths can be overridden for tests on a normal Linux box.
var (
	stateDir = envOr("NORRSKEN_STATE", "/var/lib/webosbrew/norrsken")
	runDir   = envOr("NORRSKEN_RUN", "/var/run/norrsken")
	initDir  = envOr("NORRSKEN_INITD", "/var/lib/webosbrew/init.d")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func socketPath() string { return filepath.Join(runDir, "ctl.sock") }
func logPath() string    { return filepath.Join(stateDir, "norrsken.log") }

func usage() {
	fmt.Fprintf(os.Stderr, `norrsken %s — NordVPN for rooted webOS

usage:
  norrsken ctl <base64-json>   send a request to the daemon (starts it if needed)
  norrsken ctl-json '<json>'   same, with plain JSON (for SSH debugging)
  norrsken daemon [--boot]     run the daemon in the foreground
  norrsken boot                start the daemon detached and auto-connect (init.d)
  norrsken stop                disconnect and stop the daemon
  norrsken diag                print an environment report
  norrsken version
`, version)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "ctl", "ctl-json":
		if len(os.Args) < 3 {
			printJSON(reply{OK: false, Error: "missing request"})
			return
		}
		raw := []byte(os.Args[2])
		if os.Args[1] == "ctl" {
			dec, err := base64.StdEncoding.DecodeString(os.Args[2])
			if err != nil {
				printJSON(reply{OK: false, Error: "bad base64 request"})
				return
			}
			raw = dec
		}
		printJSON(clientRequest(raw, true))
	case "daemon":
		boot := len(os.Args) > 2 && os.Args[2] == "--boot"
		if err := runDaemon(boot); err != nil {
			fmt.Fprintln(os.Stderr, "daemon:", err)
			os.Exit(1)
		}
	case "boot":
		// Called from /var/lib/webosbrew/init.d at TV start-up.
		if r := clientRequest([]byte(`{"cmd":"ping"}`), false); r.OK {
			clientRequest([]byte(`{"cmd":"bootConnect"}`), false)
			return
		}
		if err := spawnDaemon(true); err != nil {
			fmt.Fprintln(os.Stderr, "boot:", err)
			os.Exit(1)
		}
	case "stop":
		printJSON(clientRequest([]byte(`{"cmd":"shutdown"}`), false))
	case "diag":
		b, _ := json.MarshalIndent(collectEnv(), "", "  ")
		fmt.Println(string(b))
	case "version", "--version", "-v":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func printJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"ok":false,"error":"encode failed"}`)
	}
	os.Stdout.Write(append(b, '\n'))
}
