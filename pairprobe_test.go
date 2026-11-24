package main

import (
	"net"
	"testing"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

func TestClassifyPairReply(t *testing.T) {
	target := net.ParseIP("1.1.1.1").To4()
	token := []byte("12345678")
	request := pairRequest{target: target, ttl: 2, popSeq: 10, echoSeq: 11, token: token}
	marshal := func(message icmp.Message) []byte {
		bytes, err := message.Marshal(nil)
		if err != nil {
			t.Fatal(err)
		}
		return bytes
	}
	echoReply := func(seq int, data []byte) []byte {
		return marshal(icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: 1, Seq: seq, Data: data}})
	}
	exceeded := func(seq int) []byte {
		return marshal(icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: quotedEcho(target, seq, 20)}})
	}
	gateway := net.ParseIP("100.64.0.1")
	cases := []struct {
		name    string
		message []byte
		from    net.IP
		want    int
	}{
		{"pop answer", exceeded(10), gateway, pairReplyPoP},
		{"answer from a hop that is not the PoP", exceeded(10), net.ParseIP("192.168.2.1"), pairReplyWrongHop},
		{"time exceeded for another probe", exceeded(11), gateway, pairReplyNone},
		{"echo reply", echoReply(11, append(token, "houston"...)), target, pairReplyEcho},
		{"echo reply without our token", echoReply(11, []byte("someone else")), target, pairReplyNone},
		{"echo reply with the pop seq", echoReply(10, token), target, pairReplyNone},
		{"echo reply from elsewhere", echoReply(11, token), net.ParseIP("8.8.8.8"), pairReplyNone},
		{"garbage", []byte{1, 2}, target, pairReplyNone},
	}
	for _, test := range cases {
		if got := classifyPairReply(test.message, test.from, request); got != test.want {
			t.Errorf("%s: got %d, want %d", test.name, got, test.want)
		}
	}
}
