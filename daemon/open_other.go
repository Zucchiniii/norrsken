//go:build !linux

package main

import "errors"

type tunnelOpts struct {
	PrivateKeyHex string
	KillSwitch    bool
	DNS           []string
}

func openTunnel(s Server, o tunnelOpts) (tunnelAPI, error) {
	if fakeTunnelEnabled() {
		return newFakeTunnel(s), nil
	}
	return nil, errors.New("tunnels are only supported on Linux (set NORRSKEN_FAKE_TUNNEL=1 for UI work)")
}

func recoverLeftovers() {}
