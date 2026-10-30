package main

import (
	"context"
	"net/netip"
	"os/exec"
	"time"
)

func defaultGateway() (netip.Addr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return netip.Addr{}, err
	}
	return parseRouteGetOutput(string(output))
}
