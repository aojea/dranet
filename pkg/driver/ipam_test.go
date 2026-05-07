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
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/dranet/pkg/apis"
)

func TestLocalIPAMAllocateAndRelease(t *testing.T) {
	ipam := newLocalIPAM(nil)
	iface := &apis.InterfaceConfig{IPAM: &apis.IPAMConfig{Ranges: []string{"10.10.0.0/30"}}}

	allocated1, err := ipam.Allocate(types.NamespacedName{Namespace: "ns", Name: "claim"}, "pod-1", "dev0", iface)
	if err != nil {
		t.Fatalf("first allocation failed: %v", err)
	}
	if got, want := allocated1[0], "10.10.0.1/30"; got != want {
		t.Fatalf("unexpected first allocation %q, want %q", got, want)
	}

	iface2 := &apis.InterfaceConfig{IPAM: &apis.IPAMConfig{Ranges: []string{"10.10.0.0/30"}}}
	allocated2, err := ipam.Allocate(types.NamespacedName{Namespace: "ns", Name: "claim"}, "pod-2", "dev0", iface2)
	if err != nil {
		t.Fatalf("second allocation failed: %v", err)
	}
	if got, want := allocated2[0], "10.10.0.2/30"; got != want {
		t.Fatalf("unexpected second allocation %q, want %q", got, want)
	}

	iface3 := &apis.InterfaceConfig{IPAM: &apis.IPAMConfig{Ranges: []string{"10.10.0.0/30"}}}
	if _, err := ipam.Allocate(types.NamespacedName{Namespace: "ns", Name: "claim"}, "pod-3", "dev0", iface3); err == nil {
		t.Fatalf("expected allocation to fail when pool is exhausted")
	}

	ipam.ReleaseAllocated(allocated1)
	iface4 := &apis.InterfaceConfig{IPAM: &apis.IPAMConfig{Ranges: []string{"10.10.0.0/30"}}}
	reallocated, err := ipam.Allocate(types.NamespacedName{Namespace: "ns", Name: "claim"}, "pod-4", "dev0", iface4)
	if err != nil {
		t.Fatalf("re-allocation failed after release: %v", err)
	}
	if got, want := reallocated[0], "10.10.0.1/30"; got != want {
		t.Fatalf("unexpected re-allocation %q, want %q", got, want)
	}
}

func TestLocalIPAMRestoreFromPodConfigStore(t *testing.T) {
	store := mustNewPodConfigStore()
	store.SetDeviceConfig("pod-1", "dev0", DeviceConfig{
		Claim:                  types.NamespacedName{Namespace: "ns", Name: "claim"},
		AllocatedIPAMAddresses: []string{"10.20.0.1/30"},
	})

	ipam := newLocalIPAM(store)
	iface := &apis.InterfaceConfig{IPAM: &apis.IPAMConfig{Ranges: []string{"10.20.0.0/30"}}}
	allocated, err := ipam.Allocate(types.NamespacedName{Namespace: "ns", Name: "claim"}, "pod-2", "dev0", iface)
	if err != nil {
		t.Fatalf("allocation failed: %v", err)
	}
	if got, want := allocated[0], "10.20.0.2/30"; got != want {
		t.Fatalf("unexpected allocation %q, want %q", got, want)
	}
}
