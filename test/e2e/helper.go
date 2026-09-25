package e2e

import (
	"context"
	"fmt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

type argoResourceRef struct {
	id         string
	apiVersion string
	kind       string
	name       string
	namespace  string
}

func (r argoResourceRef) Object() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(r.apiVersion)
	obj.SetKind(r.kind)
	obj.SetName(r.name)
	obj.SetNamespace(r.namespace)
	return obj
}

func (r argoResourceRef) String() string {
	return r.kind + "/" + r.namespace + "/" + r.name
}

var (
	testDomainApplication = argoResourceRef{
		id:         "test-domain-object",
		apiVersion: "argoproj.io/v1alpha1",
		kind:       "Application",
		name:       "test-domain-object",
		namespace:  "argocd",
	}

	generatedGuestbookApplication = argoResourceRef{
		id:         "generated-guestbook",
		apiVersion: "argoproj.io/v1alpha1",
		kind:       "Application",
		name:       "generated-guestbook",
		namespace:  "argocd",
	}

	testApplicationSet = argoResourceRef{
		id:         "test-applicationset",
		apiVersion: "argoproj.io/v1alpha1",
		kind:       "ApplicationSet",
		name:       "test-applicationset",
		namespace:  "argocd",
	}

	testAppProject = argoResourceRef{
		id:         "test-appproject",
		apiVersion: "argoproj.io/v1alpha1",
		kind:       "AppProject",
		name:       "test-appproject",
		namespace:  "argocd",
	}

	defaultAppProject = argoResourceRef{
		id:         "default-appproject",
		apiVersion: "argoproj.io/v1alpha1",
		kind:       "AppProject",
		name:       "default",
		namespace:  "argocd",
	}

	// argoResourcesList contains references to ArgoCD resources relevant to the e2e scenario.
	// Each entry is immutable metadata used to construct a fresh unstructured object when needed
	// for lookup, deletion, or finalizer cleanup.
	argoResourcesToCleanup = []argoResourceRef{
		testDomainApplication,
		generatedGuestbookApplication,
		testApplicationSet,
		testAppProject,
		defaultAppProject,
	}
)

// newApplication returns a minimal but schema-valid ArgoCD Application used as
// the domain object in the e2e tests. The Application CRD requires spec fields
// (project, source, destination), so an empty object is rejected on create.
func newApplication() *unstructured.Unstructured {
	obj := testDomainApplication.Object()
	obj.Object["spec"] = map[string]interface{}{
		"project": "default",
		"source": map[string]interface{}{
			"repoURL":        "https://github.com/argoproj/argocd-example-apps.git",
			"path":           "guestbook",
			"targetRevision": "HEAD",
		},
		"destination": map[string]interface{}{
			"server":    "https://kubernetes.default.svc",
			"namespace": "default",
		},
	}
	return obj
}

// newApplicationSet returns a minimal but schema-valid ArgoCD ApplicationSet used as
// the domain object in the e2e tests. The ApplicationSet CRD requires spec fields
// (generators, template), so an empty object is rejected on create.
func newApplicationSet() *unstructured.Unstructured {
	obj := testApplicationSet.Object()
	obj.Object["spec"] = map[string]interface{}{
		"generators": []interface{}{
			map[string]interface{}{
				"list": map[string]interface{}{
					"elements": []interface{}{
						map[string]interface{}{
							"cluster":   "in-cluster",
							"url":       "https://kubernetes.default.svc",
							"namespace": "default",
						},
					},
				},
			},
		},
		"template": map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "generated-guestbook",
			},
			"spec": map[string]interface{}{
				"project": "default",
				"source": map[string]interface{}{
					"repoURL":        "https://github.com/argoproj/argocd-example-apps.git",
					"path":           "fake-path",
					"targetRevision": "HEAD",
				},
				"destination": map[string]interface{}{
					"server":    "https://kubernetes.default.svc",
					"namespace": "default",
				},
			},
		},
	}
	return obj
}

// newAppProject returns a minimal but schema-valid ArgoCD AppProject used as
// the domain object in the e2e tests. The AppProject CRD requires spec fields
// (description, sourceRepos, destinations, clusterResourceWhitelist, namespaceResourceWhitelist),
// so an empty object is rejected on create.
func newAppProject() *unstructured.Unstructured {
	obj := testAppProject.Object()
	obj.Object["spec"] = map[string]interface{}{
		"description": "test project for e2e deletion blocking",
		"sourceRepos": []interface{}{
			"*",
		},
		"destinations": []interface{}{
			map[string]interface{}{
				"server":    "https://kubernetes.default.svc",
				"namespace": "*",
			},
		},
		"clusterResourceWhitelist": []interface{}{
			map[string]interface{}{
				"group": "*",
				"kind":  "*",
			},
		},
		"namespaceResourceWhitelist": []interface{}{
			map[string]interface{}{
				"group": "*",
				"kind":  "*",
			},
		},
	}
	return obj
}

// cleanupArgoCDResources deletes all ArgoCD domain objects that were created in the e2e tests.
// It iterates over the argoResourcesList and attempts to delete each object. If an object is not found,
// it continues to the next one. After deleting all objects, it removes the finalizer of
// the generated guestbook application (generatedGuestbookApplication.Object()) to allow for proper cleanup.
func cleanupArgoCDResources(ctx context.Context, c *envconf.Config) error {
	for _, ref := range argoResourcesToCleanup {
		obj := ref.Object()
		err := c.Client().Resources().Get(ctx, obj.GetName(), obj.GetNamespace(), obj)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("get %s: %w", ref, err)
		}
		if err := c.Client().Resources().Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s: %w", ref, err)
		}
	}

	if err := deleteResourceFinalizer(ctx, c, generatedGuestbookApplication.Object()); err != nil {
		return err
	}

	return nil
}

// deleteResourceFinalizer removes the finalizer from the given unstructured object.
// If the object is not found, it simply returns without error.
func deleteResourceFinalizer(ctx context.Context, c *envconf.Config, obj *unstructured.Unstructured) error {
	err := c.Client().Resources().Get(ctx, obj.GetName(), obj.GetNamespace(), obj)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	if len(obj.GetFinalizers()) > 0 {
		obj.SetFinalizers(nil)
		if err := c.Client().Resources().Update(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
