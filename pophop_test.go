package main

import (
	"net"
	"testing"
)

func quotedEcho(target net.IP, seq int, headerLength int) []byte {
	quoted := make([]byte, headerLength+8)
	quoted[0] = 0x40 | byte(headerLength/4)
	copy(quoted[16:20], target.To4())
	quoted[headerLength] = 8
	quoted[headerLength+6] = byte(seq >> 8)
	quoted[headerLength+7] = byte(seq)
	return quoted
}

func TestQuotesOurEcho(t *testing.T) {
	target := net.ParseIP("1.1.1.1")
	cases := []struct {
		name   string
		quoted []byte
		want   bool
	}{
		{"match", quotedEcho(target, 513, 20), true},
		{"ip options", quotedEcho(target, 7, 24), true},
		{"other seq", quotedEcho(target, 514, 20), false},
		{"other target", quotedEcho(net.ParseIP("8.8.8.8"), 513, 20), false},
		{"truncated", quotedEcho(target, 513, 20)[:24], false},
		{"empty", nil, false},
	}
	for _, testCase := range cases {
		seq := 513
		if testCase.name == "ip options" {
			seq = 7
		}
		if got := quotesOurEcho(testCase.quoted, target.To4(), seq); got != testCase.want {
			t.Errorf("%s: got %v, want %v", testCase.name, got, testCase.want)
		}
	}

	notEcho := quotedEcho(target, 513, 20)
	notEcho[20] = 0
	if quotesOurEcho(notEcho, target.To4(), 513) {
		t.Error("echo reply quoted as our echo")
	}
}

func TestPopGatewayRange(t *testing.T) {
	for address, want := range map[string]bool{
		"100.64.0.1":     true,
		"100.127.255.1":  true,
		"100.128.0.1":    false,
		"192.168.100.1":  false,
		"100.63.255.255": false,
	} {
		if got := popGatewayRange.Contains(net.ParseIP(address)); got != want {
			t.Errorf("%s: got %v, want %v", address, got, want)
		}
	}
}
