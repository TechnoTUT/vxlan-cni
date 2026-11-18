package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/containernetworking/plugins/pkg/netlinksafe"
	"github.com/containernetworking/plugins/pkg/utils/sysctl"
	"github.com/vishvananda/netlink"
)

const lockFile = "/run/vxlan-cni.lock"

// withLock serializes VTEP creation and removal between concurrent plugin runs.
func withLock(f func() error) error {
	fd, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}
	defer fd.Close()
	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("failed to lock %s: %w", lockFile, err)
	}
	defer syscall.Flock(int(fd.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return f()
}

// underlay finds the link and source address used to reach the remote VTEP.
func underlay(n *NetConf) (netlink.Link, net.IP, error) {
	routes, err := netlink.RouteGet(n.remote)
	if err != nil || len(routes) == 0 {
		return nil, nil, fmt.Errorf("no route to remote %s: %v", n.remote, err)
	}
	dev, err := netlink.LinkByIndex(routes[0].LinkIndex)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to look up the underlay link: %w", err)
	}
	local := n.local
	if local == nil {
		local = routes[0].Src.To4()
	}
	if local == nil {
		return nil, nil, fmt.Errorf("no IPv4 source address towards %s; set local", n.remote)
	}
	return dev, local, nil
}

// ensureVTEP makes sure vxlan<vlan> and vxbr<vlan> exist and are up, and returns the bridge and MTU.
func ensureVTEP(n *NetConf, vlan int) (*netlink.Bridge, int, error) {
	dev, local, err := underlay(n)
	if err != nil {
		return nil, 0, err
	}
	mtu := n.MTU
	if mtu == 0 {
		mtu = dev.Attrs().MTU - vxlanOverhead
	}

	br, err := ensureBridge(bridgeNameOf(vlan), mtu)
	if err != nil {
		return nil, 0, err
	}
	vx, err := ensureVxlan(n, vlan, dev, local, mtu)
	if err != nil {
		return nil, 0, err
	}
	if vx.Attrs().MasterIndex != br.Attrs().Index {
		if err := netlink.LinkSetMaster(vx, br); err != nil {
			return nil, 0, fmt.Errorf("failed to attach %s to %s: %w", vx.Attrs().Name, br.Name, err)
		}
	}
	if err := ensurePeers(vx, local, n.peers); err != nil {
		return nil, 0, err
	}
	for _, l := range []netlink.Link{vx, br} {
		if err := netlink.LinkSetUp(l); err != nil {
			return nil, 0, fmt.Errorf("failed to set %s up: %w", l.Attrs().Name, err)
		}
	}
	return br, mtu, nil
}

func ensureBridge(name string, mtu int) (*netlink.Bridge, error) {
	if l, err := netlinksafe.LinkByName(name); err == nil {
		br, ok := l.(*netlink.Bridge)
		if !ok {
			return nil, fmt.Errorf("%s exists but is not a bridge", name)
		}
		return br, nil
	}
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: mtu, TxQLen: -1}}
	if err := netlink.LinkAdd(br); err != nil && !errors.Is(err, syscall.EEXIST) {
		return nil, fmt.Errorf("failed to create bridge %s: %w", name, err)
	}
	// The host only switches frames; it must not take part in the VLAN.
	_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", name), "1")
	l, err := netlinksafe.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("failed to look up bridge %s: %w", name, err)
	}
	return l.(*netlink.Bridge), nil
}

func ensureVxlan(n *NetConf, vlan int, dev netlink.Link, local net.IP, mtu int) (*netlink.Vxlan, error) {
	name := vxlanName(vlan)
	if l, err := netlinksafe.LinkByName(name); err == nil {
		vx, ok := l.(*netlink.Vxlan)
		if !ok {
			return nil, fmt.Errorf("%s exists but is not a vxlan device", name)
		}
		if vx.VxlanId != n.vni(vlan) || !vx.Group.Equal(n.remote) || vx.Port != n.Port {
			return nil, fmt.Errorf("%s exists with different settings (vni %d, remote %s, port %d); delete it once no pod uses it",
				name, vx.VxlanId, vx.Group, vx.Port)
		}
		return vx, nil
	}
	vx := &netlink.Vxlan{
		LinkAttrs:    netlink.LinkAttrs{Name: name, MTU: mtu, TxQLen: -1},
		VxlanId:      n.vni(vlan),
		VtepDevIndex: dev.Attrs().Index,
		SrcAddr:      local,
		Group:        n.remote, // a unicast group is the default remote
		Port:         n.Port,
		Learning:     true,
	}
	if err := netlink.LinkAdd(vx); err != nil && !errors.Is(err, syscall.EEXIST) {
		return nil, fmt.Errorf("failed to create %s: %w", name, err)
	}
	_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", name), "1")
	l, err := netlinksafe.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("failed to look up %s: %w", name, err)
	}
	return l.(*netlink.Vxlan), nil
}

// ensurePeers adds an all-zero FDB entry per peer so BUM traffic is also sent to other nodes.
func ensurePeers(vx *netlink.Vxlan, local net.IP, peers []net.IP) error {
	for _, p := range peers {
		if p.Equal(local) {
			continue
		}
		err := netlink.NeighAppend(&netlink.Neigh{
			LinkIndex:    vx.Attrs().Index,
			Family:       syscall.AF_BRIDGE,
			State:        netlink.NUD_PERMANENT | netlink.NUD_NOARP,
			Flags:        netlink.NTF_SELF,
			IP:           p,
			HardwareAddr: make(net.HardwareAddr, 6),
		})
		if err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("failed to add FDB entry for peer %s: %w", p, err)
		}
	}
	return nil
}

// removeVTEPIfUnused deletes vxbr<vlan> and vxlan<vlan> once no pod is attached.
func removeVTEPIfUnused(vlan int) error {
	brl, err := netlinksafe.LinkByName(bridgeNameOf(vlan))
	if err != nil {
		return nil // already gone
	}
	links, err := netlinksafe.LinkList()
	if err != nil {
		return err
	}
	vxName := vxlanName(vlan)
	for _, l := range links {
		if l.Attrs().MasterIndex == brl.Attrs().Index && l.Attrs().Name != vxName {
			return nil // still in use
		}
	}
	if vx, err := netlinksafe.LinkByName(vxName); err == nil {
		if err := netlink.LinkDel(vx); err != nil {
			return fmt.Errorf("failed to delete %s: %w", vxName, err)
		}
	}
	if err := netlink.LinkDel(brl); err != nil {
		return fmt.Errorf("failed to delete %s: %w", brl.Attrs().Name, err)
	}
	return nil
}
