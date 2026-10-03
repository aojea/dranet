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

package apis

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
)

// Contract of the provider OCI createRuntime hooks DRANET adds to a Pod's
// containers after native device attachment and configuration. Hooks receive
// the Pod's claims and devices in the same models as the provider.
//
// Hooks are scoped to the Pod's network namespace. The environment names the
// Pod and its devices, never a container, and a hook must not use the OCI
// container state the runtime writes on its stdin.
const (
	// Environment of the hook process.
	HookEnvPodUID       = "DRANET_POD_UID"
	HookEnvPodNamespace = "DRANET_POD_NAMESPACE"
	HookEnvPodName      = "DRANET_POD_NAME"
	// HookEnvNetNS is the path of the Pod's network namespace.
	HookEnvNetNS = "DRANET_NETNS"
	// HookEnvClaims is a JSON array with one HookClaim per ResourceClaim of
	// the Pod.
	HookEnvClaims = "DRANET_CLAIMS"
)

// HookEnvMaxBytes is the most one environment string (name, '=' and value)
// of the hook may take: the kernel refuses to execute a process with a longer
// one (MAX_ARG_STRLEN). The claims of a Pod have to fit in it.
const HookEnvMaxBytes = 128 * 1024

// HookClaim is one ResourceClaim of the Pod with the devices DRANET handles
// for it. Each claim appears once: the claim is the large part of the
// environment, which HookEnvMaxBytes bounds.
type HookClaim struct {
	// Claim is the ResourceClaim as the providers received it at prepare
	// time, without managedFields.
	Claim *resourceapi.ResourceClaim `json:"claim"`
	// Devices are the claim's devices, sorted by name.
	Devices []HookDevice `json:"devices"`
}

// HookDevice is one device of a claim.
type HookDevice struct {
	// Name is the device name in the ResourceSlice and the ResourceClaim.
	Name string `json:"name"`
	// Device identifies the device on the host, as the providers see it.
	Device DeviceIdentifiers `json:"device"`
	// Config is the network configuration DRANET applies inside the Pod;
	// unset for devices that only expose RDMA character devices.
	Config *NetworkConfig `json:"config,omitempty"`
	// Data is the opaque data the profile provider attached to its runtime
	// hook for this device (RuntimeHook.Data). DRANET does not interpret it.
	Data json.RawMessage `json:"data,omitempty"`
}

// The hooks of a container run inside the start of the Pod's first container,
// which the kubelet bounds with its runtime request timeout (2m by default),
// so every timeout below is capped and the caps add up to well under it.
const (
	// RuntimeHookDefaultTimeoutSeconds applies when the provider sets none.
	RuntimeHookDefaultTimeoutSeconds = 10
	// RuntimeHookMaxTimeoutSeconds is the most a provider hook may ask for.
	RuntimeHookMaxTimeoutSeconds = 30
	// RuntimeHookChainMaxTimeoutSeconds bounds the sum of provider hook
	// timeouts inside one container startup request.
	RuntimeHookChainMaxTimeoutSeconds = 90
)

// RuntimeHookDataMaxBytes bounds RuntimeHook.Data: the data of every device
// travels in the hook environment, which HookEnvMaxBytes bounds as a whole.
const RuntimeHookDataMaxBytes = 4 * 1024

// RuntimeHook is a profile provider's decision to run a binary on the node
// before the Pod's first successful container startup and after DRANET
// attached and configured the Pod's devices, to apply post-configuration to
// the Pod's network namespace. The binary receives the HookEnv* environment,
// with Data in its device's entry of HookEnvClaims. It may only change
// network state in the namespace at HookEnvNetNS; it must not
// use the OCI container state on its stdin. It must be idempotent: it runs
// again when the kubelet retries a failed startup or tries another container. A
// non-zero exit fails the container start with its stderr in the Pod's
// events.
type RuntimeHook struct {
	// Path is the absolute path of the binary on the host.
	Path string `json:"path"`
	// Args are the arguments of the binary, after its name.
	Args []string `json:"args,omitempty"`
	// TimeoutSeconds is how long the runtime lets the hook run before it
	// kills it and fails the container start. Defaults to
	// RuntimeHookDefaultTimeoutSeconds; at most RuntimeHookMaxTimeoutSeconds.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// Data is passed to the hook as is, in its device's HookDevice.Data; at
	// most RuntimeHookDataMaxBytes.
	Data json.RawMessage `json:"data,omitempty"`
}

// Validate checks the hook and applies the timeout default.
func (h *RuntimeHook) Validate() error {
	if h.Path == "" {
		return fmt.Errorf("runtime hook path is empty")
	}
	if !filepath.IsAbs(h.Path) {
		return fmt.Errorf("runtime hook path %q is not absolute", h.Path)
	}
	if strings.ContainsRune(h.Path, '\x00') {
		return fmt.Errorf("runtime hook path contains a NUL byte")
	}
	for index, arg := range h.Args {
		if strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("runtime hook argument %d contains a NUL byte", index)
		}
		if len(arg)+1 > HookEnvMaxBytes {
			return fmt.Errorf("runtime hook argument %d is over the maximum of %d bytes", index, HookEnvMaxBytes)
		}
	}
	if h.TimeoutSeconds < 0 {
		return fmt.Errorf("runtime hook timeout %d is negative", h.TimeoutSeconds)
	}
	if h.TimeoutSeconds == 0 {
		h.TimeoutSeconds = RuntimeHookDefaultTimeoutSeconds
	}
	if h.TimeoutSeconds > RuntimeHookMaxTimeoutSeconds {
		return fmt.Errorf("runtime hook timeout %ds is over the maximum of %ds", h.TimeoutSeconds, RuntimeHookMaxTimeoutSeconds)
	}
	if len(h.Data) > 0 && !json.Valid(h.Data) {
		return fmt.Errorf("runtime hook data is not valid JSON")
	}
	if len(h.Data) > RuntimeHookDataMaxBytes {
		return fmt.Errorf("runtime hook data is %d bytes, over the maximum of %d", len(h.Data), RuntimeHookDataMaxBytes)
	}
	return nil
}
