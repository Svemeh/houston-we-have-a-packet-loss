package main

import (
	"net/netip"
	"os"
)

func defaultGateway() (netip.Addr, error) {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return netip.Addr{}, err
	}
	defer file.Close()
	return parseProcNetRoute(file)
}
