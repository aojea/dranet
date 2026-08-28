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

package discovery

import (
	"context"
	"fmt"

	"cloud.google.com/go/compute/metadata"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/cloudprovider/alibaba"
	"sigs.k8s.io/dranet/pkg/cloudprovider/aws"
	"sigs.k8s.io/dranet/pkg/cloudprovider/azure"
	"sigs.k8s.io/dranet/pkg/cloudprovider/oke"
	"sigs.k8s.io/dranet/pkg/cloudprovider/webhook"
)

type CloudProviderHint string

const (
	CloudProviderHintGCE     CloudProviderHint = "GCE"
	CloudProviderHintAWS     CloudProviderHint = "AWS"
	CloudProviderHintAzure   CloudProviderHint = "AZURE"
	CloudProviderHintOKE     CloudProviderHint = "OKE"
	CloudProviderHintAlibaba CloudProviderHint = "ALIBABA"
	CloudProviderHintWebhook CloudProviderHint = "webhook"
	CloudProviderHintNone    CloudProviderHint = "NONE"
)

// DiscoverCloudProvider probes the environment to detect which cloud provider DRANET is running on.
func DiscoverCloudProvider(ctx context.Context, webhookURL string) CloudProviderHint {
	if metadata.OnGCE() {
		return CloudProviderHintGCE
	}
	if aws.OnAWS(ctx) {
		return CloudProviderHintAWS
	}
	if azure.OnAzure(ctx) {
		return CloudProviderHintAzure
	}
	if oke.OnOKE(ctx) {
		return CloudProviderHintOKE
	}
	if alibaba.OnAlibaba(ctx) {
		return CloudProviderHintAlibaba
	}
	if webhookURL != "" && webhook.OnWebhook(ctx, webhookURL) {
		return CloudProviderHintWebhook
	}
	return CloudProviderHintNone
}

// ProviderFactory constructs a specific cloud provider's CloudInstance. Each
// provider's factory closes over exactly the inputs that provider needs
// (e.g. gce.WithReservedAddresses(...)); GetInstanceProperties itself never
// holds or interprets any of them, so its signature never has to change as
// providers' construction-time needs change.
type ProviderFactory func(ctx context.Context) (cloudprovider.CloudInstance, error)

// GetInstanceProperties looks up and invokes the factory registered for hint.
// Callers build factories (see cmd/dranet/app.go) with one entry per
// discoverable CloudProviderHint.
func GetInstanceProperties(ctx context.Context, hint CloudProviderHint, factories map[CloudProviderHint]ProviderFactory) (cloudprovider.CloudInstance, error) {
	if hint == CloudProviderHintNone || hint == "none" || hint == "" {
		return nil, nil
	}
	factory, ok := factories[hint]
	if !ok {
		return nil, fmt.Errorf("unknown cloud provider hint: %s", hint)
	}
	return factory(ctx)
}
