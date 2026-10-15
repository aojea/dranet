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

package ndp

import (
	"crypto/rand"
	"fmt"
	"net"
	"net/netip"
	"slices"
)

// identifierBits is the length of the interface identifier of an
// autoconfigured address; a prefix must leave exactly this many bits
// (RFC 4862 section 5.5.3 d).
const identifierBits = 64

// InterfaceIdentifier is the low 64 bits of an autoconfigured address.
type InterfaceIdentifier [identifierBits / 8]byte

// universalBit is the universal/local bit of a modified EUI-64 identifier:
// set for a globally unique identifier, clear for a local one (RFC 4291
// Appendix A).
const universalBit = 0x02

// EUI64 returns the modified EUI-64 interface identifier of a hardware
// address: a 48-bit Ethernet address gets ff:fe inserted in the middle and
// its universal/local bit inverted (RFC 2464 section 4, RFC 4291 Appendix A);
// a 64-bit EUI-64 only gets the bit inverted. This is the identifier the
// kernel derives in its default address generation mode, so a passthrough
// interface gets in the Pod the address the link already knows it by.
func EUI64(hw net.HardwareAddr) (InterfaceIdentifier, error) {
	var id InterfaceIdentifier
	switch len(hw) {
	case 6:
		id = InterfaceIdentifier{hw[0], hw[1], hw[2], 0xff, 0xfe, hw[3], hw[4], hw[5]}
	case 8:
		copy(id[:], hw)
	default:
		return id, fmt.Errorf("ndp: no EUI-64 identifier for the %d-byte hardware address %s", len(hw), hw)
	}
	id[0] ^= universalBit
	return id, nil
}

// RandomIdentifier returns an interface identifier drawn from a
// cryptographic random source, with the universal/local bit clear as RFC 4941
// section 3.2.1 does for an identifier that is not derived from hardware. It
// suits an interface whose hardware address it shares with another one on
// the link, such as an IPVLAN child and its parent, which EUI-64 would give
// the same address.
func RandomIdentifier() (InterfaceIdentifier, error) {
	var id InterfaceIdentifier
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("ndp: random interface identifier: %w", err)
	}
	id[0] &^= universalBit
	return id, nil
}

// Address forms the address with the prefix's first 64 bits and the
// identifier (RFC 4862 section 5.5.3 d). The prefix must be a /64.
func (id InterfaceIdentifier) Address(prefix netip.Prefix) (netip.Addr, error) {
	if !prefix.IsValid() || !prefix.Addr().Is6() || prefix.Addr().Is4In6() || prefix.Bits() != 128-identifierBits {
		return netip.Addr{}, fmt.Errorf("ndp: an address needs an IPv6 /%d prefix, got %v", 128-identifierBits, prefix)
	}
	a := prefix.Masked().Addr().As16()
	copy(a[8:], id[:])
	return netip.AddrFrom16(a), nil
}

// Route is a route a host installs from a Router Advertisement.
type Route struct {
	// Destination is the prefix the route covers; ::/0 for a default route.
	Destination netip.Prefix
	// Gateway is the router the destination is reached through. It is the
	// zero Addr for an on-link prefix, which is reached directly.
	Gateway netip.Addr
}

// Autoconf is what a host configures from a Router Advertisement.
type Autoconf struct {
	// Addresses are the autoconfigured addresses, each with the prefix
	// length to configure it with: the advertised prefix length when the
	// prefix is on-link, so that the connected route comes with the address
	// as it does for a kernel-autoconfigured one, and /128 when it is not,
	// since the prefix is then reached through the router.
	Addresses []netip.Prefix
	// Routes are the default route, the more specific routes through the
	// router (RFC 4191) and the on-link prefixes no address was formed from.
	Routes []Route
}

// Autoconfigure derives what a host configures from an advertisement sent by
// router, forming addresses with id. It applies RFC 4862 section 5.5.3 to the
// prefixes usable for autoconfiguration, RFC 4861 section 6.3.4 to the
// on-link prefixes and the default router, and RFC 4191 section 3.1 to the
// Route Information options. Lifetimes are not tracked: the result is a
// snapshot for a host that configures itself once.
func Autoconfigure(ra *RouterAdvertisement, router netip.Addr, id InterfaceIdentifier) Autoconf {
	var ac Autoconf
	for _, p := range ra.Prefixes {
		prefix := p.Prefix.Masked()
		// The link-local prefix is never autoconfigured from nor routed
		// (RFC 4862 section 5.5.3 b, RFC 4861 section 6.3.4).
		if prefix.Addr().IsLinkLocalUnicast() {
			continue
		}
		// A zero valid lifetime only ever withdraws a prefix.
		if p.ValidLifetime == 0 {
			continue
		}
		formed := false
		// RFC 4862 section 5.5.3 c and d: a preferred lifetime beyond the
		// valid one or a prefix that leaves no room for the identifier is
		// ignored for autoconfiguration.
		if p.Autonomous && p.PreferredLifetime <= p.ValidLifetime {
			if addr, err := id.Address(prefix); err == nil {
				bits := 128
				if p.OnLink {
					bits = prefix.Bits()
				}
				ac.Addresses = append(ac.Addresses, netip.PrefixFrom(addr, bits))
				formed = true
			}
		}
		// An on-link prefix is reached directly. When an address was formed
		// from it, the address's own prefix length already says so.
		if p.OnLink && !formed {
			ac.Routes = appendRoute(ac.Routes, Route{Destination: prefix})
		}
	}
	defaultRoute := netip.PrefixFrom(netip.IPv6Unspecified(), 0)
	if ra.RouterLifetime > 0 {
		ac.Routes = appendRoute(ac.Routes, Route{Destination: defaultRoute, Gateway: router})
	}
	for _, r := range ra.Routes {
		if r.Lifetime == 0 {
			continue
		}
		ac.Routes = appendRoute(ac.Routes, Route{Destination: r.Prefix.Masked(), Gateway: router})
	}
	return ac
}

// appendRoute adds r unless an equal route is already there: a router may
// advertise ::/0 both through its lifetime and as a Route Information option.
func appendRoute(routes []Route, r Route) []Route {
	if slices.Contains(routes, r) {
		return routes
	}
	return append(routes, r)
}
