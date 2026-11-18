#!/usr/bin/env bash
# Builds the test topology and boots the FortiGate VM.
#   sudo ./up.sh /path/to/fortios.qcow2   (or the FGT_VM64_KVM-*.zip from the support portal)
set -euo pipefail
source "$(dirname "$0")/env.sh"

IMAGE=${1:-${FGT_IMAGE:-}}
[ -n "$IMAGE" ] && [ -f "$IMAGE" ] || die "usage: sudo $0 <fortios.qcow2 | FGT_VM64_KVM zip>"
for c in virsh virt-install xorriso python3 go; do
	command -v "$c" >/dev/null || [ "$c" = go ] || die "$c not found"
done
LIBVIRT_DIR=/var/lib/libvirt/images   # SELinux lets qemu read disks here
mkdir -p "$STATE" "$BIN"

echo "== build plugins"
BUILD_USER=${SUDO_USER:-root}
# Build as the invoking user (Go cache, modules), then install into the root-owned state dir.
TMPB=$(sudo -u "$BUILD_USER" mktemp -d)
sudo -u "$BUILD_USER" bash -lc "cd '$REPO' && go build -o '$TMPB/vxlan' . && \
	cd / && GOBIN='$TMPB' go install github.com/containernetworking/plugins/plugins/ipam/dhcp@v1.10.0"
install -m 755 "$TMPB/vxlan" "$TMPB/dhcp" "$BIN/" && rm -rf "$TMPB"

echo "== VM disk"
DISK=$LIBVIRT_DIR/$VM.qcow2
if [[ $IMAGE == *.zip ]]; then
	rm -rf "$STATE/image" && mkdir -p "$STATE/image"
	unzip -q -o "$IMAGE" -d "$STATE/image"
	IMAGE=$(find "$STATE/image" -name '*.qcow2' | head -1)
	[ -n "$IMAGE" ] || die "no qcow2 in the zip"
fi
cp "$IMAGE" "$DISK"

echo "== day0 config drive"
[ -f "$STATE/admin-password" ] || python3 -I -c 'import secrets; print("Vx9#" + secrets.token_hex(8))' >"$STATE/admin-password"
chmod 600 "$STATE/admin-password"
CD=$STATE/config-drive
rm -rf "$CD" && mkdir -p "$CD/openstack/latest"
sed -e "s/@ADMIN_PASSWORD@/$(cat "$STATE/admin-password")/" \
	-e "s/@FGT_IP@/$FGT_IP/" -e "s/@VLAN@/$VLAN/g" -e "s/@VLAN_GW@/$VLAN_GW/g" \
	-e "s/@NODE1_IP@/${NODE_IP[node1]}/" -e "s/@NODE2_IP@/${NODE_IP[node2]}/" \
	"$HERE/fgt-day0.conf.in" >"$CD/openstack/latest/user_data"
echo '{"uuid": "vxt-fgt", "files": []}' >"$CD/openstack/latest/meta_data.json"
xorriso -as mkisofs -quiet -V config-2 -J -R -o "$LIBVIRT_DIR/$VM-config.iso" "$CD"

echo "== bridges"
for br in "$BR_SV" "$BR_TRUNK"; do
	ip link show "$br" >/dev/null 2>&1 || ip link add "$br" type bridge
	ip link set "$br" up
done
ip addr replace "$HOST_IP/24" dev "$BR_SV"

echo "== nodes and pods"
attach() { # netns, bridge, ifname-in-netns
	local ns=$1 br=$2 ifn=$3 host=vxt-$1
	ip netns add "$ns" 2>/dev/null || true
	ip -n "$ns" link set lo up
	ip link show "$host" >/dev/null 2>&1 || ip link add "$host" type veth peer name "$ifn" netns "$ns"
	ip link set "$host" master "$br" up
	ip -n "$ns" link set "$ifn" up
}
for n in "${NODES[@]}"; do
	attach "$n" "$BR_SV" eth0
	ip -n "$n" addr replace "${NODE_IP[$n]}/24" dev eth0
done
for p in pod1 pod2; do ip netns add "$p" 2>/dev/null || true; done

attach client "$BR_TRUNK" eth0
ip -n client link show "eth0.$VLAN" >/dev/null 2>&1 || ip -n client link add link eth0 name "eth0.$VLAN" type vlan id "$VLAN"
ip -n client addr replace "$CLIENT_IP/24" dev "eth0.$VLAN"
ip -n client link set "eth0.$VLAN" up

echo "== DHCP daemons (one per node)"
for n in "${NODES[@]}"; do
	pidf=/run/vxt-dhcp-$n.pid
	[ -f "$pidf" ] && kill -0 "$(cat "$pidf")" 2>/dev/null && continue
	rm -f "/run/vxt-dhcp-$n.sock"
	ip netns exec "$n" "$BIN/dhcp" daemon -socketpath "/run/vxt-dhcp-$n.sock" -pidfile "$pidf" \
		>"$STATE/dhcp-$n.log" 2>&1 &
done

echo "== FortiGate VM"
NET_WAN=()
if [ "${FGT_WAN:-0}" = 1 ]; then   # port3 with Internet access, needed for FortiCare on 7.2+
	if ! virsh -q net-info default >/dev/null 2>&1; then
		cat >"$STATE/default-net.xml" <<'XML'
<network>
  <name>default</name>
  <forward mode="nat"/>
  <bridge name="virbr0"/>
  <ip address="192.168.122.1" netmask="255.255.255.0">
    <dhcp><range start="192.168.122.2" end="192.168.122.254"/></dhcp>
  </ip>
</network>
XML
		virsh -q net-define "$STATE/default-net.xml"
	fi
	virsh -q net-start default 2>/dev/null || true
	NET_WAN=(--network network=default,model=virtio)
fi
if ! virsh -q dominfo "$VM" >/dev/null 2>&1; then
	virt-install -q --name "$VM" --memory 2048 --vcpus 1 --osinfo linux2020 \
		--import --disk "path=$DISK,bus=virtio" \
		--disk "path=$LIBVIRT_DIR/$VM-config.iso,device=cdrom" \
		--network "bridge=$BR_SV,model=virtio" --network "bridge=$BR_TRUNK,model=virtio" "${NET_WAN[@]}" \
		--graphics none --noautoconsole
fi

echo "== waiting for the FortiGate at $FGT_IP (a few minutes)"
for _ in $(seq 1 60); do
	if ping -c1 -W2 -I "$BR_SV" "$FGT_IP" >/dev/null 2>&1; then
		echo "up. GUI: https://$FGT_IP  user: admin  password: $STATE/admin-password"
		exit 0
	fi
	sleep 5
done
cat <<EOF >&2
The FortiGate did not answer. Check the console:  sudo virsh console $VM
If the day0 config was not applied, log in and paste $CD/openstack/latest/user_data.
EOF
exit 1
