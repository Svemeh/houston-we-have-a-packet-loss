#!/bin/sh
set -eu

ns() { name=$1; shift; ip netns exec "$name" "$@"; }

if [ "${1:-}" = attach ]; then
	ip link add d0 type veth peer name l2 netns lan
	ip link set d0 netns "$2"
	nsenter -t "$2" -n sh -c 'ip link set lo up; ip addr add 192.168.3.20/24 dev d0; ip link set d0 up; ip route add default via 192.168.3.1'
	ns lan ip addr add 192.168.3.1/24 dev l2
	ns lan ip link set l2 up
	ns pop ip route add 192.168.3.0/24 via 100.64.0.2
	exit 0
fi

for name in client lan pop far; do ip netns add "$name"; ns "$name" ip link set lo up; done

ip link add c0 netns client type veth peer name l0 netns lan
ip link add l1 netns lan type veth peer name p0 netns pop
ip link add p1 netns pop type veth peer name f0 netns far

ns client ip addr add 192.168.2.10/24 dev c0
ns lan ip addr add 192.168.2.1/24 dev l0
ns lan ip addr add 100.64.0.2/24 dev l1
ns pop ip addr add 100.64.0.1/24 dev p0
ns pop ip addr add 10.0.0.1/24 dev p1
ns far ip addr add 10.0.0.2/24 dev f0
ns far ip addr add 1.1.1.1/32 dev lo
for link in client:c0 lan:l0 lan:l1 pop:p0 pop:p1 far:f0; do ns "${link%%:*}" ip link set "${link#*:}" up; done

ns client ip route add default via 192.168.2.1
ns lan ip route add default via 100.64.0.1
ns pop ip route add 192.168.2.0/24 via 100.64.0.2
ns pop ip route add 1.1.1.1/32 via 10.0.0.2
ns far ip route add default via 10.0.0.1
for router in lan pop; do
	ns "$router" sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward; echo 0 > /proc/sys/net/ipv4/icmp_ratelimit'
done
ns client sh -c 'echo "0 2147483647" > /proc/sys/net/ipv4/ping_group_range'
