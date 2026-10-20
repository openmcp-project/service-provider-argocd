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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

	// Values holds optional inline Helm chart values applied on top of any
	// ValuesFrom sources. They take precedence over ValuesFrom entries but are
	// overridden by the ProviderConfig operator values.
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`

	// ValuesFrom lists ConfigMaps or Secrets in the same namespace as this
	// resource from which Helm chart values are sourced. Entries are merged in
	// order; later entries take precedence over earlier ones. All ValuesFrom
	// values are overridden by the inline Values field and by the ProviderConfig
	// operator values.
	// +optional
	ValuesFrom []ValuesReference `json:"valuesFrom,omitempty"`
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

// ValuesReference describes a single ConfigMap or Secret on the onboarding
// cluster from which Helm chart values are sourced. It mirrors the subset of
// the Flux HelmRelease ValuesReference API that applies here.
type ValuesReference struct {
	// Kind is the type of the values referent. Supported values: ConfigMap, Secret.
	// +required
	// +kubebuilder:validation:Enum=ConfigMap;Secret
	Kind string `json:"kind"`

	// Name is the metadata name of the values referent in the same namespace
	// as the ArgoCD resource.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// ValuesKey is the data key in the referent at which the values can be
	// found. Defaults to 'values.yaml'.
	// +optional
	ValuesKey string `json:"valuesKey,omitempty"`

	// TargetPath is the YAML dot-notation path at which the value should be
	// merged. When set, the referent's ValuesKey content is treated as a single
	// scalar placed at this path rather than as a complete values object.
	// Dots in path segments can be escaped with a backslash (e.g. 'a\.b').
	// +optional
	TargetPath string `json:"targetPath,omitempty"`

	// Literal when true instructs the controller to use the value verbatim at
	// TargetPath without interpreting Helm's --set special characters (commas,
	// brackets, dots, equal signs). Mirrors helm --set-literal. Has no effect
	// when TargetPath is empty. Defaults to false.
	// +optional
	Literal bool `json:"literal,omitempty"`

	// Optional marks the reference as optional. When true a missing ConfigMap
	// or Secret is silently ignored; other errors are still propagated.
	// +optional
	Optional bool `json:"optional,omitempty"`
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
