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
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/prometheus/client_golang/prometheus/testutil"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/inventory"
)

func TestCreateContainerRuntimeHooks(t *testing.T) {
	pod := &api.PodSandbox{Uid: "pod-hooks", Name: "test-pod", Namespace: "default", Linux: &api.LinuxPodSandbox{
		Namespaces: []*api.LinuxNamespace{{Type: "network", Path: "/var/run/netns/pod-hooks"}},
	}}
	newDriver := func(t *testing.T, hooks ...*apis.RuntimeHook) *NetworkDriver {
		t.Helper()
		np := &NetworkDriver{podConfigStore: mustNewPodConfigStore()}
		for index, hook := range hooks {
			name := "device-" + string(rune('0'+index))
			config := DeviceConfig{
				Claim:                        types.NamespacedName{Namespace: "default", Name: "claim"},
				ResourceClaim:                &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: "claim-uid"}},
				NetworkInterfaceConfigInHost: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: name}},
				NetworkInterfaceConfigInPod:  apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: name}},
				RuntimeHook:                  hook,
			}
			if err := np.podConfigStore.SetDeviceConfig(types.UID(pod.Uid), name, config); err != nil {
				t.Fatal(err)
			}
		}
		return np
	}
	t.Run("deduplicated with maximum timeout and per-device data", func(t *testing.T) {
		np := newDriver(t,
			&apis.RuntimeHook{Path: "/opt/example/hook", TimeoutSeconds: 5},
			&apis.RuntimeHook{Path: "/opt/example/hook", TimeoutSeconds: 20, Data: json.RawMessage(`{"rail":1}`)},
		)
		for attempt := 0; attempt < 100; attempt++ {
			adjust, _, err := np.CreateContainer(t.Context(), pod, &api.Container{Name: "app"})
			if err != nil {
				t.Fatal(err)
			}
			hooks := adjust.GetHooks().GetCreateRuntime()
			if len(hooks) != 1 || hooks[0].Timeout.GetValue() != 20 {
				t.Fatalf("expected one 20-second hook, got %v", hooks)
			}
			var claims []apis.HookClaim
			for _, entry := range hooks[0].Env {
				if value, found := strings.CutPrefix(entry, apis.HookEnvClaims+"="); found {
					if err := json.Unmarshal([]byte(value), &claims); err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(claims) != 1 || len(claims[0].Devices) != 2 || string(claims[0].Devices[1].Data) != `{"rail":1}` || claims[0].Claim.UID != "claim-uid" {
				t.Fatalf("unexpected claim environment: %#v", claims)
			}
		}
	})
	t.Run("empty argument is a distinct invocation", func(t *testing.T) {
		np := newDriver(t, &apis.RuntimeHook{Path: "/opt/example/hook"}, &apis.RuntimeHook{Path: "/opt/example/hook", Args: []string{""}})
		adjust, _, err := np.CreateContainer(t.Context(), pod, &api.Container{Name: "app"})
		if err != nil {
			t.Fatal(err)
		}
		hooks := adjust.GetHooks().GetCreateRuntime()
		if len(hooks) != 2 || len(hooks[0].Args) != 1 || len(hooks[1].Args) != 2 {
			t.Fatalf("unexpected invocations: %v", hooks)
		}
	})
	for _, names := range [][]string{{"app"}, {"first", "second"}} {
		t.Run("failure and retry with "+strings.Join(names, "/"), func(t *testing.T) {
			np := newDriver(t, &apis.RuntimeHook{Path: "/bin/sh", Args: []string{"-c", `if [ "$FAIL_POST_CONFIG" = "1" ]; then printf 'post-configuration failed\n' >&2; exit 1; fi`}})
			original, _ := np.podConfigStore.GetDeviceConfig(types.UID(pod.Uid), "device-0")
			for _, name := range append([]string{names[0]}, names...) {
				adjust, _, err := np.CreateContainer(t.Context(), pod, &api.Container{Name: name})
				if err != nil || len(adjust.GetHooks().GetCreateRuntime()) != 1 {
					t.Fatalf("pending hooks missing: %v, %v", adjust, err)
				}
				hook := adjust.Hooks.CreateRuntime[0]
				command := exec.Command(hook.Path, hook.Args[1:]...)
				command.Env = append(hook.Env, "FAIL_POST_CONFIG=1")
				command.Stdin = strings.NewReader(`{"id":"container-id","status":"creating"}`)
				output, err := command.CombinedOutput()
				if err == nil || !strings.Contains(string(output), "post-configuration failed") {
					t.Fatalf("expected executable failure: %q, %v", output, err)
				}
				config, _ := np.podConfigStore.GetDeviceConfig(types.UID(pod.Uid), "device-0")
				if !reflect.DeepEqual(original, config) {
					t.Fatal("failed startup changed the prepared configuration")
				}
			}
			successful := &api.Container{Name: names[len(names)-1]}
			adjust, _, err := np.CreateContainer(t.Context(), pod, successful)
			if err != nil {
				t.Fatal(err)
			}
			hook := adjust.Hooks.CreateRuntime[0]
			command := exec.Command(hook.Path, hook.Args[1:]...)
			command.Env = hook.Env
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("successful retry: %q, %v", output, err)
			}
			if err := np.StartContainer(t.Context(), pod, successful); err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				adjust, _, err := np.CreateContainer(t.Context(), pod, &api.Container{Name: name})
				if err != nil || len(adjust.GetHooks().GetCreateRuntime()) != 0 {
					t.Fatalf("completed hooks repeated: %v, %v", adjust, err)
				}
			}
			if err := np.podConfigStore.SetRuntimeHooksDone(types.UID(pod.Uid), false); err != nil {
				t.Fatal(err)
			}
			adjust, _, err = np.CreateContainer(t.Context(), pod, successful)
			if err != nil || len(adjust.GetHooks().GetCreateRuntime()) != 1 {
				t.Fatalf("reset hooks missing: %v, %v", adjust, err)
			}
		})
	}
	t.Run("chain cap", func(t *testing.T) {
		var hooks []*apis.RuntimeHook
		for index := 0; index <= apis.RuntimeHookChainMaxTimeoutSeconds/apis.RuntimeHookMaxTimeoutSeconds; index++ {
			hooks = append(hooks, &apis.RuntimeHook{Path: "/opt/example/hook" + string(rune('a'+index)), TimeoutSeconds: apis.RuntimeHookMaxTimeoutSeconds})
		}
		np := newDriver(t, hooks...)
		if _, _, err := np.CreateContainer(t.Context(), pod, &api.Container{Name: "app"}); err == nil || !strings.Contains(err.Error(), "over the maximum") {
			t.Fatalf("expected chain cap failure, got %v", err)
		}
	})
	t.Run("environment cap", func(t *testing.T) {
		np := newDriver(t, &apis.RuntimeHook{Path: "/opt/example/hook"})
		config, _ := np.podConfigStore.GetDeviceConfig(types.UID(pod.Uid), "device-0")
		config.ResourceClaim.Annotations = map[string]string{"large": strings.Repeat("x", apis.HookEnvMaxBytes)}
		if err := np.podConfigStore.SetDeviceConfig(types.UID(pod.Uid), "device-0", config); err != nil {
			t.Fatal(err)
		}
		if _, _, err := np.CreateContainer(t.Context(), pod, &api.Container{Name: "app"}); err == nil || !strings.Contains(err.Error(), "kernel allows") {
			t.Fatalf("expected environment cap failure, got %v", err)
		}
	})
}

func TestRuntimeHookCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime-hook.db")
	podUID := types.UID("hook-pod")
	config := DeviceConfig{
		Claim:         types.NamespacedName{Namespace: "default", Name: "hook-claim"},
		ResourceClaim: &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "hook-claim", Namespace: "default", UID: "hook-claim-uid"}},
		RuntimeHook:   &apis.RuntimeHook{Path: "/opt/example/hook", Args: []string{"--configure"}, TimeoutSeconds: 20, Data: json.RawMessage(`{"rail":1}`)},
	}
	store, err := NewPodConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDeviceConfig(podUID, "device", config); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeHooksDone(podUID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewPodConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config.RuntimeHookDone = true
	restored, found := store.GetDeviceConfig(podUID, "device")
	if !found || !reflect.DeepEqual(config, restored) {
		t.Fatalf("runtime checkpoint mismatch: %#v", restored)
	}
	np := &NetworkDriver{podConfigStore: store}
	pod := &api.PodSandbox{Uid: string(podUID), Name: "pod", Namespace: "default"}
	adjust, _, err := np.CreateContainer(t.Context(), pod, &api.Container{Name: "later"})
	if err != nil || len(adjust.GetHooks().GetCreateRuntime()) != 0 {
		t.Fatalf("completed hooks repeated after restart: %v, %v", adjust, err)
	}
	if err := store.SetRuntimeHooksDone(podUID, false); err != nil {
		t.Fatal(err)
	}
	adjust, _, err = np.CreateContainer(t.Context(), pod, &api.Container{Name: "new-sandbox"})
	if err != nil || len(adjust.GetHooks().GetCreateRuntime()) != 1 {
		t.Fatalf("reset hooks missing after restart: %v, %v", adjust, err)
	}
}

func TestCreateContainerNoDuplicateDevices(t *testing.T) {
	np := &NetworkDriver{
		podConfigStore: mustNewPodConfigStore(),
	}

	podUID := types.UID("test-pod")
	pod := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "test-pod",
		Namespace: "test-ns",
	}
	ctr := &api.Container{
		Name: "test-container",
	}

	// Setup pod config with duplicate RDMA devices
	rdmaDevChars := []LinuxDevice{
		{Path: "/dev/infiniband/uverbs0", Type: "c", Major: 231, Minor: 192},
	}

	deviceCfg := DeviceConfig{
		RDMADevice: RDMAConfig{
			DevChars: rdmaDevChars,
		},
	}
	np.podConfigStore.SetDeviceConfig(podUID, "eth0", deviceCfg)
	np.podConfigStore.SetDeviceConfig(podUID, "eth1", deviceCfg)

	adjust, _, err := np.CreateContainer(context.Background(), pod, ctr)
	if err != nil {
		t.Fatalf("CreateContainer failed: %v", err)
	}

	if len(adjust.Linux.Devices) != 1 {
		t.Errorf("CreateContainer should not adjust the same device multiple times\n%v", adjust.Linux.Devices)
	}
}

func TestCreateContainerUsesPersistedConfigAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pod_configs.db")
	podUID := types.UID("test-pod")
	deviceCfg := DeviceConfig{
		RDMADevice: RDMAConfig{
			DevChars: []LinuxDevice{
				{Path: "/dev/infiniband/uverbs0", Type: "c", Major: 231, Minor: 192},
			},
		},
	}

	// Simulate NodePrepareResource storing config before the driver restarts.
	cp1, err := newBoltCheckpointer(dbPath)
	if err != nil {
		t.Fatalf("newBoltCheckpointer() error: %v", err)
	}
	store1, err := newPodConfigStoreWithCheckpointer(cp1)
	if err != nil {
		t.Fatalf("NewPodConfigStore() error: %v", err)
	}
	store1.SetDeviceConfig(podUID, "eth0", deviceCfg) //nolint:errcheck
	if err := store1.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	cp2, err := newBoltCheckpointer(dbPath)
	if err != nil {
		t.Fatalf("newBoltCheckpointer() after restart error: %v", err)
	}
	storeAfterRestart, err := newPodConfigStoreWithCheckpointer(cp2)
	if err != nil {
		t.Fatalf("NewPodConfigStore() after restart error: %v", err)
	}
	defer storeAfterRestart.Close()

	np := &NetworkDriver{podConfigStore: storeAfterRestart}
	pod := &api.PodSandbox{Uid: string(podUID), Name: "test-pod", Namespace: "test-ns"}
	ctr := &api.Container{Name: "test-container"}

	adjust, _, err := np.CreateContainer(context.Background(), pod, ctr)
	if err != nil {
		t.Fatalf("CreateContainer failed: %v", err)
	}
	if adjust == nil || adjust.Linux == nil {
		t.Fatalf("CreateContainer returned nil container adjustment")
	}
	if len(adjust.Linux.Devices) != 1 {
		t.Fatalf("expected 1 injected RDMA char device after restart, got %d", len(adjust.Linux.Devices))
	}
	if got := adjust.Linux.Devices[0].Path; got != "/dev/infiniband/uverbs0" {
		t.Fatalf("unexpected injected device path %q", got)
	}
}

func TestRunPodSandboxUsesPersistedConfigAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dranet.db")
	podUID := types.UID("test-pod-sandbox")
	deviceCfg := DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns", Name: "claim1"},
		// Set a host interface name so runPodSandbox takes the netdev path,
		// which will fail (no real interface) — proving the config was found.
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "nonexistent0"},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0-pod"},
		},
	}

	// Simulate NodePrepareResource storing config before the driver restarts.
	cp1, err := newBoltCheckpointer(dbPath)
	if err != nil {
		t.Fatalf("newBoltCheckpointer() error: %v", err)
	}
	store1, err := newPodConfigStoreWithCheckpointer(cp1)
	if err != nil {
		t.Fatalf("NewPodConfigStore() error: %v", err)
	}
	if err := store1.SetDeviceConfig(podUID, "eth0", deviceCfg); err != nil {
		t.Fatalf("SetDeviceConfig() error: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	// Reopen store to simulate driver restart.
	cp2, err := newBoltCheckpointer(dbPath)
	if err != nil {
		t.Fatalf("newBoltCheckpointer() after restart error: %v", err)
	}
	storeAfterRestart, err := newPodConfigStoreWithCheckpointer(cp2)
	if err != nil {
		t.Fatalf("NewPodConfigStore() after restart error: %v", err)
	}
	defer storeAfterRestart.Close()

	np := &NetworkDriver{
		podConfigStore: storeAfterRestart,
		netdb:          inventory.New(),
		eventRecorder:  record.NewFakeRecorder(100),
	}
	pod := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "test-pod-sandbox",
		Namespace: "test-ns",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{
				{Type: "network", Path: "/var/run/netns/test"},
			},
		},
	}

	// RunPodSandbox should find the persisted config and attempt netdev
	// operations, which will fail (no real interface). An error proves the
	// config was found — a nil return would mean the config was missing.
	err = np.RunPodSandbox(context.Background(), pod)
	if err == nil {
		t.Fatal("expected RunPodSandbox to error (config found, netdev ops fail), got nil (config missing?)")
	}
}

// TestRunPodSandboxSubinterfaceCreation tests the subinterface creation
// during RunPodSandbox. It verifies the creation call by checking
// the expected error message is returned from nsCreateSubinterface.
func TestRunPodSandboxSubinterfaceCreation(t *testing.T) {
	podUID := types.UID("test-pod-subinterface")
	store := mustNewPodConfigStore()

	deviceCfg := DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns", Name: "claim1"},
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "nonexistent-parent"},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{
				Name:      "eth0",
				Type:      apis.InterfaceTypeIPVLAN,
				Addresses: []string{"2001:db8::10/64"},
			},
		},
	}

	if err := store.SetDeviceConfig(podUID, "eth0", deviceCfg); err != nil {
		t.Fatalf("SetDeviceConfig() error: %v", err)
	}

	np := &NetworkDriver{
		podConfigStore: store,
		netdb:          inventory.New(),
		eventRecorder:  record.NewFakeRecorder(100),
	}
	pod := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "test-pod-subinterface",
		Namespace: "test-ns",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{
				{Type: "network", Path: "/proc/self/ns/net"},
			},
		},
	}

	// Verify that the subinterface creation path is called by passing a non-existent parent interface
	// and asserting that the call fails with a "could not find parent interface on host" error.
	err := np.RunPodSandbox(context.Background(), pod)
	if err == nil {
		t.Fatal("expected RunPodSandbox to error, got nil")
	}

	if !strings.Contains(err.Error(), "could not find parent interface nonexistent-parent on host") {
		t.Errorf("expected error to contain 'could not find parent interface nonexistent-parent on host', got: %v", err)
	}
}

func TestSynchronizeStoresNetNSOnlyForConfiguredPods(t *testing.T) {
	store := mustNewPodConfigStore()

	// Pod 1: Has device config (configured)
	store.SetDeviceConfig("configured-pod", "eth0", DeviceConfig{}) //nolint:errcheck

	// Pod 2: Does not have device config (unconfigured)

	np := &NetworkDriver{
		podConfigStore: store,
		netdb:          inventory.New(),
	}

	pods := []*api.PodSandbox{
		{
			Uid:       "configured-pod",
			Name:      "configured",
			Namespace: "default",
			Linux: &api.LinuxPodSandbox{
				Namespaces: []*api.LinuxNamespace{
					{Type: "network", Path: "/var/run/netns/configured"},
				},
			},
		},
		{
			Uid:       "unconfigured-pod",
			Name:      "unconfigured",
			Namespace: "default",
			Linux: &api.LinuxPodSandbox{
				Namespaces: []*api.LinuxNamespace{
					{Type: "network", Path: "/var/run/netns/unconfigured"},
				},
			},
		},
	}

	_, err := np.Synchronize(context.Background(), pods, nil)
	if err != nil {
		t.Fatalf("Synchronize() error: %v", err)
	}

	// Case 1: Configured pod should have its NetNS stored
	podConfig1, found1 := store.GetPodConfig("configured-pod")
	if !found1 {
		t.Error("configured-pod should have its config stored")
	}
	if podConfig1.NetNS != "/var/run/netns/configured" {
		t.Errorf("expected NetNS /var/run/netns/configured, got %q", podConfig1.NetNS)
	}

	// Case 2: Unconfigured pod should NOT have its NetNS stored
	podConfig2, found2 := store.GetPodConfig("unconfigured-pod")
	if found2 && podConfig2.NetNS != "" {
		t.Error("unconfigured-pod should NOT have its NetNS stored")
	}

	// Also verify it didn't create a skeleton config for unconfigured-pod
	_, found3 := store.GetPodConfig("unconfigured-pod")
	if found3 {
		t.Error("unconfigured-pod should NOT have any PodConfig in the store")
	}
}

func TestCreateContainerMetrics(t *testing.T) {
	testCases := []struct {
		name           string
		podConfigStore *PodConfigStore
		expectSuccess  bool
	}{
		{
			name:           "Success",
			podConfigStore: mustNewPodConfigStore(),
			expectSuccess:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nriPluginRequestsTotal.Reset()
			nriPluginRequestsLatencySeconds.Reset()
			np := &NetworkDriver{
				podConfigStore: tc.podConfigStore,
				netdb:          inventory.New(),
			}

			podUID := types.UID("test-pod")
			pod := &api.PodSandbox{
				Uid:       string(podUID),
				Name:      "test-pod",
				Namespace: "test-ns",
			}
			ctr := &api.Container{
				Name: "test-container",
			}

			np.CreateContainer(context.Background(), pod, ctr)
			expected := `
						# HELP dranet_driver_nri_plugin_requests_latency_seconds NRI plugin request latency in seconds.
						# TYPE dranet_driver_nri_plugin_requests_latency_seconds histogram
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.005"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.01"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.025"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.05"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.25"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="2.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="10"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="+Inf"} 1
						`
			if err := testutil.CollectAndCompare(nriPluginRequestsLatencySeconds, strings.NewReader(expected), "dranet_driver_nri_plugin_requests_latency_seconds_bucket"); err != nil {
				t.Fatalf("CollectAndCompare failed: %v", err)
			}
			if tc.expectSuccess {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodCreateContainer, statusNoop)); got != float64(1) {
					t.Errorf("Expected 1 success, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodCreateContainer, statusFailed)); got != float64(0) {
					t.Errorf("Expected 0 failures, got %f", got)
				}
			} else {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodCreateContainer, statusSuccess)); got != float64(0) {
					t.Errorf("Expected 0 successes, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodCreateContainer, statusFailed)); got != float64(1) {
					t.Errorf("Expected 1 failure, got %f", got)
				}
			}
		})
	}
}

func TestRunPodSandboxMetrics(t *testing.T) {
	podUID := types.UID("test-pod")
	podUIDHostNetwork := types.UID("test-pod-host-network")

	testCases := []struct {
		name           string
		podConfigStore *PodConfigStore
		pod            *api.PodSandbox
		expectSuccess  bool
	}{
		{
			name:           "Success",
			podConfigStore: mustNewPodConfigStore(),
			pod: &api.PodSandbox{
				Uid:       string(podUID),
				Name:      "test-pod",
				Namespace: "test-ns",
				Linux: &api.LinuxPodSandbox{
					Namespaces: []*api.LinuxNamespace{
						{
							Type: "network",
							Path: "/var/run/netns/test",
						},
					},
				},
			},
			expectSuccess: true,
		},
		{
			name:           "Failure - Host Network",
			podConfigStore: mustNewPodConfigStore(),
			pod: &api.PodSandbox{
				Uid:       string(podUIDHostNetwork),
				Name:      "test-pod-host-network",
				Namespace: "test-ns",
				Linux:     &api.LinuxPodSandbox{}, // No network namespace
			},
			expectSuccess: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nriPluginRequestsTotal.Reset()
			nriPluginRequestsLatencySeconds.Reset()
			np := &NetworkDriver{
				podConfigStore: tc.podConfigStore,
				netdb:          inventory.New(),
				eventRecorder:  record.NewFakeRecorder(100),
			}
			if !tc.expectSuccess {
				tc.podConfigStore.SetDeviceConfig(podUIDHostNetwork, "eth0", DeviceConfig{})
			}

			np.RunPodSandbox(context.Background(), tc.pod)
			status := statusSuccess
			if !tc.expectSuccess {
				status = statusFailed
			}
			expected := `
						# HELP dranet_driver_nri_plugin_requests_latency_seconds NRI plugin request latency in seconds.
						# TYPE dranet_driver_nri_plugin_requests_latency_seconds histogram
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.005"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.01"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.025"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.05"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.25"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="2.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="10"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="+Inf"} 1
						`
			expected = strings.Replace(expected, `method="RunPodSandbox"`, `method="RunPodSandbox",status="`+status+`"`, -1)
			if err := testutil.CollectAndCompare(nriPluginRequestsLatencySeconds, strings.NewReader(expected), "dranet_driver_nri_plugin_requests_latency_seconds_bucket"); err != nil {
				t.Fatalf("CollectAndCompare failed: %v", err)
			}
			if tc.expectSuccess {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, statusNoop)); got != float64(1) {
					t.Errorf("Expected 1 success, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, statusFailed)); got != float64(0) {
					t.Errorf("Expected 0 failures, got %f", got)
				}
			} else {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, statusSuccess)); got != float64(0) {
					t.Errorf("Expected 0 successes, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, statusFailed)); got != float64(1) {
					t.Errorf("Expected 1 failure, got %f", got)
				}
			}
		})
	}
}

func TestStopPodSandboxMetrics(t *testing.T) {
	testCases := []struct {
		name           string
		podConfigStore *PodConfigStore
		expectSuccess  bool
	}{
		{
			name:           "Success",
			podConfigStore: mustNewPodConfigStore(),
			expectSuccess:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nriPluginRequestsTotal.Reset()
			nriPluginRequestsLatencySeconds.Reset()
			np := &NetworkDriver{
				podConfigStore: tc.podConfigStore,
				netdb:          inventory.New(),
			}
			podUID := types.UID("test-pod")
			pod := &api.PodSandbox{
				Uid:       string(podUID),
				Name:      "test-pod",
				Namespace: "test-ns",
			}

			np.StopPodSandbox(context.Background(), pod)
			expected := `
						# HELP dranet_driver_nri_plugin_requests_latency_seconds NRI plugin request latency in seconds.
						# TYPE dranet_driver_nri_plugin_requests_latency_seconds histogram
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.005"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.01"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.025"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.05"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.25"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="2.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="10"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="+Inf"} 1
						`
			if err := testutil.CollectAndCompare(nriPluginRequestsLatencySeconds, strings.NewReader(expected), "dranet_driver_nri_plugin_requests_latency_seconds_bucket"); err != nil {
				t.Fatalf("CollectAndCompare failed: %v", err)
			}
			if tc.expectSuccess {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodStopPodSandbox, statusNoop)); got != float64(1) {
					t.Errorf("Expected 1 success, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodStopPodSandbox, statusFailed)); got != float64(0) {
					t.Errorf("Expected 0 failures, got %f", got)
				}
			} else {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodStopPodSandbox, statusSuccess)); got != float64(0) {
					t.Errorf("Expected 0 successes, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodStopPodSandbox, statusFailed)); got != float64(1) {
					t.Errorf("Expected 1 failure, got %f", got)
				}
			}
		})
	}
}

// TestNeedsRescanAfterDetach checks the gating logic that decides whether
// stopPodSandbox needs to fire an explicit inventory rescan after returning
// a device's RDMA / netdev to init_net.
func TestNeedsRescanAfterDetach(t *testing.T) {
	testCases := []struct {
		name           string
		rdmaDetached   bool
		netdevDetached bool
		want           bool
	}{
		{
			name:           "neither detached: nothing new in init_net",
			rdmaDetached:   false,
			netdevDetached: false,
			want:           false,
		},
		{
			name:           "netdev only: NEWLINK covers any rescan need",
			rdmaDetached:   false,
			netdevDetached: true,
			want:           false,
		},
		{
			name:           "RDMA only (IB-only success, or SR-IOV with netdev failure): rescan needed",
			rdmaDetached:   true,
			netdevDetached: false,
			want:           true,
		},
		{
			name:           "both detached (SR-IOV success): NEWLINK covers it",
			rdmaDetached:   true,
			netdevDetached: true,
			want:           false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsRescanAfterDetach(tc.rdmaDetached, tc.netdevDetached); got != tc.want {
				t.Errorf("needsRescanAfterDetach(%v, %v) = %v, want %v",
					tc.rdmaDetached, tc.netdevDetached, got, tc.want)
			}
		})
	}
}

// TestStopPodSandboxRescanGating verifies the integration paths in
// stopPodSandbox where a rescan must NOT be requested. The success path
// (where a detach actually completes) cannot be exercised here because the
// real netlink calls fail against synthetic netns paths; that condition is
// covered by TestNeedsRescanAfterDetach.
func TestStopPodSandboxRescanGating(t *testing.T) {
	testCases := []struct {
		name              string
		setupDeviceConfig bool
		deviceConfig      DeviceConfig
		setupNetNs        bool
		rdmaSharedMode    bool
	}{
		{
			name:              "no device config: early return at NRI level",
			setupDeviceConfig: false,
			setupNetNs:        false,
		},
		{
			name:              "host network pod: stopPodSandbox skips before the loop",
			setupDeviceConfig: true,
			deviceConfig:      DeviceConfig{RDMADevice: RDMAConfig{LinkDev: "mlx5_0"}},
			setupNetNs:        false,
		},
		{
			name:              "shared RDMA mode: RDMA branch is skipped",
			setupDeviceConfig: true,
			deviceConfig:      DeviceConfig{RDMADevice: RDMAConfig{LinkDev: "mlx5_0"}},
			setupNetNs:        true,
			rdmaSharedMode:    true,
		},
		{
			name:              "exclusive RDMA + fake netns: detach fails, no rescan",
			setupDeviceConfig: true,
			deviceConfig:      DeviceConfig{RDMADevice: RDMAConfig{LinkDev: "mlx5_0"}},
			setupNetNs:        true,
		},
		{
			name:              "subinterface configured: no rescan triggered",
			setupDeviceConfig: true,
			deviceConfig: DeviceConfig{
				NetworkInterfaceConfigInPod: apis.NetworkConfig{
					Interface: apis.InterfaceConfig{
						Name: "ipvl-eth0",
						Type: apis.InterfaceTypeIPVLAN,
					},
				},
			},
			setupNetNs: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nriPluginRequestsTotal.Reset()
			nriPluginRequestsLatencySeconds.Reset()
			netdb := newFakeInventoryDB()
			np := &NetworkDriver{
				podConfigStore: mustNewPodConfigStore(),
				netdb:          netdb,
				rdmaSharedMode: tc.rdmaSharedMode,
				eventRecorder:  record.NewFakeRecorder(100),
			}
			podUID := types.UID("test-pod")
			pod := &api.PodSandbox{
				Uid:       string(podUID),
				Name:      "test-pod",
				Namespace: "test-ns",
			}
			if tc.setupDeviceConfig {
				np.podConfigStore.SetDeviceConfig(podUID, "eth0", tc.deviceConfig)
			}
			if tc.setupDeviceConfig && tc.setupNetNs {
				np.podConfigStore.SetPodNetNs(podUID, "/dummy/netns")
			}

			if err := np.StopPodSandbox(context.Background(), pod); err != nil {
				t.Fatalf("StopPodSandbox() error = %v", err)
			}
			if got := netdb.rescanCalls.Load(); got != 0 {
				t.Errorf("RequestRescan call count = %d, want 0", got)
			}
		})
	}
}

func TestRemovePodSandboxMetrics(t *testing.T) {
	testCases := []struct {
		name           string
		podConfigStore *PodConfigStore
		expectSuccess  bool
	}{
		{
			name:           "Success",
			podConfigStore: mustNewPodConfigStore(),
			expectSuccess:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nriPluginRequestsTotal.Reset()
			nriPluginRequestsLatencySeconds.Reset()
			np := &NetworkDriver{
				podConfigStore: tc.podConfigStore,
				netdb:          inventory.New(),
			}
			podUID := types.UID("test-pod")
			pod := &api.PodSandbox{
				Uid:       string(podUID),
				Name:      "test-pod",
				Namespace: "test-ns",
			}

			np.RemovePodSandbox(context.Background(), pod)
			expected := `
						# HELP dranet_driver_nri_plugin_requests_latency_seconds NRI plugin request latency in seconds.
						# TYPE dranet_driver_nri_plugin_requests_latency_seconds histogram
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.005"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.01"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.025"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.05"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.25"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="2.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="10"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="+Inf"} 1
						`
			if err := testutil.CollectAndCompare(nriPluginRequestsLatencySeconds, strings.NewReader(expected), "dranet_driver_nri_plugin_requests_latency_seconds_bucket"); err != nil {
				t.Fatalf("CollectAndCompare failed: %v", err)
			}
			if tc.expectSuccess {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRemovePodSandbox, statusNoop)); got != float64(1) {
					t.Errorf("Expected 1 success, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRemovePodSandbox, statusFailed)); got != float64(0) {
					t.Errorf("Expected 0 failures, got %f", got)
				}
			} else {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRemovePodSandbox, statusSuccess)); got != float64(0) {
					t.Errorf("Expected 0 successes, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRemovePodSandbox, statusFailed)); got != float64(1) {
					t.Errorf("Expected 1 failure, got %f", got)
				}
			}
		})
	}
}

// TestCreateContainerSubinterfaceRDMAInjection verifies that the
// RDMA character devices are correctly injected into the container
// adjustment when the pod is configured to use a subinterface.
func TestCreateContainerSubinterfaceRDMAInjection(t *testing.T) {
	podUID := types.UID("test-pod-subinterface")
	deviceCfg := DeviceConfig{
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{
				Name:      "ipvl-eth0",
				Type:      apis.InterfaceTypeIPVLAN,
				Addresses: []string{"2001:db8::10/64"},
			},
		},
		RDMADevice: RDMAConfig{
			DevChars: []LinuxDevice{
				{Path: "/dev/infiniband/uverbs0", Type: "c", Major: 231, Minor: 192},
				{Path: "/dev/infiniband/rdma_cm", Type: "c", Major: 10, Minor: 58},
			},
		},
	}

	store := mustNewPodConfigStore()
	if err := store.SetDeviceConfig(podUID, "eth0", deviceCfg); err != nil {
		t.Fatalf("SetDeviceConfig() error: %v", err)
	}

	np := &NetworkDriver{podConfigStore: store}
	pod := &api.PodSandbox{Uid: string(podUID), Name: "test-pod-subinterface", Namespace: "test-ns"}
	ctr := &api.Container{Name: "test-container"}

	adjust, _, err := np.CreateContainer(context.Background(), pod, ctr)
	if err != nil {
		t.Fatalf("CreateContainer failed: %v", err)
	}
	if adjust == nil || adjust.Linux == nil {
		t.Fatalf("CreateContainer returned nil container adjustment")
	}
	if len(adjust.Linux.Devices) != 2 {
		t.Fatalf("expected 2 injected RDMA char devices, got %d", len(adjust.Linux.Devices))
	}

	expectedPaths := []string{"/dev/infiniband/uverbs0", "/dev/infiniband/rdma_cm"}
	for i, expectedPath := range expectedPaths {
		if got := adjust.Linux.Devices[i].Path; got != expectedPath {
			t.Errorf("expected device %d path to be %q, got %q", i, expectedPath, got)
		}
	}
}
