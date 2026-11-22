package main

import (
	"net"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func recvErrControl(origin, icmpType byte, offender net.IP) []byte {
	data := make([]byte, 32)
	data[4] = origin
	data[5] = icmpType
	data[16] = unix.AF_INET
	copy(data[20:24], offender.To4())
	oob := make([]byte, unix.CmsgSpace(len(data)))
	header := (*unix.Cmsghdr)(unsafe.Pointer(&oob[0]))
	header.Level = unix.IPPROTO_IP
	header.Type = unix.IP_RECVERR
	header.SetLen(unix.CmsgLen(len(data)))
	copy(oob[unix.CmsgLen(0):], data)
	return oob
}

func TestTimeExceededFrom(t *testing.T) {
	target := net.ParseIP("1.1.1.1").To4()
	gateway := net.ParseIP("100.64.0.1")
	echo := quotedEcho(target, 42, 20)[20:]

	if got, ok := timeExceededFrom(echo, recvErrControl(unix.SO_EE_ORIGIN_ICMP, 11, gateway), target, 42); !ok || !got.Equal(gateway) {
		t.Errorf("echo payload: got %v %v", got, ok)
	}
	if got, ok := timeExceededFrom(quotedEcho(target, 42, 20), recvErrControl(unix.SO_EE_ORIGIN_ICMP, 11, gateway), target, 42); !ok || !got.Equal(gateway) {
		t.Errorf("ip payload: got %v %v", got, ok)
	}
	if _, ok := timeExceededFrom(echo, recvErrControl(unix.SO_EE_ORIGIN_ICMP, 11, gateway), target, 43); ok {
		t.Error("matched another seq")
	}
	if _, ok := timeExceededFrom(echo, recvErrControl(unix.SO_EE_ORIGIN_ICMP, 3, gateway), target, 42); ok {
		t.Error("matched destination unreachable")
	}
	if _, ok := timeExceededFrom(echo, recvErrControl(unix.SO_EE_ORIGIN_LOCAL, 11, gateway), target, 42); ok {
		t.Error("matched a local error")
	}
	if _, ok := timeExceededFrom(echo, nil, target, 42); ok {
		t.Error("matched without control message")
	}
}
