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
package controller

import (
	"context"

	// opencontrolplane-gen:if SAMPLECODE=true
	"fmt"
	"time"

	// opencontrolplane-gen:fi
	// opencontrolplane-gen:if SECRETWATCHER=true
	corev1 "k8s.io/api/core/v1"
	// opencontrolplane-gen:fi

	ctrl "sigs.k8s.io/controller-runtime"
	// opencontrolplane-gen:if SAMPLECODE=true
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	// opencontrolplane-gen:fi
	"github.com/openmcp-project/controller-utils/pkg/clusters"
	// opencontrolplane-gen:if SAMPLECODE=true
	"github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider"
	// opencontrolplane-gen:fi
	clusteraccess "github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider/clusteraccess"

	// opencontrolplane-gen:replace github.com/openmcp-project/service-provider-template=MODULE
	apiv1alpha1 "github.com/openmcp-project/service-provider-template/api/v1alpha1"
)

// opencontrolplane-gen:replace Foo=KIND
// FooReconciler reconciles a Foo object
// opencontrolplane-gen:replace Foo=KIND
type FooReconciler struct {
	// opencontrolplane-gen:replace Foo=KIND
	// OnboardingCluster is the cluster where this controller watches Foo resources and reacts to their changes.
	OnboardingCluster *clusters.Cluster
	// PlatformCluster is the cluster where this controller is deployed and configured.
	PlatformCluster *clusters.Cluster
	// PodNamespace is the namespace where this controller is deployed in.
	PodNamespace string
}

// CreateOrUpdate is called on every add or update event
// opencontrolplane-gen:replace Foo=KIND
func (r *FooReconciler) CreateOrUpdate(ctx context.Context, svcobj *apiv1alpha1.Foo, _ *apiv1alpha1.ProviderConfig, clusters clusteraccess.ClusterContext) (ctrl.Result, error) {
	// opencontrolplane-gen:if SAMPLECODE=true
	l := logf.FromContext(ctx)
	serviceprovider.StatusProgressing(svcobj, "Reconciling", "Reconcile in progress")
	managedObj := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foos.example.domain",
		},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, clusters.MCPCluster.Client(), managedObj, func() error {
		managedObj.Spec = fooCRD().Spec
		return nil
	}); err != nil {
		l.Error(err, "createOrUpdate failed")
		return ctrl.Result{}, err
	}
	serviceprovider.StatusReady(svcobj)
	// opencontrolplane-gen:fi
	// opencontrolplane-gen:if SAMPLECODE=false
	// TODO
	_, _, _ = ctx, svcobj, clusters
	// opencontrolplane-gen:fi
	return ctrl.Result{}, nil
}

// Delete is called on every delete event
// opencontrolplane-gen:replace Foo=KIND
func (r *FooReconciler) Delete(ctx context.Context, obj *apiv1alpha1.Foo, _ *apiv1alpha1.ProviderConfig, clusters clusteraccess.ClusterContext) (ctrl.Result, error) {
	// opencontrolplane-gen:if SAMPLECODE=true
	l := logf.FromContext(ctx)
	serviceprovider.StatusTerminating(obj)
	managedObj := fooCRD()
	// Check if no custom resource objects related to the managed domain service CRD remain on a ControlPlane before deleting the service provider
	fooList := &unstructured.UnstructuredList{}
	fooList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   managedObj.Spec.Group,
		Version: managedObj.Spec.Versions[0].Name,
		Kind:    managedObj.Spec.Names.ListKind,
	})
	if err := clusters.MCPCluster.Client().List(ctx, fooList); err != nil {
		if !meta.IsNoMatchError(err) {
			l.Error(err, "list Foo resources failed")
			return ctrl.Result{}, err
		}
	}
	if len(fooList.Items) != 0 {
		meta.SetStatusCondition(obj.GetConditions(), metav1.Condition{
			Type:               "DeletionBlocked",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: obj.GetGeneration(),
			Reason:             "UserResourcesPresent",
			Message:            fmt.Sprintf("user resources still present, kind %s: %d", managedObj.Spec.Names.Kind, len(fooList.Items)),
		})
		obj.SetObservedGeneration(obj.GetGeneration())
		obj.SetPhase("Terminating")

		return ctrl.Result{
			RequeueAfter: time.Second * 10,
		}, nil
	}
	if err := clusters.MCPCluster.Client().Delete(ctx, managedObj); client.IgnoreNotFound(err) != nil {
		l.Error(err, "delete object failed")
		return ctrl.Result{}, err
	}
	if err := clusters.MCPCluster.Client().Get(ctx, client.ObjectKeyFromObject(managedObj), managedObj); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// object still exists
	// opencontrolplane-gen:fi
	// opencontrolplane-gen:if SAMPLECODE=false
	// TODO
	_, _, _ = ctx, obj, clusters
	// opencontrolplane-gen:fi
	return ctrl.Result{
		// opencontrolplane-gen:if SAMPLECODE=true
		RequeueAfter: time.Second * 10,
		// opencontrolplane-gen:fi
	}, nil
}

// opencontrolplane-gen:if SECRETWATCHER=true
// IsReferencedSecret returns true if the given secret should trigger
// reconciliation. See serviceprovider.SecretWatcher for details.
//
// revive:disable:unused-parameter
// opencontrolplane-gen:replace Foo=KIND
func (r *FooReconciler) IsReferencedSecret(ctx context.Context, secret *corev1.Secret, pc *apiv1alpha1.ProviderConfig) bool {
	if pc == nil {
		return false
	}
	// TODO: Check if the secret is referenced in the provider config, for example:
	// for _, ref := range pc.Spec.ImagePullSecrets {
	//     if ref.Name == secret.Name {
	//         return true
	//     }
	// }
	return false
}

// opencontrolplane-gen:fi
// opencontrolplane-gen:if SAMPLECODE=true
func fooCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foos.example.domain",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.domain",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"foo": {Type: "string"},
									},
								},
							},
						},
					},
				},
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "foos",
				Singular: "foo",
				Kind:     "Foo",
				ListKind: "FooList",
			},
		},
	}
}

// opencontrolplane-gen:fi
