/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Code generated by client-gen. DO NOT EDIT.

package v1

import (
	context "context"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	applyconfigurationspolicyv1 "k8s.io/client-go/applyconfigurations/policy/v1"
	gentype "k8s.io/client-go/gentype"
	scheme "k8s.io/client-go/kubernetes/scheme"
)

// PodDisruptionBudgetsGetter has a method to return a PodDisruptionBudgetInterface.
// A group's client should implement this interface.
type PodDisruptionBudgetsGetter interface {
	PodDisruptionBudgets(namespace string) PodDisruptionBudgetInterface
}

// PodDisruptionBudgetInterface has methods to work with PodDisruptionBudget resources.
type PodDisruptionBudgetInterface interface {
	Create(ctx context.Context, podDisruptionBudget *policyv1.PodDisruptionBudget, opts metav1.CreateOptions) (*policyv1.PodDisruptionBudget, error)
	Update(ctx context.Context, podDisruptionBudget *policyv1.PodDisruptionBudget, opts metav1.UpdateOptions) (*policyv1.PodDisruptionBudget, error)
	// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
	UpdateStatus(ctx context.Context, podDisruptionBudget *policyv1.PodDisruptionBudget, opts metav1.UpdateOptions) (*policyv1.PodDisruptionBudget, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*policyv1.PodDisruptionBudget, error)
	List(ctx context.Context, opts metav1.ListOptions) (*policyv1.PodDisruptionBudgetList, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *policyv1.PodDisruptionBudget, err error)
	Apply(ctx context.Context, podDisruptionBudget *applyconfigurationspolicyv1.PodDisruptionBudgetApplyConfiguration, opts metav1.ApplyOptions) (result *policyv1.PodDisruptionBudget, err error)
	// Add a +genclient:noStatus comment above the type to avoid generating ApplyStatus().
	ApplyStatus(ctx context.Context, podDisruptionBudget *applyconfigurationspolicyv1.PodDisruptionBudgetApplyConfiguration, opts metav1.ApplyOptions) (result *policyv1.PodDisruptionBudget, err error)
	PodDisruptionBudgetExpansion
}

// podDisruptionBudgets implements PodDisruptionBudgetInterface
type podDisruptionBudgets struct {
	*gentype.ClientWithListAndApply[*policyv1.PodDisruptionBudget, *policyv1.PodDisruptionBudgetList, *applyconfigurationspolicyv1.PodDisruptionBudgetApplyConfiguration]
}

// newPodDisruptionBudgets returns a PodDisruptionBudgets
func newPodDisruptionBudgets(c *PolicyV1Client, namespace string) *podDisruptionBudgets {
	return &podDisruptionBudgets{
		gentype.NewClientWithListAndApply[*policyv1.PodDisruptionBudget, *policyv1.PodDisruptionBudgetList, *applyconfigurationspolicyv1.PodDisruptionBudgetApplyConfiguration](
			"poddisruptionbudgets",
			c.RESTClient(),
			scheme.ParameterCodec,
			namespace,
			func() *policyv1.PodDisruptionBudget { return &policyv1.PodDisruptionBudget{} },
			func() *policyv1.PodDisruptionBudgetList { return &policyv1.PodDisruptionBudgetList{} },
			gentype.PrefersProtobuf[*policyv1.PodDisruptionBudget](),
		),
	}
}