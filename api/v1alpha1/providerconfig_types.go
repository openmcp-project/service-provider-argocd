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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ProviderConfigSpec defines the desired state of ProviderConfig
type ProviderConfigSpec struct {
	// Versions specify the valid inputs for the ArgoCD.Spec.Version field.
	// Each entry describes an installable ArgoCD version and its deployment
	// artifacts (Helm chart coordinates and values).
	// +required
	Versions []ArgoCDVersion `json:"versions"`

	// PollInterval determines how often to reconcile resources to prevent drift.
	// +optional
	// +kubebuilder:default:="1m"
	// +kubebuilder:validation:Format=duration
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`

	// Exposure defines platform-level policy applied when a tenant requests
	// external exposure of the ArgoCD server (DNS class, TTL and certificate
	// purpose). Tenants only choose the hostname and source ranges; the platform
	// owns these landscape-wide settings.
	// +optional
	Exposure *ExposurePolicy `json:"exposure,omitempty"`

	// Reloader optionally installs the Stakater Reloader controller alongside
	// ArgoCD on each Managed Control Plane. When set, the provider declares a
	// dedicated OCIRepository + HelmRelease for it in the same namespace as
	// ArgoCD so that argocd-server is automatically restarted when its
	// Gardener-managed TLS certificate rotates. When nil, Reloader is not
	// installed. Reloader is treated as a best-effort addon: its failures are
	// logged but never affect the lifecycle of the ArgoCD CR.
	// +optional
	Reloader *ReloaderConfig `json:"reloader,omitempty"`
}

// ReloaderConfig describes the Stakater Reloader Helm chart to install
// alongside ArgoCD on each Managed Control Plane.
type ReloaderConfig struct {
	// Version is an informational label for the Reloader release (e.g. the
	// Reloader app version). It is not used to select artifacts; ChartVersion
	// governs what is installed.
	// +optional
	Version string `json:"version,omitempty"`

	// ChartURL is the OCI registry URL for the Reloader Helm chart.
	// +optional
	// +kubebuilder:default="oci://ghcr.io/stakater/charts/reloader"
	ChartURL string `json:"chartUrl,omitempty"`

	// ChartVersion is the OCI tag of the Reloader Helm chart to install.
	// +required
	ChartVersion string `json:"chartVersion"`

	// ChartPullSecret is the name of a Secret in the service provider's
	// namespace containing credentials to pull the chart from a private OCI
	// registry.
	// +optional
	ChartPullSecret string `json:"chartPullSecret,omitempty"`

	// Values contains Helm values to override the Reloader chart defaults.
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
}

// ExposurePolicy captures the Gardener-specific, landscape-wide settings used
// when exposing an ArgoCD server. All fields have sensible defaults.
type ExposurePolicy struct {
	// DNSClass is the Gardener DNS class used for managed DNS records.
	// +optional
	// +kubebuilder:default="garden"
	DNSClass string `json:"dnsClass,omitempty"`

	// DNSTTL is the TTL, in seconds, for managed DNS records.
	// +optional
	// +kubebuilder:default=3600
	DNSTTL int `json:"dnsTTL,omitempty"`

	// CertPurpose is the value of the cert.gardener.cloud/purpose annotation.
	// +optional
	// +kubebuilder:default="managed"
	CertPurpose string `json:"certPurpose,omitempty"`
}

// ArgoCDVersion defines a version of ArgoCD that can be installed.
type ArgoCDVersion struct {
	// Version is the ArgoCD version to install.
	// This value is compared with ArgoCD.Spec.Version to define the available
	// versions and the deployment artifacts of a version.
	// +required
	Version string `json:"version"`

	// ChartVersion is the version of the Helm chart to install.
	// +required
	ChartVersion string `json:"chartVersion"`

	// ChartURL is the OCI registry URL for the ArgoCD Helm chart.
	// +optional
	// +kubebuilder:default="oci://ghcr.io/argoproj/argo-helm/argo-cd"
	ChartURL *string `json:"chartUrl,omitempty"`

	// ChartPullSecret is the name of a Secret in the service provider's namespace
	// containing credentials to pull the Helm chart from a private OCI registry.
	// The secret must be of type kubernetes.io/dockerconfigjson.
	// +optional
	ChartPullSecret string `json:"chartPullSecret,omitempty"`

	// Values contains Helm values to override defaults for the ArgoCD deployment.
	// This field supports all configuration options from the ArgoCD Helm chart.
	//
	// Image pull secrets for ArgoCD components should be specified via
	// values.imagePullSecrets. Any secrets referenced there will be copied from
	// the service provider's namespace to the ArgoCD namespace on the
	// ManagedControlPlane.
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
}

// ProviderConfigStatus defines the observed state of ProviderConfig.
type ProviderConfigStatus struct {
	// Conditions represent the current state of the ProviderConfig resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ProviderConfig is the Schema for the providerconfigs API
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:labels="openmcp.cloud/cluster=platform"
type ProviderConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ProviderConfig
	// +required
	Spec ProviderConfigSpec `json:"spec"`

	// status defines the observed state of ProviderConfig
	// +optional
	Status ProviderConfigStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ProviderConfigList contains a list of ProviderConfig
type ProviderConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProviderConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ProviderConfig{}, &ProviderConfigList{})
		return nil
	})
}

// PollInterval returns the poll interval duration from the spec, falling back
// to DefaultPollInterval when it is not set.
func (o *ProviderConfig) PollInterval() time.Duration {
	if o.Spec.PollInterval == nil {
		return DefaultPollInterval
	}
	return o.Spec.PollInterval.Duration
}

// SelectVersion returns the ArgoCDVersion matching the requested version from
// the ProviderConfig's spec.versions list. The second return value is false if
// no matching version is configured.
func (o *ProviderConfig) SelectVersion(requestedVersion string) (ArgoCDVersion, bool) {
	for _, v := range o.Spec.Versions {
		if v.Version == requestedVersion {
			return v, true
		}
	}
	return ArgoCDVersion{}, false
}

// Default values for the exposure policy.
const (
	DefaultDNSClass    = "garden"
	DefaultDNSTTL      = 3600
	DefaultCertPurpose = "managed"

	// DefaultReloaderChartURL is the public Stakater Reloader OCI chart URL.
	DefaultReloaderChartURL = "oci://ghcr.io/stakater/charts/reloader"
)

// ExposurePolicy returns the effective exposure policy, filling in defaults for
// any unset field and tolerating a nil spec.exposure.
func (o *ProviderConfig) ExposurePolicy() ExposurePolicy {
	policy := ExposurePolicy{
		DNSClass:    DefaultDNSClass,
		DNSTTL:      DefaultDNSTTL,
		CertPurpose: DefaultCertPurpose,
	}
	if o == nil || o.Spec.Exposure == nil {
		return policy
	}
	if o.Spec.Exposure.DNSClass != "" {
		policy.DNSClass = o.Spec.Exposure.DNSClass
	}
	if o.Spec.Exposure.DNSTTL != 0 {
		policy.DNSTTL = o.Spec.Exposure.DNSTTL
	}
	if o.Spec.Exposure.CertPurpose != "" {
		policy.CertPurpose = o.Spec.Exposure.CertPurpose
	}
	return policy
}

// ReloaderConfig returns the effective Reloader configuration and whether
// Reloader is enabled. ChartURL is defaulted when omitted.
func (o *ProviderConfig) ReloaderConfig() (ReloaderConfig, bool) {
	if o == nil || o.Spec.Reloader == nil {
		return ReloaderConfig{}, false
	}
	cfg := *o.Spec.Reloader
	if cfg.ChartURL == "" {
		cfg.ChartURL = DefaultReloaderChartURL
	}
	return cfg, true
}
