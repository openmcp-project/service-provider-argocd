package e2e

import (
	"context"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	libutils "github.com/openmcp-project/openmcp-operator/lib/utils"
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

	// Reloader test fixtures — keep in sync with the Stakater chart.
	testReloaderChartVersion = "2.2.12"
	testReloaderChartURL     = "oci://ghcr.io/stakater/charts/reloader"

	// Stable Flux resource names for the Reloader addon; match internal/argocd/constants.go.
	reloaderHelmReleaseName   = "argocd-reloader"
	reloaderOCIRepositoryName = "argocd-reloader"

	expectedDeletionBlockedMessage = "deletion blocked: ArgoCD CR(s) still present"
)

// stableTenantNamespace returns the platform-cluster namespace where Flux
// resources for the given ArgoCD CR live, using the same derivation as the
// controller (stable hash of name+namespace).
func stableTenantNamespace(t *testing.T, name, namespace string) string {
	t.Helper()
	ns, err := libutils.StableMCPNamespace(name, namespace)
	if err != nil {
		t.Fatalf("failed to derive tenant namespace for %s/%s: %v", namespace, name, err)
	}
	return ns
}

// TestServiceProvider runs the full provider lifecycle in a single feature so
// only one MCP is created and torn down. The assess steps cover:
//
//  1. Basic provisioning — ArgoCD CR becomes Ready.
//  2. Reloader addon — OCIRepository + HelmRelease are created with the correct
//     distinct managed-by label that prevents framework cascade deletion.
//  3. Exposure policy — ProviderConfig fields (DNSClass, DNSTTL, CertPurpose)
//     round-trip correctly; spec.exposure is intentionally absent from the ArgoCD
//     CR because Gardener DNS/TLS infrastructure is not available in kind.
//  4. Domain object guard — deletion is blocked while Applications exist.
//  5. Cleanup — after Applications are removed, the ArgoCD CR is deleted and both
//     the ArgoCD and Reloader HelmReleases are removed before the finalizer drops.
func TestServiceProvider(t *testing.T) {
	const mcpName = "test-controlplane"

	providerTest := features.New("provider test").
		Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiv1alpha1.AddToScheme(c.Client().Resources().GetScheme())
			helmv2.AddToScheme(c.Client().Resources().GetScheme())
			sourcev1.AddToScheme(c.Client().Resources().GetScheme())

			config := &apiv1alpha1.ProviderConfig{}
			config.SetName("argocd")
			config.Spec.Versions = []apiv1alpha1.ArgoCDVersion{
				{
					Version:      testArgoCDVersion,
					ChartVersion: testArgoCDChartVersion,
					ChartURL:     ptr.To(testArgoCDChartURL),
				},
			}
			// Reloader: installed as a best-effort addon alongside ArgoCD.
			config.Spec.Reloader = &apiv1alpha1.ReloaderConfig{
				ChartURL:     testReloaderChartURL,
				ChartVersion: testReloaderChartVersion,
			}
			// Exposure: platform-level policy. The ArgoCD CR will not request exposure
			// (no spec.exposure) because Gardener DNS/TLS infra is not available in kind.
			// This just verifies the fields are stored and returned correctly.
			config.Spec.Exposure = &apiv1alpha1.ExposurePolicy{
				DNSClass:    "garden",
				DNSTTL:      3600,
				CertPurpose: "managed",
			}
			if err := c.Client().Resources().Create(ctx, config); err != nil {
				t.Errorf("failed to create ProviderConfig: %v", err)
			}
			return ctx
		}).
		Setup(providers.CreateMCP(mcpName)).

		// ── 1. Basic provisioning ────────────────────────────────────────────────
		Assess("verify provider can be successfully consumed",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				onboardingCfg, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(onboardingCfg.Client().Resources().GetScheme())
				api := &apiv1alpha1.ArgoCD{}
				api.SetName(mcpName)
				api.SetNamespace("default")
				api.Spec.Version = testArgoCDVersion
				// spec.exposure is intentionally absent — Gardener DNS/TLS resources are not
				// available in kind; the Exposure policy in the ProviderConfig is a no-op
				// unless the tenant opts in.
				if err := onboardingCfg.Client().Resources().Create(ctx, api); err != nil {
					t.Errorf("failed to create ArgoCD object: %v", err)
				}

				// Check if the status is Progressing before Ready
				if err := wait.For(func(ctx context.Context) (bool, error) {
					if err := onboardingCfg.Client().Resources().Get(ctx, api.GetName(), api.GetNamespace(), api); err != nil {
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
				if err := wait.For(openmcpconditions.Match(api, onboardingCfg, "Ready", corev1.ConditionTrue)); err != nil {
					t.Error(err)
				}

				// Verify that the Phase is set to StatusPhaseReady
				if api.Status.Phase != "Ready" {
					t.Errorf("expected Phase to be Ready, but got: %v", api.Status.Phase)
				}
				return ctx
			},
		).

		// ── 2. Reloader addon ────────────────────────────────────────────────────
		Assess("verify Reloader Flux resources exist on platform cluster",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				ns := stableTenantNamespace(t, mcpName, "default")

				reloaderHR := &helmv2.HelmRelease{}
				if err := wait.For(func(ctx context.Context) (bool, error) {
					err := c.Client().Resources().Get(ctx, reloaderHelmReleaseName, ns, reloaderHR)
					return err == nil, nil
				}, wait.WithTimeout(2*time.Minute)); err != nil {
					t.Errorf("Reloader HelmRelease %q not found in namespace %q: %v", reloaderHelmReleaseName, ns, err)
					return ctx
				}
				// The Reloader HelmRelease must NOT carry "service-provider-argocd". If it
				// did, the framework would cascade any HelmRelease failure into ArgoCD CR deletion.
				if v := reloaderHR.GetLabels()["app.kubernetes.io/managed-by"]; v == "service-provider-argocd" {
					t.Errorf("Reloader HelmRelease has managed-by=%q; want %q to prevent framework cascade deletion", v, "service-provider-argocd-reloader")
				}

				reloaderRepo := &sourcev1.OCIRepository{}
				if err := c.Client().Resources().Get(ctx, reloaderOCIRepositoryName, ns, reloaderRepo); err != nil {
					t.Errorf("Reloader OCIRepository %q not found in namespace %q: %v", reloaderOCIRepositoryName, ns, err)
				}
				return ctx
			},
		).

		// ── 3. Exposure policy round-trip ────────────────────────────────────────
		Assess("verify ProviderConfig Exposure policy fields round-trip correctly",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				got := &apiv1alpha1.ProviderConfig{}
				if err := c.Client().Resources().Get(ctx, "argocd", "", got); err != nil {
					t.Errorf("failed to read ProviderConfig: %v", err)
					return ctx
				}
				if got.Spec.Exposure == nil {
					t.Error("ProviderConfig.Spec.Exposure is nil after create")
					return ctx
				}
				if got.Spec.Exposure.DNSClass != "garden" {
					t.Errorf("DNSClass: got %q, want %q", got.Spec.Exposure.DNSClass, "garden")
				}
				if got.Spec.Exposure.DNSTTL != 3600 {
					t.Errorf("DNSTTL: got %d, want %d", got.Spec.Exposure.DNSTTL, 3600)
				}
				if got.Spec.Exposure.CertPurpose != "managed" {
					t.Errorf("CertPurpose: got %q, want %q", got.Spec.Exposure.CertPurpose, "managed")
				}
				return ctx
			},
		).

		// ── 4. Domain object guard ───────────────────────────────────────────────
		Assess("verify domain objects can be created",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				mcpConfig, err := clusterutils.MCPConfig(ctx, c, mcpName)
				if err != nil {
					t.Error(err)
					return ctx
				}
				domainObj := newApplication()
				if err := mcpConfig.Client().Resources().Create(ctx, domainObj); err != nil {
					t.Errorf("failed to create domain object on controlplane: %v", err)
				}
				appSetObj := newApplicationSet()
				if err := mcpConfig.Client().Resources().Create(ctx, appSetObj); err != nil {
					t.Errorf("failed to create ApplicationSet on controlplane: %v", err)
				}
				appProjectObj := newAppProject()
				if err := mcpConfig.Client().Resources().Create(ctx, appProjectObj); err != nil {
					t.Errorf("failed to create AppProject on controlplane: %v", err)
				}

				return ctx
			},
		).
		Assess("verify service deletion is blocked due to existing domain service object",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				onboardingCfg, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(onboardingCfg.Client().Resources().GetScheme())
				api := &apiv1alpha1.ArgoCD{}
				api.SetName(mcpName)
				api.SetNamespace("default")
				if err := onboardingCfg.Client().Resources().Delete(ctx, api); err != nil {
					t.Errorf("failed to delete ArgoCD object: %v", err)
				}
				// verify object is stuck in Terminating
				if err := wait.For(func(ctx context.Context) (bool, error) {
					if err := onboardingCfg.Client().Resources().Get(ctx, api.GetName(), api.GetNamespace(), api); err != nil {
						return false, nil
					}
					cond := meta.FindStatusCondition(api.Status.Conditions, "Ready")
					if cond == nil {
						return false, nil
					}

					if cond.Status == metav1.ConditionFalse && cond.Message == expectedDeletionBlockedMessage && cond.Reason == "Terminating" {
						return true, nil
					}

					return false, nil
				}); err != nil {
					t.Errorf("expected deletion to be blocked with reason Terminating: %v", err)
				}
				return ctx
			},
		).
		Assess(" “delete the test created domain resources and remove the generated app finalizer.",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				onboardingCfg, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				if err := cleanupArgoCDResources(ctx, onboardingCfg); err != nil {
					t.Fatalf("failed to delete ArgoCD resources: %v", err)
				}
				return ctx
			},
		).

		// ── 5. Cleanup — ArgoCD and Reloader both removed ───────────────────────
		Assess("verify ArgoCD CR is deleted and Reloader Flux resources are removed",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				onboardingCfg, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(onboardingCfg.Client().Resources().GetScheme())
				api := &apiv1alpha1.ArgoCD{}
				api.SetName(mcpName)
				api.SetNamespace("default")
				// The ArgoCD CR was already deleted in the previous assess step. Wait for
				// the finalizer to be released. The finalizer is only dropped once both the
				// ArgoCD and Reloader HelmReleases are NotFound, so by the time this
				// succeeds, both Flux resources have already been removed.
				if err := wait.For(
					conditions.New(onboardingCfg.Client().Resources()).ResourceDeleted(api),
					wait.WithTimeout(5*time.Minute),
				); err != nil {
					t.Errorf("expected ArgoCD to be deleted after domain object removal, but it still exists: %v", err)
					return ctx
				}

				// Confirm the Reloader HelmRelease is gone — belt-and-suspenders check on top
				// of the IsUninstalled guarantee in the finalizer.
				ns := stableTenantNamespace(t, mcpName, "default")
				reloaderHR := &helmv2.HelmRelease{}
				reloaderHR.SetName(reloaderHelmReleaseName)
				reloaderHR.SetNamespace(ns)
				if err := wait.For(
					conditions.New(c.Client().Resources()).ResourceDeleted(reloaderHR),
					wait.WithTimeout(2*time.Minute),
				); err != nil {
					t.Errorf("Reloader HelmRelease was not removed when ArgoCD CR was deleted: %v", err)
				}
				return ctx
			},
		).
		Teardown(providers.DeleteMCP(mcpName, wait.WithTimeout(5*time.Minute)))
	testenv.Test(t, providerTest.Feature())
}
