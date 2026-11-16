package main

import (
	"net"
	"net/netip"
)

func listenTCP(address string) (net.Listener, error) {
	network := "tcp"
	if host, _, err := net.SplitHostPort(address); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil && ip.Is4() {
			network = "tcp4"
		}
	}
	return net.Listen(network, address)
}
