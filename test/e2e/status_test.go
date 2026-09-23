package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/openmcp-project/openmcp-testing/pkg/clusterutils"
	"github.com/openmcp-project/openmcp-testing/pkg/providers"
	apiv1alpha1 "github.com/openmcp-project/service-provider-argocd/api/v1alpha1"
	"github.com/openmcp-project/service-provider-argocd/internal/argocd"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient/wait"

	openmcpconditions "github.com/openmcp-project/openmcp-testing/pkg/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const statusReasonInvalidVersion = "InvalidVersion"

// TestStatusFailed verifies the failure-reporting behaviour implemented by
// internal/argocd/status.go (argocd.StatusFailed). It drives the controller
// into a deterministic failure by requesting an ArgoCD version that the
// ProviderConfig does not offer, then asserts that StatusFailed set:
//   - the "Ready" condition to False,
//   - the phase to argocd.StatusPhaseFailed ("Failed"), and
//   - the reason to InvalidVersion.
func TestStatusFailed(t *testing.T) {
	statusFailedTest := features.New("status failed reporting").
		Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiv1alpha1.AddToScheme(c.Client().Resources().GetScheme())
			providerConfigName := "argocd-" + time.Now().Format("20060102150405")
			config := &apiv1alpha1.ProviderConfig{}
			config.SetName(providerConfigName)

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

			// Add cleanup logic to delete the ProviderConfig after the test
			defer func() {
				if err := c.Client().Resources().Delete(ctx, config); err != nil {
					t.Logf("failed to clean up ProviderConfig: %v", err)
				}
			}()

			return ctx
		}).
		Setup(providers.CreateMCP("test-status-failed")).
		Assess("StatusFailed sets Ready=False with reason InvalidVersion for an unknown version",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				config, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(config.Client().Resources().GetScheme())

				api := &apiv1alpha1.ArgoCD{}
				api.SetName("test-status-failed")
				api.SetNamespace("default")
				// Request a version that is not declared in the ProviderConfig so
				// the controller rejects it and calls argocd.StatusFailed.
				api.Spec.Version = "0.0.0-does-not-exist"
				if err := config.Client().Resources().Create(ctx, api); err != nil {
					t.Errorf("failed to create ArgoCD object: %v", err)
				}

				// StatusFailed sets the Ready condition to ConditionFalse.
				if err := wait.For(openmcpconditions.Match(api, config, argocd.ServiceProviderConditionReady, corev1.ConditionFalse)); err != nil {
					t.Errorf("expected Ready condition to be False: %v", err)
				}

				// StatusFailed sets the reason to the value passed by the
				// controller (InvalidVersion) for this failure path.
				if err := wait.For(func(ctx context.Context) (bool, error) {
					if err := config.Client().Resources().Get(ctx, api.GetName(), api.GetNamespace(), api); err != nil {
						return false, nil
					}
					cond := meta.FindStatusCondition(api.Status.Conditions, argocd.ServiceProviderConditionReady)
					if cond == nil {
						return false, nil
					}
					return cond.Status == metav1.ConditionFalse && cond.Reason == statusReasonInvalidVersion, nil
				}); err != nil {
					t.Errorf("expected Ready condition reason %q: %v", statusReasonInvalidVersion, err)
				}
				return ctx
			},
		).
		Assess("StatusFailed sets phase to Failed",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				config, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(config.Client().Resources().GetScheme())

				api := &apiv1alpha1.ArgoCD{}
				api.SetName("test-status-failed")
				api.SetNamespace("default")

				// StatusFailed sets the phase to argocd.StatusPhaseFailed.
				if err := wait.For(openmcpconditions.Status(api, config, "phase", argocd.StatusPhaseFailed)); err != nil {
					t.Errorf("expected phase to be %q: %v", argocd.StatusPhaseFailed, err)
				}
				return ctx
			},
		).
		Teardown(providers.DeleteMCP("test-status-failed", wait.WithTimeout(5*time.Minute)))
	testenv.Test(t, statusFailedTest.Feature())
}
