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
	output, err := exec.CommandContext(ctx, "route", "print", "-4", "0.0.0.0").Output()
	if err != nil {
		return netip.Addr{}, err
	}
	return parseRoutePrintOutput(string(output))
}
