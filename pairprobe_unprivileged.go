//go:build !linux

package main

import (
	"fmt"
	"net"

	"golang.org/x/net/icmp"
)

func pairUnprivileged(request pairRequest) (pairResult, error) {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return pairResult{}, fmt.Errorf("open icmp socket: %w", err)
	}
	defer conn.Close()
	return exchangePair(conn, &net.UDPAddr{IP: request.target}, request)
}
