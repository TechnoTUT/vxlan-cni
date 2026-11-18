#!/usr/bin/env bash
# Runs the checks against the FortiGate VM brought up by up.sh.
#   sudo ./test.sh
set -uo pipefail
source "$(dirname "$0")/env.sh"

declare -A POD_NODE=([pod1]=node1 [pod2]=node2)
RESULTS=()
pass() { RESULTS+=("PASS  $*"); echo "PASS  $*"; }
fail() { RESULTS+=("FAIL  $*"); echo "FAIL  $*"; }
info() { RESULTS+=("INFO  $*"); echo "INFO  $*"; }
check() { # description, command...
	local d=$1; shift
	if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi
}

conf() { # node, peers(0|1)
	local peers=""
	[ "$2" = 1 ] && peers="\"peers\": [\"${NODE_IP[node1]}\", \"${NODE_IP[node2]}\"],"
	cat <<EOF
{"cniVersion": "1.0.0", "name": "vxlan", "type": "vxlan", "remote": "$FGT_IP", $peers
 "allowedVlans": [$VLAN], "args": {"cni": {"vlan": $VLAN}},
 "ipam": {"type": "dhcp", "daemonSocketPath": "/run/vxt-dhcp-$1.sock"}}
EOF
}
cni() { # command, pod, peers
	local node=${POD_NODE[$2]}
	conf "$node" "$3" | ip netns exec "$node" env CNI_COMMAND="$1" CNI_CONTAINERID="$2" \
		CNI_NETNS="/run/netns/$2" CNI_IFNAME=net1 CNI_PATH="$BIN" "$BIN/vxlan"
}
pod_ip() { ip -n "$1" -4 -o addr show net1 2>/dev/null | awk '{print $4}' | cut -d/ -f1; }
udp() { ip netns exec "$1" python3 -I "$HERE/udp.py" "${@:2}"; }

# Starts a listener in each netns, runs the sender, and reports how many datagrams arrived.
deliver() { # description, sender-netns, dst, port, group|"", listener-netns...
	local d=$1 from=$2 dst=$3 port=$4 group=$5; shift 5
	local tmp; tmp=$(mktemp -d)
	for ns in "$@"; do
		local local_ip=$CLIENT_IP
		[ "$ns" != client ] && local_ip=$(pod_ip "$ns")
		if [ -n "$group" ]; then udp "$ns" listen "$port" "$group" "$local_ip" >"$tmp/$ns" &
		else udp "$ns" listen "$port" >"$tmp/$ns" &
		fi
	done
	sleep 0.5
	local src=$CLIENT_IP
	[ "$from" != client ] && src=$(pod_ip "$from")
	udp "$from" send "$dst" "$port" "$src"
	wait
	for ns in "$@"; do
		local n; n=$(cat "$tmp/$ns")
		if [ "${n:-0}" -gt 0 ]; then pass "$d: $from -> $ns ($n/5)"; else fail "$d: $from -> $ns (0/5)"; fi
	done
	rm -rf "$tmp"
}

ip netns exec client ip neigh flush all 2>/dev/null
ip netns exec node1 ping -c1 -W2 "$FGT_IP" >/dev/null 2>&1 || die "the FortiGate is not reachable from node1; run up.sh first"
pass "FortiGate reachable on the underlay"
check "client reaches the FortiGate on VLAN $VLAN" ip netns exec client ping -c2 -W2 "$VLAN_GW"

echo "== phase A: no peers"
for p in pod1 pod2; do
	if out=$(cni ADD "$p" 0 2>&1); then pass "ADD $p (dhcp)"; else fail "ADD $p: $out"; fi
done
for p in pod1 pod2; do
	ipaddr=$(pod_ip "$p")
	case "$ipaddr" in
	10.20.0.1[0-9][0-9]) pass "$p got $ipaddr from the FortiGate DHCP server" ;;
	*) fail "$p address '$ipaddr' is not from the DHCP range" ;;
	esac
	if ip -n "$p" route show default | grep -q .; then fail "$p has a default route"; else pass "$p has no default route"; fi
	check "$p -> client ping" ip netns exec "$p" ping -c2 -W2 "$CLIENT_IP"
	check "$p -> FortiGate ping" ip netns exec "$p" ping -c2 -W2 "$VLAN_GW"
done

deliver "broadcast" client 10.20.0.255 50000 "" pod1 pod2
deliver "broadcast" client 255.255.255.255 50001 "" pod1 pod2
deliver "broadcast" pod1 255.255.255.255 50002 "" client
deliver "multicast" client 224.76.78.75 20808 224.76.78.75 pod1 pod2
deliver "multicast" pod1 224.76.78.75 20809 224.76.78.75 client

if ip netns exec pod1 ping -c2 -W2 "$(pod_ip pod2)" >/dev/null 2>&1; then
	info "pod1 -> pod2 without peers: the FortiGate forwards between VXLAN remotes"
else
	info "pod1 -> pod2 without peers: not forwarded by the FortiGate (set peers)"
fi

check "client -> pod1 ping, 1400 bytes" ip netns exec client ping -c2 -W2 -s 1372 -M do "$(pod_ip pod1)"
if ip netns exec client ping -c2 -W2 -s 1472 -M dont "$(pod_ip pod1)" >/dev/null 2>&1; then
	info "client -> pod1 1500-byte packet: delivered (the FortiGate fragments the outer packet)"
else
	info "client -> pod1 1500-byte packet: dropped"
fi

for p in pod1 pod2; do
	if out=$(cni DEL "$p" 0 2>&1); then pass "DEL $p"; else fail "DEL $p: $out"; fi
done
for n in "${NODES[@]}"; do
	if ip -n "$n" link show "vxbr$VLAN" >/dev/null 2>&1; then fail "$n still has vxbr$VLAN"; else pass "$n removed the VTEP"; fi
done

echo "== phase B: with peers"
for p in pod1 pod2; do
	if out=$(cni ADD "$p" 1 2>&1); then pass "ADD $p with peers"; else fail "ADD $p with peers: $out"; fi
done
check "pod1 -> pod2 ping with peers" ip netns exec pod1 ping -c2 -W2 "$(pod_ip pod2)"
deliver "broadcast with peers" pod1 255.255.255.255 50003 "" client pod2
deliver "multicast with peers" pod1 224.76.78.75 20810 224.76.78.75 client pod2
for p in pod1 pod2; do cni DEL "$p" 1 >/dev/null 2>&1; done

echo
echo "== summary"
printf '%s\n' "${RESULTS[@]}"
! printf '%s\n' "${RESULTS[@]}" | grep -q '^FAIL'
