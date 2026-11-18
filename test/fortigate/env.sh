# Shared settings for the FortiGate VM test. Sourced by the other scripts.
# shellcheck shell=bash

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
STATE=$HERE/.state      # binaries, VM disk, config drive, admin password
BIN=$STATE/bin

VM=vxt-fgt
BR_SV=vxt-sv            # underlay (stands in for the SV VLAN)
BR_TRUNK=vxt-trunk      # FortiGate port2, carries tagged VLANs

FGT_IP=172.30.0.1       # FortiGate port1, the remote VTEP
HOST_IP=172.30.0.254    # this host on the underlay, to reach the FortiGate
NODES=(node1 node2)
declare -A NODE_IP=([node1]=172.30.0.11 [node2]=172.30.0.12)

VLAN=20
VLAN_GW=10.20.0.1       # FortiGate software switch
CLIENT_IP=10.20.0.10    # a host on the tagged VLAN (stands in for a CDJ)

die() { echo "error: $*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "run as root (sudo)"
