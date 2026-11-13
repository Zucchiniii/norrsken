//go:build linux

package main

func openTunnel(s Server, o tunnelOpts) (tunnelAPI, error) {
	if fakeTunnelEnabled() {
		return newFakeTunnel(s), nil
	}
	t, err := bringUp(s, o)
	if err != nil {
		return nil, err
	}
	return t, nil
}
