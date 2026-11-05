package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/openmcp-project/openmcp-testing/pkg/clusterutils"
	openmcpconditions "github.com/openmcp-project/openmcp-testing/pkg/conditions"
	"github.com/openmcp-project/openmcp-testing/pkg/providers"
	apiv1alpha1 "github.com/openmcp-project/service-provider-argocd/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	clientValuesMCPName         = "test-client-values"
	clientValuesConfigMapName   = "vals-cm"
	clientValuesSecretName      = "vals-secret"
	clientValuesConfigMapTPName = "vals-tp"
)

// upsertClientValuesProviderConfig creates or replaces the "argocd" ProviderConfig
// with operator values that must win over all client-supplied values:
//
//	configs.cm.admin.enabled: false  (operator-only key)
//	server.name: "from-operator"     (overrides Secret's "from-secret")
func upsertClientValuesProviderConfig(ctx context.Context, t *testing.T, c *envconf.Config) {
	t.Helper()
	cfg := &apiv1alpha1.ProviderConfig{}
	cfg.SetName("argocd")
	cfg.Spec.Versions = []apiv1alpha1.ArgoCDVersion{{
		Version:      testArgoCDVersion,
		ChartVersion: testArgoCDChartVersion,
		ChartURL:     ptr.To(testArgoCDChartURL),
		Values: &apiextensionsv1.JSON{Raw: []byte(
			`{"configs":{"cm":{"admin.enabled":false}},"server":{"name":"from-operator"}}`,
		)},
	}}
	cfg.Spec.PollInterval = &metav1.Duration{Duration: 10 * time.Second}

	existing := &apiv1alpha1.ProviderConfig{}
	if err := c.Client().Resources().Get(ctx, "argocd", "", existing); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Errorf("failed to check for existing ProviderConfig: %v", err)
			return
		}
		if err := c.Client().Resources().Create(ctx, cfg); err != nil {
			t.Errorf("failed to create ProviderConfig: %v", err)
		}
		return
	}
	cfg.SetResourceVersion(existing.GetResourceVersion())
	if err := c.Client().Resources().Update(ctx, cfg); err != nil {
		t.Errorf("failed to update ProviderConfig: %v", err)
	}
}

// TestClientValues verifies that spec.values and spec.valuesFrom on the ArgoCD
// CR are resolved, merged in the correct priority order, and written into the
// HelmRelease. Sources and their expected contribution:
//
//  1. ConfigMap vals-cm (non-targetPath): server.replicas: 2
//     → overridden by inline spec.values (replicas: 3)
//  2. Secret vals-secret (non-targetPath): server.name: "from-secret"
//     → overridden by ProviderConfig (name: "from-operator")
//  3. spec.values inline: server.replicas: 3
//     → beats ConfigMap at the same key
//  4. ConfigMap vals-tp (targetPath configs.cm.timeout\.reconciliation): "80s"
//     → escaped-dot path sets a literal-dot key in configs.cm
//  5. ProviderConfig: configs.cm.admin.enabled: false, server.name: "from-operator"
//     → always wins regardless of client sources
func TestClientValues(t *testing.T) {
	cvTest := features.New("client values merge").
		Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiv1alpha1.AddToScheme(c.Client().Resources().GetScheme())
			helmv2.AddToScheme(c.Client().Resources().GetScheme())

			upsertClientValuesProviderConfig(ctx, t, c)
			return ctx
		}).
		Setup(providers.CreateMCP(clientValuesMCPName, wait.WithTimeout(10*time.Minute))).

		// ── 1. Create all sources and wait for the ArgoCD CR to become Ready ──
		Assess("ArgoCD CR becomes Ready with all client value sources configured",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				onboarding, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(onboarding.Client().Resources().GetScheme())

				// ConfigMap 1: non-targetPath — contributes server.replicas: 2 (will
				// be overridden by the inline spec.values replicas: 3).
				cm := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: clientValuesConfigMapName, Namespace: "default"},
					Data:       map[string]string{"values.yaml": "server:\n  replicas: 2\n"},
				}
				if err := onboarding.Client().Resources().Create(ctx, cm); err != nil {
					t.Errorf("failed to create ConfigMap %q: %v", clientValuesConfigMapName, err)
					return ctx
				}

				// Secret: non-targetPath — contributes server.name: "from-secret"
				// (will be overridden by ProviderConfig server.name: "from-operator").
				sec := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: clientValuesSecretName, Namespace: "default"},
					Data:       map[string][]byte{"values.yaml": []byte("server:\n  name: from-secret\n")},
				}
				if err := onboarding.Client().Resources().Create(ctx, sec); err != nil {
					t.Errorf("failed to create Secret %q: %v", clientValuesSecretName, err)
					return ctx
				}

				// ConfigMap 2: targetPath with an escaped dot in the path, placing the
				// raw scalar "80s" at configs.cm["timeout.reconciliation"].
				tp := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: clientValuesConfigMapTPName, Namespace: "default"},
					Data:       map[string]string{"values.yaml": "80s"},
				}
				if err := onboarding.Client().Resources().Create(ctx, tp); err != nil {
					t.Errorf("failed to create ConfigMap %q: %v", clientValuesConfigMapTPName, err)
					return ctx
				}

				api := &apiv1alpha1.ArgoCD{}
				api.SetName(clientValuesMCPName)
				api.SetNamespace("default")
				api.Spec.Version = testArgoCDVersion
				api.Spec.ValuesFrom = []apiv1alpha1.ValuesReference{
					{Kind: "ConfigMap", Name: clientValuesConfigMapName},
					{Kind: "Secret", Name: clientValuesSecretName},
					// \. escapes the literal dot in the ArgoCD config key name.
					{Kind: "ConfigMap", Name: clientValuesConfigMapTPName, TargetPath: `configs.cm.timeout\.reconciliation`},
				}
				api.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"server":{"replicas":3}}`)}

				if err := onboarding.Client().Resources().Create(ctx, api); err != nil {
					t.Errorf("failed to create ArgoCD CR: %v", err)
					return ctx
				}
				if err := wait.For(
					openmcpconditions.Match(api, onboarding, "Ready", corev1.ConditionTrue),
					wait.WithTimeout(10*time.Minute),
				); err != nil {
					t.Errorf("ArgoCD CR did not become Ready: %v", err)
				}
				return ctx
			},
		).

		// ── 2. Assert HelmRelease carries the correctly merged values ─────────
		Assess("HelmRelease values reflect merged client and operator values",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				ns := stableTenantNamespace(t, clientValuesMCPName, "default")

				hr := &helmv2.HelmRelease{}
				if err := c.Client().Resources().Get(ctx, "argocd", ns, hr); err != nil {
					t.Errorf("failed to get HelmRelease in %q: %v", ns, err)
					return ctx
				}
				if hr.Spec.Values == nil {
					t.Error("HelmRelease.Spec.Values is nil")
					return ctx
				}
				var vals map[string]any
				if err := json.Unmarshal(hr.Spec.Values.Raw, &vals); err != nil {
					t.Errorf("failed to unmarshal HelmRelease values: %v", err)
					return ctx
				}

				// inline spec.values (replicas: 3) must beat ConfigMap (replicas: 2).
				server, ok := vals["server"].(map[string]any)
				if !ok {
					t.Error("server key missing or not a map in HelmRelease values")
					return ctx
				}
				if replicas, ok := server["replicas"].(float64); !ok || replicas != 3 {
					t.Errorf("server.replicas = %v, want 3 (inline must beat ConfigMap)", server["replicas"])
				}

				// ProviderConfig server.name must beat Secret's "from-secret".
				if name, ok := server["name"].(string); !ok || name != "from-operator" {
					t.Errorf("server.name = %v, want %q (ProviderConfig must win)", server["name"], "from-operator")
				}

				// targetPath with escaped dot: key "timeout.reconciliation" (literal dot)
				// must be present under configs.cm.
				cfgCM, err := cvNestedMap(vals, "configs", "cm")
				if err != nil {
					t.Errorf("configs.cm not found in HelmRelease values: %v", err)
					return ctx
				}
				if v, ok := cfgCM["timeout.reconciliation"].(string); !ok || v != "80s" {
					t.Errorf(`configs.cm["timeout.reconciliation"] = %v, want "80s"`, cfgCM["timeout.reconciliation"])
				}

				// ProviderConfig-only key: configs.cm["admin.enabled"] is a literal-dot key.
				if enabled, ok := cfgCM["admin.enabled"].(bool); !ok || enabled {
					t.Errorf("configs.cm[\"admin.enabled\"] = %v, want false", cfgCM["admin.enabled"])
				}

				return ctx
			},
		).

		// ── Teardown ──────────────────────────────────────────────────────────
		Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			onboarding, err := clusterutils.OnboardingConfig()
			if err != nil {
				t.Logf("teardown: failed to get onboarding config: %v", err)
				return ctx
			}
			apiv1alpha1.AddToScheme(onboarding.Client().Resources().GetScheme())

			api := &apiv1alpha1.ArgoCD{}
			api.SetName(clientValuesMCPName)
			api.SetNamespace("default")
			if err := onboarding.Client().Resources().Delete(ctx, api); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("teardown: failed to delete ArgoCD CR: %v", err)
			}
			if err := wait.For(
				conditions.New(onboarding.Client().Resources()).ResourceDeleted(api),
				wait.WithTimeout(10*time.Minute),
			); err != nil {
				t.Logf("teardown: ArgoCD CR not fully deleted: %v", err)
			}

			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clientValuesConfigMapName, Namespace: "default"}}
			if err := onboarding.Client().Resources().Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("teardown: failed to delete ConfigMap %q: %v", clientValuesConfigMapName, err)
			}
			tp := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clientValuesConfigMapTPName, Namespace: "default"}}
			if err := onboarding.Client().Resources().Delete(ctx, tp); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("teardown: failed to delete ConfigMap %q: %v", clientValuesConfigMapTPName, err)
			}
			sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: clientValuesSecretName, Namespace: "default"}}
			if err := onboarding.Client().Resources().Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("teardown: failed to delete Secret %q: %v", clientValuesSecretName, err)
			}

			cfg := &apiv1alpha1.ProviderConfig{}
			cfg.SetName("argocd")
			if err := c.Client().Resources().Delete(ctx, cfg); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("teardown: failed to delete ProviderConfig: %v", err)
			}
			return ctx
		}).
		Teardown(providers.DeleteMCP(clientValuesMCPName, wait.WithTimeout(5*time.Minute)))

	testenv.Test(t, cvTest.Feature())
}

// cvNestedMap navigates a nested map[string]any and returns the map at the
// given key path, returning an error if any step is missing or not a map.
func cvNestedMap(m map[string]any, keys ...string) (map[string]any, error) {
	cur := m
	for i, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("key %q not found or not a map at path index %d", k, i)
		}
		cur = next
	}
	return cur, nil
}
