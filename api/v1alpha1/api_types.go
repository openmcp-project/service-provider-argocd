/*
Copyright 2025.

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

package v1alpha1

import (
	"time"

	commonapi "github.com/openmcp-project/openmcp-operator/api/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DefaultPollInterval is used when the ProviderConfig does not specify one.
const DefaultPollInterval = time.Minute

// ArgoCDSpec defines the desired state of ArgoCD.
type ArgoCDSpec struct {
	// Version selects which ArgoCD version to install. The value must match one
	// of the versions declared in the ProviderConfig's spec.versions list.
	// +required
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// namespaceOverride overrides the default namespace into which ArgoCD is
	// installed on the target cluster. If empty, the provider's default
	// namespace is used.
	// +optional
	NamespaceOverride string `json:"namespaceOverride,omitempty"`

	// Exposure configures external access to the ArgoCD server UI/API. When left
	// unset, ArgoCD is only reachable in-cluster.
	// +optional
	Exposure *ServerExposure `json:"exposure,omitempty"`
}

// ServerExposure configures external, TLS-terminated access to the ArgoCD
// server. Its mere presence enables exposure: on Gardener the LoadBalancer
// Service is annotated so that the shoot-dns-service and shoot-cert-service
// provision DNS records and an ACME certificate automatically.
type ServerExposure struct {
	// Host is the sub-domain label (for example "argocd") or a fully-qualified
	// domain name under which the ArgoCD server will be reachable. A bare label
	// is completed with the shoot's root domain, derived from the MCP apiserver
	// host. Each DNS label is limited to 63 characters.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Host string `json:"host"`

	// AllowedIPs optionally restricts access to the LoadBalancer to the given
	// CIDR ranges. When empty, the LoadBalancer accepts traffic from any source.
	// +optional
	AllowedIPs []string `json:"allowedIPs,omitempty"`
}

// ArgoCDStatus defines the observed state of ArgoCD.
type ArgoCDStatus struct {
	commonapi.Status `json:",inline"`

	// Endpoint is the externally reachable URL of the ArgoCD server once exposure
	// has been provisioned. It is empty when exposure is disabled or not yet
	// ready.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// ArgoCD is the Schema for the argocds API
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:JSONPath=`.status.phase`,name="Phase",type=string
// +kubebuilder:printcolumn:JSONPath=`.status.endpoint`,name="Endpoint",type=string
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:metadata:labels="openmcp.cloud/cluster=onboarding"
type ArgoCD struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ArgoCD
	// +required
	Spec ArgoCDSpec `json:"spec"`

	// status defines the observed state of ArgoCD
	// +optional
	Status ArgoCDStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ArgoCDList contains a list of ArgoCD
type ArgoCDList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ArgoCD `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ArgoCD{}, &ArgoCDList{})
		return nil
	})
}

// Finalizer returns the finalizer string for the ArgoCD resource
func (o *ArgoCD) Finalizer() string {
	return GroupVersion.Group + "/finalizer"
}

// GetStatus returns the status of the ArgoCD resource
func (o *ArgoCD) GetStatus() any {
	return o.Status
}

// GetConditions returns the conditions of the ArgoCD resource
func (o *ArgoCD) GetConditions() *[]metav1.Condition {
	return &o.Status.Conditions
}

// SetPhase sets the phase of the ArgoCD resource status
func (o *ArgoCD) SetPhase(phase string) {
	o.Status.Phase = phase
}

// SetObservedGeneration sets the observed generation of the ArgoCD resource
func (o *ArgoCD) SetObservedGeneration(gen int64) {
	o.Status.ObservedGeneration = gen
}
