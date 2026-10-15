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
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/ndp"
)

var (
	slaacTestRouter = netip.MustParseAddr("fe80::1")
	slaacTestPrefix = netip.MustParsePrefix("2001:db8::/64")
)

// slaacTestRA is what a router on a fabric that hands out prefixes by Router
// Advertisement typically sends: a default route, an on-link prefix for
// autoconfiguration, a second on-link prefix that is routed only, and a more
// specific route (RFC 4191).
var slaacTestRA = &ndp.RouterAdvertisement{
	CurrentHopLimit: 64,
	RouterLifetime:  30 * time.Minute,
	MTU:             9000,
	Prefixes: []ndp.PrefixInformation{
		{Prefix: slaacTestPrefix, OnLink: true, Autonomous: true, ValidLifetime: 24 * time.Hour, PreferredLifetime: 4 * time.Hour},
		{Prefix: netip.MustParsePrefix("2001:db8:2::/64"), OnLink: true, ValidLifetime: 24 * time.Hour},
	},
	Routes: []ndp.RouteInformation{
		{Prefix: netip.MustParsePrefix("2001:db8:1::/48"), Lifetime: time.Hour},
	},
}

// slaacTestRoutes are the routes slaacTestRA configures through router.
func slaacTestRoutes(router netip.Addr) []apis.RouteConfig {
	return []apis.RouteConfig{
		{Destination: "2001:db8:2::/64", Scope: unix.RT_SCOPE_LINK},
		{Destination: "::/0", Gateway: router.String()},
		{Destination: "2001:db8:1::/48", Gateway: router.String()},
	}
}

func TestSlaacIdentifier(t *testing.T) {
	hostMAC := net.HardwareAddr{0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde}
	hostEUI64 := ndp.InterfaceIdentifier{0x36, 0x56, 0x78, 0xff, 0xfe, 0x9a, 0xbc, 0xde}

	t.Run("passthrough derives from the host hardware address", func(t *testing.T) {
		got, err := slaacIdentifier(apis.InterfaceConfig{}, hostMAC)
		if err != nil {
			t.Fatal(err)
		}
		if got != hostEUI64 {
			t.Errorf("identifier = % x, want % x", got, hostEUI64)
		}
	})
	// The Pod interface will carry the configured address, so the identifier
	// must come from it: the host one would give an address the link never
	// sees the Pod's hardware address under.
	t.Run("passthrough derives from the configured hardware address", func(t *testing.T) {
		got, err := slaacIdentifier(apis.InterfaceConfig{HardwareAddr: ptr.To("02:00:5e:10:00:00")}, hostMAC)
		if err != nil {
			t.Fatal(err)
		}
		if want := (ndp.InterfaceIdentifier{0x00, 0x00, 0x5e, 0xff, 0xfe, 0x10, 0x00, 0x00}); got != want {
			t.Errorf("identifier = % x, want % x", got, want)
		}
	})
	t.Run("invalid configured hardware address", func(t *testing.T) {
		if _, err := slaacIdentifier(apis.InterfaceConfig{HardwareAddr: ptr.To("zz")}, hostMAC); err == nil {
			t.Error("slaacIdentifier accepted an invalid hardwareAddr")
		}
	})
	t.Run("IPVLAN child gets a random identifier", func(t *testing.T) {
		iface := apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN}
		first, err := slaacIdentifier(iface, hostMAC)
		if err != nil {
			t.Fatal(err)
		}
		second, err := slaacIdentifier(iface, hostMAC)
		if err != nil {
			t.Fatal(err)
		}
		if first == hostEUI64 || second == hostEUI64 {
			t.Errorf("an IPVLAN child got the parent's EUI-64 identifier % x", hostEUI64)
		}
		if first == second {
			t.Errorf("two IPVLAN children got the same identifier % x", first)
		}
	})
}

func TestDescribeUnusableAdvertisement(t *testing.T) {
	for name, tc := range map[string]struct {
		ra   *ndp.RouterAdvertisement
		want string
	}{
		"managed without prefixes": {
			ra:   &ndp.RouterAdvertisement{ManagedConfiguration: true},
			want: "DHCPv6",
		},
		"no prefixes": {
			ra:   &ndp.RouterAdvertisement{RouterLifetime: time.Hour},
			want: "advertises no prefix",
		},
		"on-link only": {
			ra:   &ndp.RouterAdvertisement{Prefixes: []ndp.PrefixInformation{{Prefix: slaacTestPrefix, OnLink: true, ValidLifetime: time.Hour}}},
			want: "none with the autonomous flag",
		},
		"autonomous but not a /64": {
			ra:   &ndp.RouterAdvertisement{Prefixes: []ndp.PrefixInformation{{Prefix: netip.MustParsePrefix("2001:db8::/56"), Autonomous: true, ValidLifetime: time.Hour}}},
			want: "none usable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := describeUnusableAdvertisement(tc.ra, slaacTestRouter)
			if !strings.Contains(got, tc.want) || !strings.Contains(got, slaacTestRouter.String()) {
				t.Errorf("describeUnusableAdvertisement = %q, want it to mention %q and the router", got, tc.want)
			}
		})
	}
}

// slaacTestLink is a veth pair whose router end answers solicitations the way
// a router on the fabric would.
type slaacTestLink struct {
	host   string
	router *ndp.Conn

	mu           sync.Mutex
	solicitation *ndp.RouterSolicitation
	source       netip.Addr
}

// newSlaacTestLink creates the pair in the test's network namespace and
// starts answering solicitations on the router end with ra; a nil ra leaves
// the link without a router.
func newSlaacTestLink(t *testing.T, host, peer string, ra *ndp.RouterAdvertisement) *slaacTestLink {
	t.Helper()
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: host}, PeerName: peer}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("add veth: %v", err)
	}
	for _, name := range []string{host, peer} {
		link, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatalf("set %s up: %v", name, err)
		}
	}
	l := &slaacTestLink{host: host}
	if ra == nil {
		return l
	}
	ifi, err := net.InterfaceByName(peer)
	if err != nil {
		t.Fatal(err)
	}
	l.router, err = ndp.Listen(ifi, ndp.TypeRouterSolicitation)
	if err != nil {
		t.Fatalf("router listen: %v", err)
	}
	t.Cleanup(func() { l.router.Close() })
	go func() {
		for {
			m, src, err := l.router.ReadFrom()
			if err != nil {
				return
			}
			rs, ok := m.(*ndp.RouterSolicitation)
			if !ok {
				continue
			}
			l.mu.Lock()
			l.solicitation, l.source = rs, src
			l.mu.Unlock()
			if err := l.router.WriteTo(ra, slaacTestRouter, ndp.AllNodes, nil); err != nil {
				t.Errorf("router send: %v", err)
			}
		}
	}()
	return l
}

// lastSolicitation returns the last solicitation the router saw and its source.
func (l *slaacTestLink) lastSolicitation() (*ndp.RouterSolicitation, netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.solicitation, l.source
}

// hostEUI64Address is the address the host end autoconfigures in prefix.
func (l *slaacTestLink) hostEUI64Address(t *testing.T, prefix netip.Prefix) string {
	t.Helper()
	ifi, err := net.InterfaceByName(l.host)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ndp.EUI64(ifi.HardwareAddr)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := id.Address(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return netip.PrefixFrom(addr, prefix.Bits()).String()
}

// waitForLinkLocal waits until the host end has a link-local address that
// completed duplicate address detection, as a host interface that has been
// up for a while has.
func (l *slaacTestLink) waitForLinkLocal(t *testing.T) netip.Addr {
	t.Helper()
	link, err := netlink.LinkByName(l.host)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := linkLocalAddress(link); !addr.IsUnspecified() {
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s got no usable link-local address", l.host)
	return netip.Addr{}
}

func TestGetSLAAC(t *testing.T) {
	userns.Run(t, testGetSLAAC, syscall.CLONE_NEWNET)
}

// The configuration is what the kernel would autoconfigure from the same
// advertisement: the EUI-64 address in the autonomous prefix with its on-link
// prefix length, the on-link route for the prefix without one, the default
// route and the more specific route through the router. Once the interface
// has a link-local address, the solicitation is sent from it with the
// hardware address, so the router can answer by unicast.
func testGetSLAAC(t *testing.T) {
	link := newSlaacTestLink(t, "host0", "router0", slaacTestRA)
	linkLocal := link.waitForLinkLocal(t)

	ifi, err := net.InterfaceByName(link.host)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ndp.EUI64(ifi.HardwareAddr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), slaacTimeout)
	defer cancel()
	start := time.Now()
	addresses, routes, err := getSLAAC(ctx, link.host, id)
	if err != nil {
		t.Fatalf("getSLAAC after %v: %v", time.Since(start), err)
	}
	if want := []string{link.hostEUI64Address(t, slaacTestPrefix)}; !cmp.Equal(addresses, want) {
		t.Errorf("addresses = %v, want %v", addresses, want)
	}
	if diff := cmp.Diff(slaacTestRoutes(slaacTestRouter), routes); diff != "" {
		t.Errorf("routes mismatch (-want +got):\n%s", diff)
	}
	rs, src := link.lastSolicitation()
	if rs == nil {
		t.Fatal("the router saw no solicitation")
	}
	if src != linkLocal {
		t.Errorf("solicitation from %v, want the link-local address %v", src, linkLocal)
	}
	if rs.SourceLinkLayerAddress.String() != ifi.HardwareAddr.String() {
		t.Errorf("solicitation link-layer address = %v, want %v", rs.SourceLinkLayerAddress, ifi.HardwareAddr)
	}
}

func TestGetSLAACWithIPv6DisabledOnHost(t *testing.T) {
	userns.Run(t, testGetSLAACWithIPv6DisabledOnHost, syscall.CLONE_NEWNET)
}

// A host that has IPv6 disabled on the interface, as a node that only uses
// IPv4 itself may, can still autoconfigure the Pod: the exchange does not go
// through the host's IPv6 stack. The solicitation then comes from the
// unspecified address without a link-layer address (RFC 4861 section 4.1).
func testGetSLAACWithIPv6DisabledOnHost(t *testing.T) {
	link := newSlaacTestLink(t, "host0", "router0", slaacTestRA)
	if err := os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", link.host), []byte("1"), 0o644); err != nil {
		t.Fatalf("disable IPv6 on %s: %v", link.host, err)
	}
	hostLink, err := netlink.LinkByName(link.host)
	if err != nil {
		t.Fatal(err)
	}
	if addrs, err := netlink.AddrList(hostLink, netlink.FAMILY_V6); err != nil || len(addrs) != 0 {
		t.Fatalf("%s still has IPv6 addresses %v (err %v)", link.host, addrs, err)
	}

	id := ndp.InterfaceIdentifier{0, 0, 0, 0, 0, 0, 0, 1}
	ctx, cancel := context.WithTimeout(context.Background(), slaacTimeout)
	defer cancel()
	addresses, routes, err := getSLAAC(ctx, link.host, id)
	if err != nil {
		t.Fatalf("getSLAAC: %v", err)
	}
	if want := []string{"2001:db8::1/64"}; !cmp.Equal(addresses, want) {
		t.Errorf("addresses = %v, want %v", addresses, want)
	}
	if diff := cmp.Diff(slaacTestRoutes(slaacTestRouter), routes); diff != "" {
		t.Errorf("routes mismatch (-want +got):\n%s", diff)
	}
	rs, src := link.lastSolicitation()
	if rs == nil {
		t.Fatal("the router saw no solicitation")
	}
	if !src.IsUnspecified() {
		t.Errorf("solicitation from %v, want the unspecified address", src)
	}
	if rs.SourceLinkLayerAddress != nil {
		t.Errorf("solicitation from the unspecified address carried link-layer address %v", rs.SourceLinkLayerAddress)
	}
}

func TestGetSLAACUnusableAdvertisement(t *testing.T) {
	userns.Run(t, testGetSLAACUnusableAdvertisement, syscall.CLONE_NEWNET)
}

// A link whose router points to DHCPv6 must fail with that diagnosis rather
// than a bare timeout, and must not configure a default route without an
// address to use it from.
func testGetSLAACUnusableAdvertisement(t *testing.T) {
	link := newSlaacTestLink(t, "host0", "router0", &ndp.RouterAdvertisement{ManagedConfiguration: true, RouterLifetime: time.Hour})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	addresses, routes, err := getSLAAC(ctx, link.host, ndp.InterfaceIdentifier{0, 0, 0, 0, 0, 0, 0, 1})
	if err == nil {
		t.Fatalf("getSLAAC = %v %v, want an error", addresses, routes)
	}
	for _, want := range []string{"DHCPv6", slaacTestRouter.String(), context.DeadlineExceeded.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("getSLAAC error = %q, want it to mention %q", err, want)
		}
	}
}

func TestGetSLAACWithoutRouter(t *testing.T) {
	userns.Run(t, testGetSLAACWithoutRouter, syscall.CLONE_NEWNET)
}

func testGetSLAACWithoutRouter(t *testing.T) {
	link := newSlaacTestLink(t, "host0", "router0", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _, err := getSLAAC(ctx, link.host, ndp.InterfaceIdentifier{0, 0, 0, 0, 0, 0, 0, 1})
	if err == nil || !strings.Contains(err.Error(), "no router advertisement received") {
		t.Errorf("getSLAAC error = %v, want no router advertisement received", err)
	}
}

func TestPrepareResourceClaimSLAAC(t *testing.T) {
	userns.Run(t, testPrepareResourceClaimSLAAC, syscall.CLONE_NEWNET)
}

// The prepare path with SLAAC behaves like the one with DHCP: the exchange
// runs on the host interface and its result lands in the Pod configuration
// as static addresses and routes.
func testPrepareResourceClaimSLAAC(t *testing.T) {
	const testDriverName = "test.driver"
	link := newSlaacTestLink(t, "host0", "router0", slaacTestRA)
	hostLink, err := netlink.LinkByName(link.host)
	if err != nil {
		t.Fatal(err)
	}
	// The host has an IPv4 address, an IPv6 address of its own in another
	// prefix, and routes it learned from its own router advertisements.
	for _, cidr := range []string{"192.0.2.10/24", "2001:db8:ffff::10/64"} {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.AddrAdd(hostLink, addr); err != nil {
			t.Fatalf("add %s: %v", cidr, err)
		}
	}
	link.waitForLinkLocal(t)
	_, raDst, _ := net.ParseCIDR("2001:db8:ffff:1::/64")
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: hostLink.Attrs().Index, Dst: raDst, Gw: slaacTestRouter.AsSlice(), Protocol: unix.RTPROT_RA}); err != nil {
		t.Fatalf("add proto ra route: %v", err)
	}
	_, staticDst, _ := net.ParseCIDR("198.51.100.0/24")
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: hostLink.Attrs().Index, Dst: staticDst, Gw: net.ParseIP("192.0.2.1")}); err != nil {
		t.Fatalf("add static route: %v", err)
	}

	claim := func(name string, parameters string) *resourcev1.ResourceClaim {
		return &resourcev1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-uid-" + name), Namespace: "default", Name: name},
			Status: resourcev1.ResourceClaimStatus{
				ReservedFor: []resourcev1.ResourceClaimConsumerReference{
					{APIGroup: "", Resource: "pods", Name: "test-pod", UID: types.UID("pod-uid-" + name)},
				},
				Allocation: &resourcev1.AllocationResult{
					Devices: resourcev1.DeviceAllocationResult{
						Results: []resourcev1.DeviceRequestAllocationResult{
							{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
						},
						Config: []resourcev1.DeviceAllocationConfiguration{{
							Source: resourcev1.AllocationConfigSourceClaim,
							DeviceConfiguration: resourcev1.DeviceConfiguration{
								Opaque: &resourcev1.OpaqueDeviceConfiguration{
									Driver:     testDriverName,
									Parameters: runtime.RawExtension{Raw: []byte(parameters)},
								},
							},
						}},
					},
				},
			},
		}
	}
	prepare := func(t *testing.T, claim *resourcev1.ResourceClaim) DeviceConfig {
		t.Helper()
		fakeDB := newFakeInventoryDB()
		fakeDB.GetNetInterfaceNameFunc = func(string) (string, error) { return link.host, nil }
		fakeDB.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
			return resourcev1.Device{Name: deviceName}, true
		}
		np := &NetworkDriver{
			netdb:          fakeDB,
			driverName:     testDriverName,
			podConfigStore: mustNewPodConfigStore(),
			eventRecorder:  record.NewFakeRecorder(100),
		}
		if result := np.prepareResourceClaim(t.Context(), claim); result.Err != nil {
			t.Fatalf("prepareResourceClaim: %v", result.Err)
		}
		podCfg, ok := np.podConfigStore.GetPodConfig(claim.Status.ReservedFor[0].UID)
		if !ok {
			t.Fatal("no pod config stored")
		}
		return podCfg.DeviceConfigs["net-dev-0"]
	}

	t.Run("passthrough", func(t *testing.T) {
		cfg := prepare(t, claim("slaac-passthrough", `{"interface":{"addressing":"SLAAC"}}`))
		// The host IPv4 address is kept, the host IPv6 address is not: the
		// autoconfigured one replaces it.
		wantAddresses := []string{"192.0.2.10/24", link.hostEUI64Address(t, slaacTestPrefix)}
		if diff := cmp.Diff(wantAddresses, cfg.NetworkInterfaceConfigInPod.Interface.Addresses); diff != "" {
			t.Errorf("addresses mismatch (-want +got):\n%s", diff)
		}
		// The advertised routes come first, then the host routes except the
		// IPv6 ones the host learned from router advertisements or derived
		// from its own addresses; the IPv4 routes are inherited as usual.
		wantRoutes := append(slaacTestRoutes(slaacTestRouter),
			apis.RouteConfig{Destination: "192.0.2.0/24", Source: "192.0.2.10", Scope: unix.RT_SCOPE_LINK, Table: unix.RT_TABLE_MAIN},
			apis.RouteConfig{Destination: "198.51.100.0/24", Gateway: "192.0.2.1", Table: unix.RT_TABLE_MAIN},
		)
		if diff := cmp.Diff(wantRoutes, cfg.NetworkInterfaceConfigInPod.Routes); diff != "" {
			t.Errorf("routes mismatch (-want +got):\n%s", diff)
		}
		if cfg.NetworkInterfaceStateInPod != nil {
			t.Errorf("SLAAC left state to release: %+v", cfg.NetworkInterfaceStateInPod)
		}
	})

	t.Run("passthrough with IPv4 addresses in the claim", func(t *testing.T) {
		cfg := prepare(t, claim("slaac-passthrough-v4", `{"interface":{"addressing":"SLAAC","addresses":["192.0.2.20/24"]}}`))
		wantAddresses := []string{"192.0.2.20/24", link.hostEUI64Address(t, slaacTestPrefix)}
		if diff := cmp.Diff(wantAddresses, cfg.NetworkInterfaceConfigInPod.Interface.Addresses); diff != "" {
			t.Errorf("addresses mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("passthrough with a hardware address", func(t *testing.T) {
		cfg := prepare(t, claim("slaac-passthrough-mac", `{"interface":{"addressing":"SLAAC","hardwareAddr":"02:00:5e:10:00:01"}}`))
		if want := "2001:db8::5eff:fe10:1/64"; len(cfg.NetworkInterfaceConfigInPod.Interface.Addresses) != 2 || cfg.NetworkInterfaceConfigInPod.Interface.Addresses[1] != want {
			t.Errorf("addresses = %v, want the second to be %s", cfg.NetworkInterfaceConfigInPod.Interface.Addresses, want)
		}
	})

	t.Run("IPVLAN", func(t *testing.T) {
		cfg := prepare(t, claim("slaac-ipvlan", `{"interface":{"type":"IPVLAN","addressing":"SLAAC"}}`))
		addresses := cfg.NetworkInterfaceConfigInPod.Interface.Addresses
		if len(addresses) != 1 {
			t.Fatalf("addresses = %v, want exactly the autoconfigured one", addresses)
		}
		got, err := netip.ParsePrefix(addresses[0])
		if err != nil {
			t.Fatal(err)
		}
		if !slaacTestPrefix.Contains(got.Addr()) || got.Bits() != slaacTestPrefix.Bits() {
			t.Errorf("address %v is not in %v", got, slaacTestPrefix)
		}
		if got.Addr().String() == strings.TrimSuffix(link.hostEUI64Address(t, slaacTestPrefix), "/64") {
			t.Errorf("the IPVLAN child got the parent's own address %v", got)
		}
		// A subinterface inherits no host routes, only the advertised ones.
		if diff := cmp.Diff(slaacTestRoutes(slaacTestRouter), cfg.NetworkInterfaceConfigInPod.Routes); diff != "" {
			t.Errorf("routes mismatch (-want +got):\n%s", diff)
		}
	})
}
