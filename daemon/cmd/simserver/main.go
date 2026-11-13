// Command simserver is a userspace WireGuard "NordVPN server" for the Docker
// integration test (the Docker Desktop kernel has no WireGuard netlink API).
//
//	simserver genkey            print a new private key (base64)
//	simserver pubkey <priv>     print the public key
//	simserver run <priv> <client-pub>
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/signal"

	"github.com/vishvananda/netlink"
	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func hexKey(b64 string) string {
	b, err := base64.StdEncoding.DecodeString(b64)
	must(err)
	return hex.EncodeToString(b)
}

func main() {
	switch os.Args[1] {
	case "genkey":
		k := make([]byte, 32)
		rand.Read(k)
		k[0] &= 248
		k[31] = (k[31] & 127) | 64
		fmt.Println(base64.StdEncoding.EncodeToString(k))
	case "pubkey":
		k, err := base64.StdEncoding.DecodeString(os.Args[2])
		must(err)
		p, err := curve25519.X25519(k, curve25519.Basepoint)
		must(err)
		fmt.Println(base64.StdEncoding.EncodeToString(p))
	case "run":
		t, err := tun.CreateTUN("wg0", 1420)
		must(err)
		d := device.NewDevice(t, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "sim: "))
		must(d.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=51820\npublic_key=%s\nallowed_ip=10.5.0.2/32\n", hexKey(os.Args[2]), hexKey(os.Args[3]))))
		must(d.Up())
		l, err := netlink.LinkByName("wg0")
		must(err)
		a, _ := netlink.ParseAddr("10.5.0.1/16")
		must(netlink.AddrAdd(l, a))
		must(netlink.LinkSetUp(l))
		_ = net.IPv4zero
		fmt.Println("sim up")
		s := make(chan os.Signal, 1)
		signal.Notify(s, os.Interrupt)
		<-s
	}
}
