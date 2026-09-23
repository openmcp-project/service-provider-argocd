package e2e

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/openmcp-project/openmcp-testing/pkg/clusterutils"
	"github.com/openmcp-project/openmcp-testing/pkg/providers"

	openmcpconditions "github.com/openmcp-project/openmcp-testing/pkg/conditions"
	apiv1alpha1 "github.com/openmcp-project/service-provider-argocd/api/v1alpha1"
)

// Test fixtures for the ArgoCD version offered by the ProviderConfig and
// requested by the ArgoCD resource. Keep these in sync with the chart that is
// actually published at the given URL.
const (
	testArgoCDVersion      = "3.5.1"
	testArgoCDChartVersion = "10.4.0"
	testArgoCDChartURL     = "oci://ghcr.io/argoproj/argo-helm/argo-cd"
)

// newApplication returns a minimal but schema-valid ArgoCD Application used as
// the domain object in the e2e tests. The Application CRD requires spec fields
// (project, source, destination), so an empty object is rejected on create.
func newApplication() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-domain-object")
	obj.SetNamespace("argocd")
	obj.SetAPIVersion("argoproj.io/v1alpha1")
	obj.SetKind("Application")
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

func TestServiceProvider(t *testing.T) {
	basicProviderTest := features.New("provider test").
		Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiv1alpha1.AddToScheme(c.Client().Resources().GetScheme())
			config := &apiv1alpha1.ProviderConfig{}
			config.SetName("argocd")
			config.Spec.Versions = []apiv1alpha1.ArgoCDVersion{
				{
					Version:      testArgoCDVersion,
					ChartVersion: testArgoCDChartVersion,
					ChartURL:     ptr.To(testArgoCDChartURL),
				},
			}
			if err := c.Client().Resources().Create(ctx, config); err != nil {
				t.Errorf("failed to create ProviderConfig object: %v", err)
			}
			return ctx
		}).
		Setup(providers.CreateMCP("test-controlplane")).
		Assess("verify provider can be successfully consumed",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				config := c
				config, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(config.Client().Resources().GetScheme())
				api := &apiv1alpha1.ArgoCD{}
				api.SetName("test-controlplane")
				api.SetNamespace("default")
				api.Spec.Version = testArgoCDVersion
				if err := config.Client().Resources().Create(ctx, api); err != nil {
					t.Errorf("failed to create ArgoCD object: %v", err)
				}

				// Check if the status is Progressing before Ready
				if err := wait.For(func(ctx context.Context) (bool, error) {
					if err := config.Client().Resources().Get(ctx, api.GetName(), api.GetNamespace(), api); err != nil {
						return false, nil
					}
					if c := meta.FindStatusCondition(api.Status.Conditions, "Ready"); c != nil && c.Status == metav1.ConditionFalse && c.Reason == "Reconciling" {
						return true, nil
					}
					return false, nil
				}); err != nil {
					t.Errorf("expected status to be Progressing, but it was not: %v", err)
				}

				// Wait for the Ready condition to be True
				if err := wait.For(openmcpconditions.Match(api, config, "Ready", corev1.ConditionTrue)); err != nil {
					t.Error(err)
				}

				// Verify that the Phase is set to StatusPhaseReady
				if api.Status.Phase != "Ready" {
					t.Errorf("expected Phase to be Ready, but got: %v", api.Status.Phase)
				}
				return ctx
			},
		).
		Assess("verify domain objects can be created",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				mcpConfig, err := clusterutils.MCPConfig(ctx, c, "test-controlplane")
				if err != nil {
					t.Error(err)
					return ctx
				}
				domainObj := newApplication()
				if err := mcpConfig.Client().Resources().Create(ctx, domainObj); err != nil {
					t.Errorf("failed to create domain object on controlplane: %v", err)
				}

				return ctx
			},
		).
		Assess("verify service deletion is blocked due to existing domain service object",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				config := c
				config, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(config.Client().Resources().GetScheme())
				api := &apiv1alpha1.ArgoCD{}
				api.SetName("test-controlplane")
				api.SetNamespace("default")
				if err := config.Client().Resources().Delete(ctx, api); err != nil {
					t.Errorf("failed to delete ArgoCD object: %v", err)
				}
				// verify object is stuck in Terminating
				if err := wait.For(func(ctx context.Context) (bool, error) {
					if err := config.Client().Resources().Get(ctx, api.GetName(), api.GetNamespace(), api); err != nil {
						return false, nil
					}
					if c := meta.FindStatusCondition(api.Status.Conditions, "Ready"); c != nil && c.Status == metav1.ConditionFalse && c.Reason == "Terminating" {
						return true, nil
					}
					return false, nil
				}); err != nil {
					t.Errorf("expected deletion to be blocked with reason Terminating: %v", err)
				}
				return ctx
			},
		).
		Assess("delete domain object",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				mcpConfig, err := clusterutils.MCPConfig(ctx, c, "test-controlplane")
				if err != nil {
					t.Error(err)
					return ctx
				}
				domainObj := newApplication()
				if err := mcpConfig.Client().Resources().Delete(ctx, domainObj); err != nil {
					t.Errorf("failed to delete domain object on controlplane: %v", err)
				}
				return ctx
			},
		).
		Assess("verify service is deleted",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				config := c
				config, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(config.Client().Resources().GetScheme())
				api := &apiv1alpha1.ArgoCD{}
				api.SetName("test-controlplane")
				api.SetNamespace("default")
				if err := wait.For(conditions.New(config.Client().Resources()).ResourceDeleted(api)); err != nil {
					t.Errorf("expected ArgoCD to be deleted after domain object removal, but it still exists: %v", err)
				}
				return ctx
			},
		).
		Teardown(providers.DeleteMCP("test-controlplane", wait.WithTimeout(5*time.Minute)))
	testenv.Test(t, basicProviderTest.Feature())
}
