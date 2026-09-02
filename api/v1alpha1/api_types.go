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

//go:generate opencontrolplane-gen
package v1alpha1

import (
	commonapi "github.com/openmcp-project/openmcp-operator/api/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// opencontrolplane-gen:replace Foo=KIND
// FooSpec defines the desired state of Foo
// opencontrolplane-gen:replace Foo=KIND
type FooSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// opencontrolplane-gen:replace Foo=KIND
	// foo is an example field of Foo. Edit api_types.go to remove/update
	// +optional
	// opencontrolplane-gen:replace Foo=KIND
	Foo *string `json:"foo,omitempty"`
}

// opencontrolplane-gen:replace Foo=KIND
// FooStatus defines the observed state of Foo.
// opencontrolplane-gen:replace Foo=KIND
type FooStatus struct {
	commonapi.Status `json:",inline"`
}

// opencontrolplane-gen:replace Foo=KIND foo=KIND_LOWER
// Foo is the Schema for the foos API
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:JSONPath=`.status.phase`,name="Phase",type=string
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:metadata:labels="openmcp.cloud/cluster=onboarding"
// opencontrolplane-gen:replace Foo=KIND
type Foo struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// opencontrolplane-gen:replace Foo=KIND
	// spec defines the desired state of Foo
	// +required
	// opencontrolplane-gen:replace Foo=KIND
	Spec FooSpec `json:"spec"`

	// opencontrolplane-gen:replace Foo=KIND
	// status defines the observed state of Foo
	// +optional
	// opencontrolplane-gen:replace Foo=KIND
	Status FooStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// opencontrolplane-gen:replace Foo=KIND
// FooList contains a list of Foo
// opencontrolplane-gen:replace Foo=KIND
type FooList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// opencontrolplane-gen:replace Foo=KIND
	Items []Foo `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		// opencontrolplane-gen:replace Foo=KIND
		s.AddKnownTypes(GroupVersion, &Foo{}, &FooList{})
		return nil
	})
}

// opencontrolplane-gen:replace Foo=KIND
// Finalizer returns the finalizer string for the Foo resource
// opencontrolplane-gen:replace Foo=KIND
func (o *Foo) Finalizer() string {
	return GroupVersion.Group + "/finalizer"
}

// opencontrolplane-gen:replace Foo=KIND
// GetStatus returns the status of the Foo resource
// opencontrolplane-gen:replace Foo=KIND
func (o *Foo) GetStatus() any {
	return o.Status
}

// opencontrolplane-gen:replace Foo=KIND
// GetConditions returns the conditions of the Foo resource
// opencontrolplane-gen:replace Foo=KIND
func (o *Foo) GetConditions() *[]metav1.Condition {
	return &o.Status.Conditions
}

// opencontrolplane-gen:replace Foo=KIND
// SetPhase sets the phase of the Foo resource status
// opencontrolplane-gen:replace Foo=KIND
func (o *Foo) SetPhase(phase string) {
	o.Status.Phase = phase
}

// opencontrolplane-gen:replace Foo=KIND
// SetObservedGeneration sets the observed generation of the Foo resource
// opencontrolplane-gen:replace Foo=KIND
func (o *Foo) SetObservedGeneration(gen int64) {
	o.Status.ObservedGeneration = gen
}
