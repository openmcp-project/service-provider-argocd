package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/openmcp-project/openmcp-testing/pkg/clusterutils"
	openmcpconditions "github.com/openmcp-project/openmcp-testing/pkg/conditions"
	"github.com/openmcp-project/openmcp-testing/pkg/providers"
	apiv1alpha1 "github.com/openmcp-project/service-provider-argocd/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	caTestMCPName = "test-ca"
	caSecretName  = "test-ca-bundle"

	// caTestHost is embedded as a DNS SAN in the generated test CA so the
	// provisioner keys argocd-tls-certs-cm by this hostname.
	caTestHost = "git.test.local"

	// Mirror of internal/argocd constants; re-declared here to avoid an import cycle.
	caTestManagedByLabel = "app.kubernetes.io/managed-by"
	caTestManagedByValue = "service-provider-argocd"
)

// generateTestCACert creates a minimal self-signed CA certificate and returns
// its PEM encoding. Generated at test-run time so it never expires.
func generateTestCACert(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		DNSNames:              []string{caTestHost},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	var buf bytes.Buffer
	if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode CA cert: %v", err)
	}
	return buf.String()
}

// upsertCAProviderConfig creates or updates the "argocd" ProviderConfig to
// include the given caBundleSecret. It preserves all other fields so the
// function is safe to call whether or not TestServiceProvider has already run.
func upsertCAProviderConfig(ctx context.Context, t *testing.T, c *envconf.Config, caBundleSecret string) {
	t.Helper()
	cfg := &apiv1alpha1.ProviderConfig{}
	err := c.Client().Resources().Get(ctx, "argocd", "", cfg)
	if apierrors.IsNotFound(err) {
		cfg.SetName("argocd")
		cfg.Spec.Versions = []apiv1alpha1.ArgoCDVersion{{
			Version:      testArgoCDVersion,
			ChartVersion: testArgoCDChartVersion,
			ChartURL:     ptr.To(testArgoCDChartURL),
		}}
		cfg.Spec.CABundleSecret = caBundleSecret
		if err := c.Client().Resources().Create(ctx, cfg); err != nil {
			t.Errorf("failed to create ProviderConfig: %v", err)
		}
		return
	}
	if err != nil {
		t.Errorf("failed to get ProviderConfig: %v", err)
		return
	}
	cfg.Spec.CABundleSecret = caBundleSecret
	if err := c.Client().Resources().Update(ctx, cfg); err != nil {
		t.Errorf("failed to update ProviderConfig with caBundleSecret: %v", err)
	}
}

// TestCustomCA verifies that configuring caBundleSecret on the ProviderConfig
// results in the CA being propagated to all managed Flux resources:
//
//  1. The ArgoCD CR reaches Ready (provisioning succeeds with a CA configured).
//  2. The CA Secret is synced into the tenant namespace with the managed-by label.
//  3. The ArgoCD OCIRepository carries certSecretRef pointing at the synced secret.
//  4. The ArgoCD HelmRelease values contain the CA cert keyed by the hostname
//     extracted from the certificate's DNS SAN (configs.tls.certificates[host]).
func TestCustomCA(t *testing.T) {
	// Generate a real self-signed CA cert at test time so Flux can parse it.
	caPEM := generateTestCACert(t)

	caTest := features.New("custom CA propagation").
		Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiv1alpha1.AddToScheme(c.Client().Resources().GetScheme())
			helmv2.AddToScheme(c.Client().Resources().GetScheme())
			sourcev1.AddToScheme(c.Client().Resources().GetScheme())

			// Create the CA bundle Secret in the provider namespace so that
			// ensureCABundle can read and sync it into the tenant namespace.
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      caSecretName,
					Namespace: "openmcp-system",
				},
				Data: map[string][]byte{"ca.crt": []byte(caPEM)},
			}
			if err := c.Client().Resources().Create(ctx, secret); err != nil {
				t.Errorf("failed to create CA secret in provider namespace: %v", err)
			}

			upsertCAProviderConfig(ctx, t, c, caSecretName)
			return ctx
		}).
		Setup(providers.CreateMCP(caTestMCPName)).

		// ── 1. ArgoCD CR becomes Ready ────────────────────────────────────────
		Assess("ArgoCD CR becomes Ready with CA configured",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				onboarding, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(onboarding.Client().Resources().GetScheme())

				api := &apiv1alpha1.ArgoCD{}
				api.SetName(caTestMCPName)
				api.SetNamespace("default")
				api.Spec.Version = testArgoCDVersion
				if err := onboarding.Client().Resources().Create(ctx, api); err != nil {
					t.Errorf("failed to create ArgoCD CR: %v", err)
					return ctx
				}

				if err := wait.For(
					openmcpconditions.Match(api, onboarding, "Ready", corev1.ConditionTrue),
					wait.WithTimeout(5*time.Minute),
				); err != nil {
					t.Errorf("ArgoCD CR did not become Ready: %v", err)
				}
				return ctx
			},
		).

		// ── 2. CA Secret synced to tenant namespace ───────────────────────────
		Assess("CA Secret is synced to the tenant namespace with managed-by label",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				ns := stableTenantNamespace(t, caTestMCPName, "default")

				secret := &corev1.Secret{}
				if err := wait.For(func(ctx context.Context) (bool, error) {
					return c.Client().Resources().Get(ctx, caSecretName, ns, secret) == nil, nil
				}, wait.WithTimeout(2*time.Minute)); err != nil {
					t.Errorf("CA Secret %q not found in tenant namespace %q: %v", caSecretName, ns, err)
					return ctx
				}
				if got := secret.Labels[caTestManagedByLabel]; got != caTestManagedByValue {
					t.Errorf("managed-by label: got %q, want %q", got, caTestManagedByValue)
				}
				return ctx
			},
		).

		// ── 3. OCIRepository carries certSecretRef ────────────────────────────
		Assess("ArgoCD OCIRepository carries certSecretRef pointing at the CA secret",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				ns := stableTenantNamespace(t, caTestMCPName, "default")

				repo := &sourcev1.OCIRepository{}
				if err := wait.For(func(ctx context.Context) (bool, error) {
					return c.Client().Resources().Get(ctx, "argocd", ns, repo) == nil, nil
				}, wait.WithTimeout(2*time.Minute)); err != nil {
					t.Errorf("OCIRepository %q not found in %q: %v", "argocd", ns, err)
					return ctx
				}
				if repo.Spec.CertSecretRef == nil {
					t.Error("OCIRepository.Spec.CertSecretRef is nil; expected it to reference the CA secret")
					return ctx
				}
				if got := repo.Spec.CertSecretRef.Name; got != caSecretName {
					t.Errorf("CertSecretRef.Name = %q, want %q", got, caSecretName)
				}
				return ctx
			},
		).

		// ── 4. HelmRelease values contain the CA cert ─────────────────────────
		Assess("ArgoCD HelmRelease values contain CA cert at configs.tls.certificates",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				ns := stableTenantNamespace(t, caTestMCPName, "default")

				hr := &helmv2.HelmRelease{}
				if err := c.Client().Resources().Get(ctx, "argocd", ns, hr); err != nil {
					t.Errorf("failed to get HelmRelease %q in %q: %v", "argocd", ns, err)
					return ctx
				}
				if hr.Spec.Values == nil {
					t.Error("HelmRelease.Spec.Values is nil; CA cert was not injected")
					return ctx
				}

				var vals map[string]any
				if err := json.Unmarshal(hr.Spec.Values.Raw, &vals); err != nil {
					t.Errorf("failed to unmarshal HelmRelease values: %v", err)
					return ctx
				}

				got, err := caNestedString(vals, "configs", "tls", "certificates", caTestHost)
				if err != nil {
					t.Errorf("configs.tls.certificates[%q] not found in HelmRelease values: %v", caTestHost, err)
					return ctx
				}
				if got != caPEM {
					t.Errorf("CA cert mismatch:\n  got  %q\n  want %q", got, caPEM)
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
			api.SetName(caTestMCPName)
			api.SetNamespace("default")
			if err := onboarding.Client().Resources().Delete(ctx, api); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("teardown: failed to delete ArgoCD CR: %v", err)
			}
			if err := wait.For(
				conditions.New(onboarding.Client().Resources()).ResourceDeleted(api),
				wait.WithTimeout(5*time.Minute),
			); err != nil {
				t.Logf("teardown: ArgoCD CR not fully deleted within timeout: %v", err)
			}
			return ctx
		}).
		Teardown(providers.DeleteMCP(caTestMCPName, wait.WithTimeout(5*time.Minute))).
		Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: caSecretName, Namespace: "openmcp-system"},
			}
			if err := c.Client().Resources().Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("teardown: failed to delete CA secret: %v", err)
			}

			// Clear caBundleSecret from the ProviderConfig so TestServiceProvider
			// and subsequent runs are not affected.
			cfg := &apiv1alpha1.ProviderConfig{}
			if err := c.Client().Resources().Get(ctx, "argocd", "", cfg); err != nil {
				t.Logf("teardown: failed to get ProviderConfig: %v", err)
				return ctx
			}
			cfg.Spec.CABundleSecret = ""
			if err := c.Client().Resources().Update(ctx, cfg); err != nil {
				t.Logf("teardown: failed to clear caBundleSecret from ProviderConfig: %v", err)
			}
			return ctx
		})

	testenv.Test(t, caTest.Feature())
}

// caNestedString navigates a nested map[string]any and returns the string
// value at the given key path, returning an error if any step fails.
func caNestedString(m map[string]any, keys ...string) (string, error) {
	cur := m
	for i, k := range keys[:len(keys)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return "", fmt.Errorf("key %q not found or not a map at path index %d", k, i)
		}
		cur = next
	}
	last := keys[len(keys)-1]
	v, ok := cur[last].(string)
	if !ok {
		return "", fmt.Errorf("key %q not found or not a string", last)
	}
	return v, nil
}
