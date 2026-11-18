package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/containernetworking/cni/pkg/types"
)

const (
	defaultPort      = 4789
	vxlanOverhead    = 50
	vxlanNamePrefix  = "vxlan"
	bridgeNamePrefix = "vxbr"
)

// NetConf is the network configuration read from stdin.
type NetConf struct {
	types.NetConf

	// Remote is the VTEP that terminates every VLAN (the FortiGate). Required.
	Remote string `json:"remote"`
	// Local is the source address of VXLAN packets. Defaults to the source
	// address of the route to Remote.
	Local string `json:"local,omitempty"`
	// Port is the VXLAN UDP port.
	Port int `json:"port,omitempty"`
	// MTU of the bridge and the pod interface. Defaults to the underlay MTU minus 50.
	MTU int `json:"mtu,omitempty"`
	// Peers are other nodes that also get flooded traffic, so that pods on
	// different nodes can reach each other without hairpinning through Remote.
	Peers []string `json:"peers,omitempty"`
	// AllowedVLANs lists the VLAN IDs a pod may request, e.g. [10, "42-45"]. Required.
	AllowedVLANs []vlanRange `json:"allowedVlans"`
	// VLAN is used when the pod does not request one.
	VLAN int `json:"vlan,omitempty"`
	// KeepDefaultRoute keeps a default route returned by IPAM. By default it is
	// dropped, because the pod's primary network (e.g. Cilium) owns the default route.
	KeepDefaultRoute bool `json:"keepDefaultRoute,omitempty"`

	// Args carries per-pod arguments. Multus puts "cni-args" here.
	Args *struct {
		CNI *podArgs `json:"cni,omitempty"`
	} `json:"args,omitempty"`

	remote net.IP
	local  net.IP
	peers  []net.IP
}

type podArgs struct {
	VLAN json.Number `json:"vlan,omitempty"`
}

// envArgs is CNI_ARGS, e.g. "VLAN=20". Useful for manual testing.
type envArgs struct {
	types.CommonArgs
	VLAN types.UnmarshallableString `json:"VLAN,omitempty"`
}

type vlanRange struct{ from, to int }

func (r *vlanRange) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n int
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("allowedVlans: %s is neither a number nor a string", b)
		}
		s = strconv.Itoa(n)
	}
	from, to, isRange := strings.Cut(strings.TrimSpace(s), "-")
	if !isRange {
		to = from
	}
	var err error
	if r.from, err = parseVLAN(from); err != nil {
		return fmt.Errorf("allowedVlans: %w", err)
	}
	if r.to, err = parseVLAN(to); err != nil {
		return fmt.Errorf("allowedVlans: %w", err)
	}
	if r.from > r.to {
		return fmt.Errorf("allowedVlans: invalid range %q", s)
	}
	return nil
}

func parseVLAN(s string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v < 1 || v > 4094 {
		return 0, fmt.Errorf("invalid VLAN ID %q (must be 1-4094)", s)
	}
	return v, nil
}

func loadConf(stdin []byte) (*NetConf, error) {
	n := &NetConf{Port: defaultPort}
	if err := json.Unmarshal(stdin, n); err != nil {
		return nil, fmt.Errorf("failed to parse network configuration: %w", err)
	}
	if n.remote = net.ParseIP(n.Remote).To4(); n.remote == nil {
		return nil, fmt.Errorf("remote must be an IPv4 address, got %q", n.Remote)
	}
	if n.Local != "" {
		if n.local = net.ParseIP(n.Local).To4(); n.local == nil {
			return nil, fmt.Errorf("local must be an IPv4 address, got %q", n.Local)
		}
	}
	for _, p := range n.Peers {
		ip := net.ParseIP(p).To4()
		if ip == nil {
			return nil, fmt.Errorf("peers must be IPv4 addresses, got %q", p)
		}
		n.peers = append(n.peers, ip)
	}
	if n.Port < 1 || n.Port > 65535 {
		return nil, fmt.Errorf("invalid port %d", n.Port)
	}
	if n.MTU < 0 {
		return nil, fmt.Errorf("invalid mtu %d", n.MTU)
	}
	if len(n.AllowedVLANs) == 0 {
		return nil, errors.New("allowedVlans is required")
	}
	return n, nil
}

// resolveVLAN returns the VLAN the pod asked for and checks it against the allow list.
// Priority: args.cni.vlan (Multus cni-args), CNI_ARGS VLAN=, then the "vlan" default.
func (n *NetConf) resolveVLAN(cniArgs string) (int, error) {
	raw := ""
	if n.VLAN != 0 {
		raw = strconv.Itoa(n.VLAN)
	}
	if cniArgs != "" {
		e := envArgs{}
		if err := types.LoadArgs(cniArgs, &e); err != nil {
			return 0, err
		}
		if e.VLAN != "" {
			raw = string(e.VLAN)
		}
	}
	if n.Args != nil && n.Args.CNI != nil && n.Args.CNI.VLAN != "" {
		raw = n.Args.CNI.VLAN.String()
	}
	if raw == "" {
		return 0, errors.New(`no VLAN given: set "vlan" in cni-args`)
	}
	v, err := parseVLAN(raw)
	if err != nil {
		return 0, err
	}
	if !n.allowed(v) {
		return 0, fmt.Errorf("VLAN %d is not in allowedVlans", v)
	}
	return v, nil
}

func (n *NetConf) allowed(v int) bool {
	for _, r := range n.AllowedVLANs {
		if v >= r.from && v <= r.to {
			return true
		}
	}
	return false
}

func vxlanName(vlan int) string    { return fmt.Sprintf("%s%d", vxlanNamePrefix, vlan) }
func bridgeNameOf(vlan int) string { return fmt.Sprintf("%s%d", bridgeNamePrefix, vlan) }
