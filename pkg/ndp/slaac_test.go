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
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestEUI64(t *testing.T) {
	for name, tc := range map[string]struct {
		hw   net.HardwareAddr
		want InterfaceIdentifier
	}{
		// RFC 2464 section 4: universal address gets the u/l bit set.
		"universal Ethernet address": {
			hw:   net.HardwareAddr{0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde},
			want: InterfaceIdentifier{0x36, 0x56, 0x78, 0xff, 0xfe, 0x9a, 0xbc, 0xde},
		},
		// A locally administered address gets the bit cleared.
		"local Ethernet address": {
			hw:   net.HardwareAddr{0x02, 0x00, 0x5e, 0x10, 0x00, 0x00},
			want: InterfaceIdentifier{0x00, 0x00, 0x5e, 0xff, 0xfe, 0x10, 0x00, 0x00},
		},
		"EUI-64 address": {
			hw:   net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77},
			want: InterfaceIdentifier{0x02, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := EUI64(tc.hw)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("EUI64(%s) = % x, want % x", tc.hw, got, tc.want)
			}
		})
	}
	for _, hw := range []net.HardwareAddr{nil, {1, 2, 3, 4}, make(net.HardwareAddr, 20)} {
		if _, err := EUI64(hw); err == nil {
			t.Errorf("EUI64(%d bytes) accepted an address it cannot derive from", len(hw))
		}
	}
}

func TestRandomIdentifier(t *testing.T) {
	a, err := RandomIdentifier()
	if err != nil {
		t.Fatal(err)
	}
	b, err := RandomIdentifier()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two random identifiers are equal: % x", a)
	}
	for _, id := range []InterfaceIdentifier{a, b} {
		if id[0]&universalBit != 0 {
			t.Errorf("random identifier % x claims to be universal", id)
		}
	}
}

func TestInterfaceIdentifierAddress(t *testing.T) {
	id := InterfaceIdentifier{0x36, 0x56, 0x78, 0xff, 0xfe, 0x9a, 0xbc, 0xde}
	got, err := id.Address(netip.MustParsePrefix("2001:db8:0:1::/64"))
	if err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParseAddr("2001:db8:0:1:3656:78ff:fe9a:bcde"); got != want {
		t.Errorf("Address = %v, want %v", got, want)
	}
	// Bits of the prefix past its length do not leak into the address.
	got, err = id.Address(netip.MustParsePrefix("2001:db8:0:1:ffff::/64"))
	if err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParseAddr("2001:db8:0:1:3656:78ff:fe9a:bcde"); got != want {
		t.Errorf("Address = %v, want %v", got, want)
	}
	for _, prefix := range []string{"2001:db8::/48", "2001:db8::/80", "10.0.0.0/8", "::ffff:10.0.0.0/96"} {
		if _, err := id.Address(netip.MustParsePrefix(prefix)); err == nil {
			t.Errorf("Address(%s) accepted a prefix an identifier cannot complete", prefix)
		}
	}
}

func TestAutoconfigure(t *testing.T) {
	var (
		id           = InterfaceIdentifier{0x36, 0x56, 0x78, 0xff, 0xfe, 0x9a, 0xbc, 0xde}
		router       = netip.MustParseAddr("fe80::1")
		prefix       = netip.MustParsePrefix("2001:db8::/64")
		address      = netip.MustParseAddr("2001:db8::3656:78ff:fe9a:bcde")
		defaultRoute = netip.MustParsePrefix("::/0")
		hour         = time.Hour
	)
	pio := func(p string, onLink, autonomous bool) PrefixInformation {
		return PrefixInformation{Prefix: netip.MustParsePrefix(p), OnLink: onLink, Autonomous: autonomous, ValidLifetime: hour, PreferredLifetime: hour}
	}

	for name, tc := range map[string]struct {
		ra   *RouterAdvertisement
		want Autoconf
	}{
		"on-link autonomous prefix from a default router": {
			ra: &RouterAdvertisement{RouterLifetime: hour, Prefixes: []PrefixInformation{pio("2001:db8::/64", true, true)}},
			want: Autoconf{
				Addresses: []netip.Prefix{netip.PrefixFrom(address, 64)},
				Routes:    []Route{{Destination: defaultRoute, Gateway: router}},
			},
		},
		// The prefix is reached through the router, so the address must not
		// bring a connected route with it.
		"autonomous prefix that is not on-link": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("2001:db8::/64", false, true)}},
			want: Autoconf{Addresses: []netip.Prefix{netip.PrefixFrom(address, 128)}},
		},
		"on-link prefix without autoconfiguration": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("2001:db8::/64", true, false)}},
			want: Autoconf{Routes: []Route{{Destination: prefix}}},
		},
		"prefix with neither flag": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("2001:db8::/64", false, false)}},
			want: Autoconf{},
		},
		"router that is not a default router": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("2001:db8::/64", true, true)}},
			want: Autoconf{Addresses: []netip.Prefix{netip.PrefixFrom(address, 64)}},
		},
		// RFC 4862 section 5.5.3 d: the identifier must fit exactly; the
		// on-link flag still stands on its own.
		"autonomous prefix that is not a /64": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("2001:db8::/56", true, true)}},
			want: Autoconf{Routes: []Route{{Destination: netip.MustParsePrefix("2001:db8::/56")}}},
		},
		"link-local prefix": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("fe80::/64", true, true)}},
			want: Autoconf{},
		},
		"withdrawn prefix": {
			ra: &RouterAdvertisement{Prefixes: []PrefixInformation{{
				Prefix: prefix, OnLink: true, Autonomous: true, ValidLifetime: 0, PreferredLifetime: 0,
			}}},
			want: Autoconf{},
		},
		// RFC 4862 section 5.5.3 c.
		"preferred lifetime longer than valid lifetime": {
			ra: &RouterAdvertisement{Prefixes: []PrefixInformation{{
				Prefix: prefix, OnLink: true, Autonomous: true, ValidLifetime: hour, PreferredLifetime: 2 * hour,
			}}},
			want: Autoconf{Routes: []Route{{Destination: prefix}}},
		},
		"several prefixes": {
			ra: &RouterAdvertisement{RouterLifetime: hour, Prefixes: []PrefixInformation{
				pio("2001:db8::/64", true, true),
				pio("2001:db8:1::/64", true, true),
				pio("2001:db8:2::/48", true, false),
			}},
			want: Autoconf{
				Addresses: []netip.Prefix{
					netip.PrefixFrom(address, 64),
					netip.PrefixFrom(netip.MustParseAddr("2001:db8:1::3656:78ff:fe9a:bcde"), 64),
				},
				Routes: []Route{
					{Destination: netip.MustParsePrefix("2001:db8:2::/48")},
					{Destination: defaultRoute, Gateway: router},
				},
			},
		},
		"route information": {
			ra: &RouterAdvertisement{RouterLifetime: hour, Routes: []RouteInformation{
				{Prefix: netip.MustParsePrefix("2001:db8:ffff::/48"), Lifetime: hour},
				{Prefix: netip.MustParsePrefix("2001:db8:fffe::/48"), Lifetime: 0},
			}},
			want: Autoconf{Routes: []Route{
				{Destination: defaultRoute, Gateway: router},
				{Destination: netip.MustParsePrefix("2001:db8:ffff::/48"), Gateway: router},
			}},
		},
		// A default route advertised both ways is one route.
		"route information for the default route": {
			ra: &RouterAdvertisement{RouterLifetime: hour, Routes: []RouteInformation{
				{Prefix: defaultRoute, Lifetime: hour},
			}},
			want: Autoconf{Routes: []Route{{Destination: defaultRoute, Gateway: router}}},
		},
		// A router with a zero lifetime can still offer a default route by
		// Route Information (RFC 4191 section 3.1).
		"default route only by route information": {
			ra:   &RouterAdvertisement{Routes: []RouteInformation{{Prefix: defaultRoute, Lifetime: hour}}},
			want: Autoconf{Routes: []Route{{Destination: defaultRoute, Gateway: router}}},
		},
		"managed configuration without prefixes": {
			ra:   &RouterAdvertisement{ManagedConfiguration: true, RouterLifetime: hour},
			want: Autoconf{Routes: []Route{{Destination: defaultRoute, Gateway: router}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := Autoconfigure(tc.ra, router, id)
			if diff := cmp.Diff(tc.want, got, equateNetip); diff != "" {
				t.Errorf("Autoconfigure mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
