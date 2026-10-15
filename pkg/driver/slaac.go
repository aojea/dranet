/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/ndp"
)

// slaacTimeout bounds the wait for a usable Router Advertisement, the same
// budget the DHCP exchange gets. Solicitations go out at 0s, 1s and 3s within
// it, so a router that misses the first one is still heard from.
const slaacTimeout = 5 * time.Second

// slaacIdentifier returns the interface identifier the Pod interface forms
// its autoconfigured addresses with.
//
// A passthrough interface keeps its hardware address, or takes the one the
// claim sets, so the modified EUI-64 identifier gives it the address the link
// already knows it by and the kernel would derive itself; it is unique by
// construction of the hardware address. An IPVLAN child shares the parent's
// hardware address, so EUI-64 would give it the parent's own address: it gets
// a random identifier, as the kernel's random address generation mode would.
func slaacIdentifier(iface apis.InterfaceConfig, hostHWAddr net.HardwareAddr) (ndp.InterfaceIdentifier, error) {
	if iface.IsSubinterface() {
		return ndp.RandomIdentifier()
	}
	hw := hostHWAddr
	if iface.HardwareAddr != nil {
		var err error
		if hw, err = net.ParseMAC(*iface.HardwareAddr); err != nil {
			// Validation already rejected it; keep the error path honest.
			return ndp.InterfaceIdentifier{}, fmt.Errorf("invalid hardwareAddr %q: %w", *iface.HardwareAddr, err)
		}
	}
	return ndp.EUI64(hw)
}

// getSLAAC solicits a Router Advertisement on the host interface ifName and
// returns the IPv6 addresses and routes a host would autoconfigure from it,
// with id as the interface identifier. Like getDHCP, it only gathers the
// configuration: nothing is applied in the host namespace, and the result is
// configured statically in the Pod. The advertisement's lifetimes are not
// tracked, as a DHCP lease's are not: the Pod's addresses live as long as the
// Pod.
//
// The exchange goes over a packet socket, so it works whether or not the host
// has IPv6 enabled on the interface. When the interface has a link-local
// address that passed duplicate address detection, the solicitations are sent
// from it so routers can answer by unicast; otherwise they go out from the
// unspecified address and the routers answer all nodes (RFC 4861 section
// 6.2.6).
func getSLAAC(ctx context.Context, ifName string, id ndp.InterfaceIdentifier) (addresses []string, routes []apis.RouteConfig, err error) {
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return nil, nil, err
	}
	if link.Attrs().OperState != netlink.OperUp {
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, nil, fmt.Errorf("failed to set interface %s up: %w", ifName, err)
		}
	}
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get interface %s: %w", ifName, err)
	}

	// An advertisement is only useful if an address can be formed from it; a
	// router that only announces itself, or a link where addresses come from
	// DHCPv6, is remembered so the failure says what the link offered.
	var unusable string
	accept := func(ra *ndp.RouterAdvertisement, router netip.Addr) bool {
		if len(ndp.Autoconfigure(ra, router, id).Addresses) > 0 {
			return true
		}
		unusable = describeUnusableAdvertisement(ra, router)
		klog.V(2).Infof("ignoring router advertisement on %s: %s", ifName, unusable)
		return false
	}
	ra, router, err := ndp.Solicit(ctx, ifi, linkLocalAddress(link), accept)
	if err != nil {
		if unusable != "" {
			return nil, nil, fmt.Errorf("%w; last advertisement: %s", err, unusable)
		}
		return nil, nil, err
	}

	autoconf := ndp.Autoconfigure(ra, router, id)
	for _, address := range autoconf.Addresses {
		addresses = append(addresses, address.String())
	}
	for _, route := range autoconf.Routes {
		routeCfg := apis.RouteConfig{Destination: route.Destination.String()}
		if route.Gateway.IsValid() {
			routeCfg.Gateway = route.Gateway.String()
		} else {
			routeCfg.Scope = unix.RT_SCOPE_LINK
		}
		routes = append(routes, routeCfg)
	}
	klog.V(2).Infof("router %v on %s advertised %d prefix(es) and %d route(s): configuring addresses %v and routes %+v", router, ifName, len(ra.Prefixes), len(ra.Routes), addresses, routes)
	if ra.MTU != 0 {
		klog.V(4).Infof("router %v on %s advertises MTU %d; the interface MTU is not changed by it", router, ifName, ra.MTU)
	}
	return addresses, routes, nil
}

// describeUnusableAdvertisement says why no address could be formed from an
// advertisement, in the terms an operator can check on the router.
func describeUnusableAdvertisement(ra *ndp.RouterAdvertisement, router netip.Addr) string {
	autonomous := 0
	for _, p := range ra.Prefixes {
		if p.Autonomous {
			autonomous++
		}
	}
	switch {
	case len(ra.Prefixes) == 0 && ra.ManagedConfiguration:
		return fmt.Sprintf("router %v advertises no prefix and sets the managed address configuration flag: addresses on this link come from DHCPv6, which is not supported", router)
	case len(ra.Prefixes) == 0:
		return fmt.Sprintf("router %v advertises no prefix", router)
	case autonomous == 0:
		return fmt.Sprintf("router %v advertises %d prefix(es), none with the autonomous flag", router, len(ra.Prefixes))
	default:
		return fmt.Sprintf("router %v advertises %d autonomous prefix(es), none usable: a prefix must be a /64 with a non-zero valid lifetime not shorter than its preferred lifetime", router, autonomous)
	}
}

// linkLocalAddress returns a link-local address of link that has completed
// duplicate address detection, or the unspecified address if there is none,
// for example because the host has IPv6 disabled on the interface or the link
// just came up. A tentative address must not be used as a source yet
// (RFC 4862 section 5.4).
func linkLocalAddress(link netlink.Link) netip.Addr {
	addrs, err := nlwrap.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		klog.V(2).Infof("failed to list the IPv6 addresses of %s, soliciting from the unspecified address: %v", link.Attrs().Name, err)
		return netip.IPv6Unspecified()
	}
	for _, addr := range addrs {
		if addr.IP == nil || !addr.IP.IsLinkLocalUnicast() || addr.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) != 0 {
			continue
		}
		if ip, ok := netip.AddrFromSlice(addr.IP); ok {
			return ip
		}
	}
	return netip.IPv6Unspecified()
}
