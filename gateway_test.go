package main

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

// procRouteHex encodes an address the way /proc/net/route prints it on this host.
func procRouteHex(address string) string {
	raw := netip.MustParseAddr(address).As4()
	return fmt.Sprintf("%08X", binary.NativeEndian.Uint32(raw[:]))
}

func TestParseProcNetRoutePicksLowestMetricDefault(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"wlan0\t00000000\t" + procRouteHex("192.168.1.1") + "\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t" + procRouteHex("192.168.2.1") + "\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0\t" + procRouteHex("192.168.2.0") + "\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n"

	got, err := parseProcNetRoute(strings.NewReader(table))
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "192.168.2.1" {
		t.Errorf("gateway = %s, want 192.168.2.1", got)
	}
}

func TestParseProcNetRouteWithoutDefault(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t" + procRouteHex("192.168.2.0") + "\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n"
	if _, err := parseProcNetRoute(strings.NewReader(table)); err == nil {
		t.Error("want an error when there is no default route")
	}
}

func TestParseRouteGetOutput(t *testing.T) {
	output := `   route to: default
destination: default
       mask: default
    gateway: 192.168.2.1
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>
`
	got, err := parseRouteGetOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "192.168.2.1" {
		t.Errorf("gateway = %s, want 192.168.2.1", got)
	}

	if _, err := parseRouteGetOutput("   route to: default\n  interface: utun4\n"); err == nil {
		t.Error("want an error when no gateway line is printed")
	}
}

func TestParseRoutePrintOutput(t *testing.T) {
	output := `===========================================================================
Interface List
  7...00 15 5d 01 02 03 ......Intel(R) Ethernet
===========================================================================

IPv4 Route Table
===========================================================================
Active Routes:
Network Destination        Netmask          Gateway       Interface  Metric
          0.0.0.0          0.0.0.0      192.168.1.1     192.168.1.20     55
          0.0.0.0          0.0.0.0      192.168.2.1     192.168.2.50     25
===========================================================================
Persistent Routes:
  Network Address          Netmask  Gateway Address  Metric
          0.0.0.0          0.0.0.0      10.0.0.1  Default
===========================================================================
`
	got, err := parseRoutePrintOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "192.168.2.1" {
		t.Errorf("gateway = %s, want 192.168.2.1", got)
	}
}

func TestCheckRouterCandidate(t *testing.T) {
	cases := []struct {
		gateway     string
		inContainer bool
		wantOK      bool
	}{
		{"192.168.2.1", false, true},
		{"10.0.0.1", false, true},
		{"172.17.0.1", false, true}, // a real router can live in 172.16/12
		{"172.17.0.1", true, false}, // but inside a container that's the bridge
		{"192.168.2.1", true, true}, // host networking
		{DishHopTarget, false, false},
		{"100.64.0.1", false, false}, // CGNAT: straight onto the dish, no router
		{"fe80::1", false, false},
	}
	for _, c := range cases {
		_, err := checkRouterCandidate(netip.MustParseAddr(c.gateway), c.inContainer)
		if gotOK := err == nil; gotOK != c.wantOK {
			t.Errorf("checkRouterCandidate(%s, container=%v): ok=%v (err %v), want ok=%v",
				c.gateway, c.inContainer, gotOK, err, c.wantOK)
		}
	}
}

func TestHopTargetsLeavesOutMissingRouter(t *testing.T) {
	if got := hopTargets(""); len(got) != 3 || got[0] != DishHopTarget {
		t.Errorf("hopTargets(\"\") = %v, want dish first and no router", got)
	}
	if got := hopTargets("192.168.1.1"); len(got) != 4 || got[0] != "192.168.1.1" {
		t.Errorf("hopTargets(router) = %v, want router first", got)
	}
}
