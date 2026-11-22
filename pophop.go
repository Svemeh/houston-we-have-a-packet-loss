package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

const (
	PopHopKey             = "pop"
	PopTarget             = HopEndTarget
	PopMaxTTL             = 5
	PopDiscoveryAttempts  = 3
	PopRediscoverInterval = 10 * time.Minute
)

var popGatewayRange = mustParseCIDR("100.64.0.0/10")

func mustParseCIDR(cidr string) *net.IPNet {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return network
}

type PopProber struct {
	target  net.IP
	ttl     atomic.Int32
	gateway atomic.Value
	seq     atomic.Uint32
	id      int
}

func NewPopProber() *PopProber {
	return &PopProber{target: net.ParseIP(PopTarget).To4(), id: os.Getpid() & 0xffff}
}

func (prober *PopProber) Gateway() string {
	gateway, _ := prober.gateway.Load().(string)
	return gateway
}

func (prober *PopProber) Discover(ctx context.Context) bool {
	for ttl := 1; ttl <= PopMaxTTL; ttl++ {
		for attempt := 0; attempt < PopDiscoveryAttempts; attempt++ {
			if ctx.Err() != nil {
				return false
			}
			from, _, err := prober.probe(ctx, ttl)
			if err != nil {
				if errors.Is(err, errNoAnswer) {
					continue
				}
				log.Printf("pop hop: %v", err)
				return false
			}
			if popGatewayRange.Contains(from) {
				prober.ttl.Store(int32(ttl))
				prober.gateway.Store(from.String())
				return true
			}
			break
		}
	}
	return false
}

func (prober *PopProber) PingOnce(ctx context.Context) PingSample {
	sample := PingSample{Timestamp: time.Now(), Target: PopHopKey}
	from, rtt, err := prober.probe(ctx, int(prober.ttl.Load()))
	if err != nil {
		sample.Lost = true
		if !errors.Is(err, errNoAnswer) {
			sample.Error = err.Error()
		}
		return sample
	}
	if gateway := from.String(); gateway != prober.Gateway() {
		log.Printf("pop hop: gateway is now %s (was %s)", gateway, prober.Gateway())
		prober.gateway.Store(gateway)
	}
	sample.RTTMs = float64(rtt.Microseconds()) / 1000
	return sample
}

func (prober *PopProber) KeepCurrent(ctx context.Context, hopPinger *HopPinger) {
	added := false
	for {
		if prober.Discover(ctx) {
			if !added {
				log.Printf("pop hop: probing %s, %d hops out toward %s", prober.Gateway(), prober.ttl.Load(), PopTarget)
				hopPinger.Add(prober)
				added = true
			}
		} else if !added {
			log.Printf("pop hop: no Starlink gateway (%s) answered within %d hops; trying again in %s", popGatewayRange, PopMaxTTL, PopRediscoverInterval)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(PopRediscoverInterval):
		}
	}
}

var errNoAnswer = errors.New("no answer")

func listenICMP() (*icmp.PacketConn, bool, error) {
	if conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0"); err == nil {
		return conn, true, nil
	}
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	return conn, false, err
}

func (prober *PopProber) probe(ctx context.Context, ttl int) (net.IP, time.Duration, error) {
	conn, raw, err := listenICMP()
	if err != nil {
		return nil, 0, fmt.Errorf("open icmp socket: %w", err)
	}
	defer conn.Close()
	if err := conn.IPv4PacketConn().SetTTL(ttl); err != nil {
		return nil, 0, fmt.Errorf("set ttl: %w", err)
	}

	seq := int(prober.seq.Add(1) & 0xffff)
	message := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: prober.id, Seq: seq, Data: []byte(AppName)}}
	packet, err := message.Marshal(nil)
	if err != nil {
		return nil, 0, err
	}
	var destination net.Addr = &net.UDPAddr{IP: prober.target}
	if raw {
		destination = &net.IPAddr{IP: prober.target}
	}

	deadline := time.Now().Add(PingTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	sent := time.Now()
	if _, err := conn.WriteTo(packet, destination); err != nil {
		return nil, 0, fmt.Errorf("send: %w", err)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, 0, err
	}

	buffer := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(buffer)
		if err != nil {
			return nil, 0, errNoAnswer
		}
		reply, err := icmp.ParseMessage(1, buffer[:n])
		if err != nil || reply.Type != ipv4.ICMPTypeTimeExceeded {
			continue
		}
		exceeded, ok := reply.Body.(*icmp.TimeExceeded)
		if !ok || !quotesOurEcho(exceeded.Data, prober.target, seq) {
			continue
		}
		return addrIP(peer), time.Since(sent), nil
	}
}

func quotesOurEcho(quoted []byte, target net.IP, seq int) bool {
	if len(quoted) < 20 {
		return false
	}
	headerLength := int(quoted[0]&0x0f) * 4
	if headerLength < 20 || len(quoted) < headerLength+8 {
		return false
	}
	if !net.IP(quoted[16:20]).Equal(target) {
		return false
	}
	echo := quoted[headerLength:]
	return echo[0] == byte(ipv4.ICMPTypeEcho) && int(echo[6])<<8|int(echo[7]) == seq
}

func addrIP(addr net.Addr) net.IP {
	switch peer := addr.(type) {
	case *net.UDPAddr:
		return peer.IP
	case *net.IPAddr:
		return peer.IP
	}
	return nil
}
