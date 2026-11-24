package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

const pairEchoTTL = 64

type pairRequest struct {
	target     net.IP
	ttl        int
	popSeq     int
	echoSeq    int
	popPacket  []byte
	echoPacket []byte
	token      []byte
	deadline   time.Time
}

type pairResult struct {
	popAnswered  bool
	echoAnswered bool
	echoRTT      time.Duration
}

const (
	pairReplyNone = iota
	pairReplyPoP
	pairReplyEcho
)

func (prober *PopProber) PingPair(ctx context.Context, target string) (PingSample, error) {
	sample := PingSample{Timestamp: time.Now(), Target: target}
	ip := net.ParseIP(target).To4()
	if ip == nil {
		return sample, fmt.Errorf("not an IPv4 address: %s", target)
	}
	request := pairRequest{target: ip, ttl: int(prober.ttl.Load()), token: prober.token}
	request.popSeq = int(prober.seq.Add(1) & 0xffff)
	request.echoSeq = int(prober.seq.Add(1) & 0xffff)
	var err error
	if request.popPacket, err = prober.echoPacket(request.popSeq, []byte(AppName)); err != nil {
		return sample, err
	}
	if request.echoPacket, err = prober.echoPacket(request.echoSeq, append(append([]byte(nil), prober.token...), AppName...)); err != nil {
		return sample, err
	}
	request.deadline = time.Now().Add(PingTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(request.deadline) {
		request.deadline = ctxDeadline
	}

	var result pairResult
	if conn, rawErr := icmp.ListenPacket("ip4:icmp", "0.0.0.0"); rawErr == nil {
		defer conn.Close()
		result, err = exchangePair(conn, &net.IPAddr{IP: ip}, request)
	} else {
		result, err = pairUnprivileged(request)
	}
	if err != nil {
		return sample, err
	}
	if request.ttl > 0 {
		answered := result.popAnswered
		sample.PoPUp = &answered
	}
	if !result.echoAnswered {
		sample.Lost = true
		return sample, nil
	}
	sample.RTTMs = float64(result.echoRTT.Microseconds()) / 1000
	return sample, nil
}

func (prober *PopProber) echoPacket(seq int, data []byte) ([]byte, error) {
	message := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: prober.id, Seq: seq, Data: data}}
	return message.Marshal(nil)
}

func exchangePair(conn *icmp.PacketConn, destination net.Addr, request pairRequest) (pairResult, error) {
	var result pairResult
	packetConn := conn.IPv4PacketConn()
	if err := packetConn.SetTTL(pairEchoTTL); err != nil {
		return result, fmt.Errorf("set ttl: %w", err)
	}
	echoSent := time.Now()
	if _, err := conn.WriteTo(request.echoPacket, destination); err != nil {
		return result, fmt.Errorf("send: %w", err)
	}
	if request.ttl > 0 {
		if err := packetConn.SetTTL(request.ttl); err != nil {
			return result, fmt.Errorf("set ttl: %w", err)
		}
		if _, err := conn.WriteTo(request.popPacket, destination); err != nil {
			return result, fmt.Errorf("send: %w", err)
		}
	}
	if err := conn.SetReadDeadline(request.deadline); err != nil {
		return result, err
	}

	buffer := make([]byte, 1500)
	for !pairDone(result, request) {
		n, peer, err := conn.ReadFrom(buffer)
		if err != nil {
			break
		}
		switch classifyPairReply(buffer[:n], addrIP(peer), request) {
		case pairReplyPoP:
			result.popAnswered = true
		case pairReplyEcho:
			if !result.echoAnswered {
				result.echoAnswered, result.echoRTT = true, time.Since(echoSent)
			}
		}
	}
	return result, nil
}

func pairDone(result pairResult, request pairRequest) bool {
	return result.echoAnswered && (request.ttl == 0 || result.popAnswered)
}

func classifyPairReply(message []byte, from net.IP, request pairRequest) int {
	reply, err := icmp.ParseMessage(1, message)
	if err != nil {
		return pairReplyNone
	}
	switch body := reply.Body.(type) {
	case *icmp.TimeExceeded:
		if reply.Type == ipv4.ICMPTypeTimeExceeded && quotesOurEcho(body.Data, request.target, request.popSeq) {
			return pairReplyPoP
		}
	case *icmp.Echo:
		if reply.Type == ipv4.ICMPTypeEchoReply && body.Seq == request.echoSeq && from.Equal(request.target) && bytes.HasPrefix(body.Data, request.token) {
			return pairReplyEcho
		}
	}
	return pairReplyNone
}
