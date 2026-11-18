#!/usr/bin/env bash
# Removes everything up.sh created. Keeps .state/admin-password unless --all is given.
set -uo pipefail
source "$(dirname "$0")/env.sh"

if virsh -q dominfo "$VM" >/dev/null 2>&1; then
	virsh -q destroy "$VM" >/dev/null 2>&1
	virsh -q undefine "$VM" --nvram >/dev/null 2>&1 || virsh -q undefine "$VM" >/dev/null
fi
rm -f "/var/lib/libvirt/images/$VM.qcow2" "/var/lib/libvirt/images/$VM-config.iso"

for n in "${NODES[@]}"; do
	pidf=/run/vxt-dhcp-$n.pid
	[ -f "$pidf" ] && kill "$(cat "$pidf")" 2>/dev/null
	rm -f "$pidf" "/run/vxt-dhcp-$n.sock"
done
for ns in "${NODES[@]}" pod1 pod2 client; do
	ip netns del "$ns" 2>/dev/null
	ip link del "vxt-$ns" 2>/dev/null
done
for br in "$BR_SV" "$BR_TRUNK"; do ip link del "$br" 2>/dev/null; done

if [ "${1:-}" = --all ]; then
	rm -rf "$STATE"
else
	rm -rf "$STATE/bin" "$STATE/image" "$STATE/config-drive" "$STATE"/dhcp-*.log
fi
echo "removed"
