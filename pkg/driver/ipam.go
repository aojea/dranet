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
	"fmt"
	"net/netip"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/dranet/pkg/apis"
)

type localIPAM struct {
	mu   sync.Mutex
	used map[string]struct{}
}

func newLocalIPAM(store *PodConfigStore) *localIPAM {
	allocator := &localIPAM{used: map[string]struct{}{}}
	if store == nil {
		return allocator
	}
	for _, podUID := range store.ListPods() {
		podCfg, ok := store.GetPodConfig(podUID)
		if !ok {
			continue
		}
		for _, devCfg := range podCfg.DeviceConfigs {
			for _, addr := range devCfg.AllocatedIPAMAddresses {
				prefix, err := netip.ParsePrefix(addr)
				if err != nil {
					continue
				}
				allocator.used[prefix.Addr().String()] = struct{}{}
			}
		}
	}
	return allocator
}

func (a *localIPAM) Allocate(claim types.NamespacedName, podUID types.UID, deviceName string, iface *apis.InterfaceConfig) ([]string, error) {
	if iface == nil || iface.IPAM == nil {
		return nil, nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, cidr := range iface.IPAM.Ranges {
		pool, err := netip.ParsePrefix(cidr)
		if err != nil {
			continue
		}
		pool = pool.Masked()

		for candidate := pool.Addr(); pool.Contains(candidate); {
			if !shouldSkipCandidate(pool, candidate) {
				key := candidate.String()
				if _, exists := a.used[key]; !exists {
					a.used[key] = struct{}{}
					allocated := netip.PrefixFrom(candidate, pool.Bits()).String()
					iface.Addresses = []string{allocated}
					return []string{allocated}, nil
				}
			}

			next, ok := nextIP(candidate)
			if !ok {
				break
			}
			candidate = next
		}
	}

	return nil, fmt.Errorf("no free IP available for claim %s/%s pod %s device %s in IPAM ranges %v", claim.Namespace, claim.Name, podUID, deviceName, iface.IPAM.Ranges)
}

func (a *localIPAM) ReleaseAllocated(addresses []string) {
	if len(addresses) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, addr := range addresses {
		prefix, err := netip.ParsePrefix(addr)
		if err != nil {
			continue
		}
		delete(a.used, prefix.Addr().String())
	}
}

func shouldSkipCandidate(pool netip.Prefix, candidate netip.Addr) bool {
	if !candidate.Is4() {
		return false
	}
	// For /31 and /32, all addresses are usable.
	if pool.Bits() >= 31 {
		return false
	}
	network := pool.Masked().Addr()
	if candidate == network {
		return true
	}
	broadcast, ok := broadcastAddress(pool)
	if !ok {
		return false
	}
	return candidate == broadcast
}

func broadcastAddress(pool netip.Prefix) (netip.Addr, bool) {
	if !pool.Addr().Is4() {
		return netip.Addr{}, false
	}
	addr := pool.Masked().Addr().As4()
	hostBits := 32 - pool.Bits()
	for i := 0; i < hostBits; i++ {
		bit := i
		byteIdx := 3 - (bit / 8)
		bitIdx := bit % 8
		addr[byteIdx] |= 1 << bitIdx
	}
	return netip.AddrFrom4(addr), true
}

func nextIP(addr netip.Addr) (netip.Addr, bool) {
	if addr.Is4() {
		v4 := addr.As4()
		for i := len(v4) - 1; i >= 0; i-- {
			v4[i]++
			if v4[i] != 0 {
				return netip.AddrFrom4(v4), true
			}
		}
		return netip.Addr{}, false
	}

	v16 := addr.As16()
	for i := len(v16) - 1; i >= 0; i-- {
		v16[i]++
		if v16[i] != 0 {
			return netip.AddrFrom16(v16), true
		}
	}
	return netip.Addr{}, false
}
