package main

import (
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

const icmpTimeExceeded = 11

func probeUnprivileged(request probeRequest) (net.IP, time.Duration, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.IPPROTO_ICMP)
	if err != nil {
		return nil, 0, fmt.Errorf("open icmp socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, request.ttl); err != nil {
		return nil, 0, fmt.Errorf("set ttl: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1); err != nil {
		return nil, 0, fmt.Errorf("enable error queue: %w", err)
	}

	destination := &unix.SockaddrInet4{}
	copy(destination.Addr[:], request.target.To4())
	sent := time.Now()
	if err := unix.Sendto(fd, request.packet, 0, destination); err != nil {
		return nil, 0, fmt.Errorf("send: %w", err)
	}

	buffer := make([]byte, 1500)
	oob := make([]byte, 512)
	for {
		remaining := time.Until(request.deadline)
		if remaining <= 0 {
			return nil, 0, errNoAnswer
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, int(remaining.Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			return nil, 0, fmt.Errorf("poll: %w", err)
		}
		for {
			n, oobn, _, _, err := unix.Recvmsg(fd, buffer, oob, unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
			if err != nil {
				break
			}
			if gateway, ok := timeExceededFrom(buffer[:n], oob[:oobn], request.target, request.seq); ok {
				return gateway, time.Since(sent), nil
			}
		}
		for {
			if _, _, err := unix.Recvfrom(fd, buffer, unix.MSG_DONTWAIT); err != nil {
				break
			}
		}
	}
}

func timeExceededFrom(payload, oob []byte, target net.IP, seq int) (net.IP, bool) {
	if !isOurEcho(payload, seq) && !quotesOurEcho(payload, target, seq) {
		return nil, false
	}
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, false
	}
	for _, message := range messages {
		if message.Header.Level != unix.IPPROTO_IP || message.Header.Type != unix.IP_RECVERR {
			continue
		}
		data := message.Data
		if len(data) < 24 || data[4] != unix.SO_EE_ORIGIN_ICMP || data[5] != icmpTimeExceeded {
			continue
		}
		return net.IPv4(data[20], data[21], data[22], data[23]).To4(), true
	}
	return nil, false
}
