//go:build !linux && !darwin && !windows

package main

import (
	"errors"
	"net/netip"
)

func defaultGateway() (netip.Addr, error) {
	return netip.Addr{}, errors.New("default gateway lookup not supported on this OS")
}
