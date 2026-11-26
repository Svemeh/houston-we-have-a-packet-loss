package main

import (
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

func pairUnprivileged(request pairRequest) (pairResult, error) {
	var result pairResult
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.IPPROTO_ICMP)
	if err != nil {
		return result, fmt.Errorf("open icmp socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1); err != nil {
		return result, fmt.Errorf("enable error queue: %w", err)
	}
	destination := &unix.SockaddrInet4{}
	copy(destination.Addr[:], request.target.To4())

	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, pairEchoTTL); err != nil {
		return result, fmt.Errorf("set ttl: %w", err)
	}
	echoSent := time.Now()
	if err := unix.Sendto(fd, request.echoPacket, 0, destination); err != nil {
		return result, fmt.Errorf("send: %w", err)
	}
	if request.ttl > 0 {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, request.ttl); err != nil {
			return result, fmt.Errorf("set ttl: %w", err)
		}
		if err := unix.Sendto(fd, request.popPacket, 0, destination); err != nil {
			return result, fmt.Errorf("send: %w", err)
		}
	}

	buffer := make([]byte, 1500)
	oob := make([]byte, 512)
	for !pairDone(result, request) {
		remaining := time.Until(request.deadline)
		if remaining <= 0 {
			break
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, int(remaining.Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			return result, fmt.Errorf("poll: %w", err)
		}
		for {
			n, oobn, _, _, err := unix.Recvmsg(fd, buffer, oob, unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
			if err != nil {
				break
			}
			if gateway, ok := timeExceededFrom(buffer[:n], oob[:oobn], request.target, request.popSeq); ok {
				if popGatewayRange.Contains(gateway) {
					result.popAnswered = true
				} else {
					result.popWrongHop = true
				}
			}
		}
		for {
			n, from, err := unix.Recvfrom(fd, buffer, unix.MSG_DONTWAIT)
			if err != nil {
				break
			}
			var fromIP net.IP
			if address, ok := from.(*unix.SockaddrInet4); ok {
				fromIP = net.IP(address.Addr[:])
			}
			if !result.echoAnswered && classifyPairReply(buffer[:n], fromIP, request) == pairReplyEcho {
				result.echoAnswered, result.echoRTT = true, time.Since(echoSent)
			}
		}
	}
	return result, nil
}
