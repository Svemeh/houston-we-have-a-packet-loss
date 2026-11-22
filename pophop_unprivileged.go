//go:build !linux

package main

import (
	"fmt"
	"net"
	"time"

	"golang.org/x/net/icmp"
)

func probeUnprivileged(request probeRequest) (net.IP, time.Duration, error) {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return nil, 0, fmt.Errorf("open icmp socket: %w", err)
	}
	defer conn.Close()
	return exchange(conn, &net.UDPAddr{IP: request.target}, request)
}
