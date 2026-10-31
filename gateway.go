package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// RouterAuto and RouterNone are the special values of the -router flag.
const (
	RouterAuto = "auto"
	RouterNone = "none"
)

// dockerBridgeRange is where Docker hands out bridge networks. Inside one, the
// default gateway is the bridge, not the LAN router.
var dockerBridgeRange = netip.MustParsePrefix("172.16.0.0/12")

// resolveRouter turns the -router flag into the address the hop pinger uses
// for the LAN router, or "" to leave the router out.
//
// Guessing is worse than leaving it out: a wrong address reads as 100% loss on
// the hop that is supposed to prove the LAN is clean.
func resolveRouter(flagValue string) string {
	switch flagValue {
	case RouterNone, "":
		log.Print("hops: router hop disabled (-router none)")
		return ""
	case RouterAuto:
		router, err := detectRouter()
		if err != nil {
			log.Printf("hops: no LAN router hop: %v (set one with -router <ip>)", err)
			return ""
		}
		log.Printf("hops: router %s (default gateway)", router)
		return router
	default:
		address, err := netip.ParseAddr(flagValue)
		if err != nil {
			log.Printf("hops: -router %q is not an IP address, router hop disabled", flagValue)
			return ""
		}
		log.Printf("hops: router %s (from -router)", address)
		return address.String()
	}
}

// detectRouter finds the LAN router: this machine's IPv4 default gateway.
func detectRouter() (string, error) {
	gateway, err := defaultGateway()
	if err != nil {
		return "", err
	}
	return checkRouterCandidate(gateway, runningInContainer())
}

// checkRouterCandidate rejects gateways that aren't a router on this LAN.
func checkRouterCandidate(gateway netip.Addr, inContainer bool) (string, error) {
	gateway = gateway.Unmap()
	switch {
	case !gateway.Is4():
		return "", fmt.Errorf("default gateway %s is not IPv4", gateway)
	case gateway.String() == DishHopTarget:
		return "", fmt.Errorf("the dish is the default gateway, so there is no separate router")
	case !gateway.IsPrivate():
		return "", fmt.Errorf("default gateway %s is not a LAN address (VPN, or connected straight to the dish?)", gateway)
	case inContainer && dockerBridgeRange.Contains(gateway):
		return "", fmt.Errorf("default gateway %s looks like a Docker bridge; run the container with host networking", gateway)
	}
	return gateway.String(), nil
}

func runningInContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

var errNoDefaultRoute = errors.New("no IPv4 default route")

// parseProcNetRoute reads Linux's /proc/net/route and returns the gateway of
// the default route with the lowest metric. Addresses there are the raw
// network-order bytes printed as a host-order integer, so decoding them with
// the native byte order gives the bytes back.
func parseProcNetRoute(reader io.Reader) (netip.Addr, error) {
	const (
		flagUp      = 0x1
		flagGateway = 0x2
	)
	var best netip.Addr
	bestMetric := math.MaxInt

	scanner := bufio.NewScanner(reader)
	scanner.Scan() // header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&(flagUp|flagGateway) != flagUp|flagGateway {
			continue
		}
		raw, err := hex.DecodeString(fields[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		var gatewayBytes [4]byte
		binary.NativeEndian.PutUint32(gatewayBytes[:], binary.BigEndian.Uint32(raw))
		metric, err := strconv.Atoi(fields[6])
		if err != nil {
			continue
		}
		if metric < bestMetric {
			best, bestMetric = netip.AddrFrom4(gatewayBytes), metric
		}
	}
	if err := scanner.Err(); err != nil {
		return netip.Addr{}, err
	}
	if !best.IsValid() {
		return netip.Addr{}, errNoDefaultRoute
	}
	return best, nil
}

// parseRouteGetOutput reads macOS `route -n get default`.
func parseRouteGetOutput(output string) (netip.Addr, error) {
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found || key != "gateway" {
			continue
		}
		return netip.ParseAddr(strings.TrimSpace(value))
	}
	return netip.Addr{}, errNoDefaultRoute
}

// parseRoutePrintOutput reads Windows `route print -4 0.0.0.0` and returns the
// default route with the lowest metric. Only the numeric columns are read, so
// a localised Windows parses the same.
func parseRoutePrintOutput(output string) (netip.Addr, error) {
	var best netip.Addr
	bestMetric := math.MaxInt
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "0.0.0.0" || fields[1] != "0.0.0.0" {
			continue // persistent routes have 4 columns and land here too
		}
		gateway, err := netip.ParseAddr(fields[2])
		if err != nil {
			continue // "On-link"
		}
		metric, err := strconv.Atoi(fields[4])
		if err != nil {
			continue
		}
		if metric < bestMetric {
			best, bestMetric = gateway, metric
		}
	}
	if !best.IsValid() {
		return netip.Addr{}, errNoDefaultRoute
	}
	return best, nil
}
