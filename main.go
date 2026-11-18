// Command vxlan is a CNI plugin that attaches a pod to a VLAN carried over VXLAN.
// The VLAN is chosen per pod, and the VTEP for it is created on demand.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/netlinksafe"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/utils/sysctl"
	"github.com/vishvananda/netlink"
)

var buildVersion = "dev"

func main() {
	skel.PluginMainFuncs(skel.CNIFuncs{
		Add:    cmdAdd,
		Del:    cmdDel,
		Check:  cmdCheck,
		Status: cmdStatus,
	}, version.All, "vxlan CNI plugin "+buildVersion)
}

func cmdAdd(args *skel.CmdArgs) (retErr error) {
	n, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}
	vlan, err := n.resolveVLAN(args.Args)
	if err != nil {
		return err
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %w", args.Netns, err)
	}
	defer netns.Close()

	var br *netlink.Bridge
	var mtu int
	var hostIface, contIface *current.Interface
	// Create the VTEP and attach the veth under one lock, so a concurrent DEL
	// cannot remove the bridge between the two steps.
	if err := withLock(func() error {
		var err error
		if br, mtu, err = ensureVTEP(n, vlan); err != nil {
			return err
		}
		hostIface, contIface, err = setupVeth(netns, br, args.IfName, mtu)
		return err
	}); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = netns.Do(func(ns.NetNS) error { return ip.DelLinkByName(args.IfName) })
		}
	}()

	result := &current.Result{
		CNIVersion: current.ImplementedSpecVersion,
		Interfaces: []*current.Interface{
			{Name: br.Name, Mac: br.HardwareAddr.String()},
			hostIface,
			contIface,
		},
	}

	if n.IPAM.Type != "" {
		r, err := ipam.ExecAdd(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
		defer func() {
			if retErr != nil {
				_ = ipam.ExecDel(n.IPAM.Type, args.StdinData)
			}
		}()
		ipamResult, err := current.NewResultFromResult(r)
		if err != nil {
			return err
		}
		if len(ipamResult.IPs) == 0 {
			return errors.New("IPAM plugin returned no IP")
		}
		result.IPs = ipamResult.IPs
		result.Routes = filterRoutes(ipamResult.Routes, n.KeepDefaultRoute)
		result.DNS = ipamResult.DNS
		for _, ipc := range result.IPs {
			ipc.Interface = current.Int(2) // the container interface
		}
		if err := netns.Do(func(ns.NetNS) error {
			_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv4/conf/%s/arp_notify", args.IfName), "1")
			return ipam.ConfigureIface(args.IfName, result)
		}); err != nil {
			return err
		}
	}

	return types.PrintResult(result, n.CNIVersion)
}

// filterRoutes drops default routes unless keep is set.
func filterRoutes(routes []*types.Route, keep bool) []*types.Route {
	if keep {
		return routes
	}
	out := make([]*types.Route, 0, len(routes))
	for _, r := range routes {
		if ones, _ := r.Dst.Mask.Size(); ones == 0 {
			continue
		}
		out = append(out, r)
	}
	return out
}

func setupVeth(netns ns.NetNS, br *netlink.Bridge, ifName string, mtu int) (*current.Interface, *current.Interface, error) {
	hostIface := &current.Interface{}
	contIface := &current.Interface{}
	if err := netns.Do(func(hostNS ns.NetNS) error {
		hostVeth, contVeth, err := ip.SetupVeth(ifName, mtu, "", hostNS)
		if err != nil {
			return err
		}
		hostIface.Name = hostVeth.Name
		contIface.Name = contVeth.Name
		contIface.Mac = contVeth.HardwareAddr.String()
		contIface.Sandbox = netns.Path()
		return nil
	}); err != nil {
		return nil, nil, err
	}

	hostVeth, err := netlinksafe.LinkByName(hostIface.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to look up %q: %w", hostIface.Name, err)
	}
	hostIface.Mac = hostVeth.Attrs().HardwareAddr.String()
	if err := netlink.LinkSetMaster(hostVeth, br); err != nil {
		_ = netlink.LinkDel(hostVeth)
		return nil, nil, fmt.Errorf("failed to attach %q to %s: %w", hostIface.Name, br.Name, err)
	}
	return hostIface, contIface, nil
}

func cmdDel(args *skel.CmdArgs) error {
	n, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	if args.Netns != "" {
		err := ns.WithNetNSPath(args.Netns, func(ns.NetNS) error {
			if err := ip.DelLinkByName(args.IfName); err != nil && !errors.Is(err, ip.ErrLinkNotFound) {
				return err
			}
			return nil
		})
		var notExist ns.NSPathNotExistErr
		if err != nil && !errors.As(err, &notExist) {
			return err
		}
	}

	if n.IPAM.Type != "" {
		if err := ipam.ExecDel(n.IPAM.Type, args.StdinData); err != nil {
			return err
		}
	}

	// Without a valid VLAN there is nothing more to clean up; DEL must not fail for it.
	vlan, err := n.resolveVLAN(args.Args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vxlan: skipping VTEP cleanup: %v\n", err)
		return nil
	}
	return withLock(func() error { return removeVTEPIfUnused(vlan) })
}

func cmdCheck(args *skel.CmdArgs) error {
	n, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}
	vlan, err := n.resolveVLAN(args.Args)
	if err != nil {
		return err
	}
	for _, name := range []string{bridgeNameOf(vlan), vxlanName(vlan)} {
		if _, err := netlinksafe.LinkByName(name); err != nil {
			return fmt.Errorf("%s not found: %w", name, err)
		}
	}
	if err := ns.WithNetNSPath(args.Netns, func(ns.NetNS) error {
		_, err := netlinksafe.LinkByName(args.IfName)
		return err
	}); err != nil {
		return fmt.Errorf("pod interface %s not found: %w", args.IfName, err)
	}
	if n.IPAM.Type != "" {
		return ipam.ExecCheck(n.IPAM.Type, args.StdinData)
	}
	return nil
}

func cmdStatus(args *skel.CmdArgs) error {
	n, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}
	if _, _, err := underlay(n); err != nil {
		return err
	}
	if n.IPAM.Type != "" {
		return ipam.ExecStatus(n.IPAM.Type, args.StdinData)
	}
	return nil
}
