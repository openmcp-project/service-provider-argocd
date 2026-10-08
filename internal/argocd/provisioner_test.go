package argocd

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	apiv1alpha1 "github.com/openmcp-project/service-provider-argocd/api/v1alpha1"
)

const (
	testProviderNS = "provider-ns"
	testTenantNS   = "tenant-ns"
	testCASecret   = "my-ca"
	testCACert     = "-----BEGIN CERTIFICATE-----\nMIItest\n-----END CERTIFICATE-----\n"
)

// newScheme builds a runtime.Scheme suitable for provisioner unit tests.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add clientgoscheme: %v", err)
	}
	if err := sourcev1.AddToScheme(s); err != nil {
		t.Fatalf("add sourcev1: %v", err)
	}
	return s
}

// caSecret returns a Secret with the given name/namespace pre-loaded with a
// PEM CA cert under the standard "ca.crt" key.
func caSecret(name, namespace, cert string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"ca.crt": []byte(cert)},
	}
}

// newProvisioner builds a minimal Provisioner backed by a fake client.
func newTestProvisioner(fakeClient client.Client, caBundleSecret string) *Provisioner {
	return NewProvisioner(ProvisionerConfig{
		PlatformClient:          fakeClient,
		TenantNamespace:         testTenantNS,
		CABundleSecret:          caBundleSecret,
		CABundleSourceNamespace: testProviderNS,
	})
}

// ── ensureCABundle ────────────────────────────────────────────────────────────

func TestEnsureCABundle_Noop(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	p := newTestProvisioner(fc, "")

	if err := p.ensureCABundle(context.Background()); err != nil {
		t.Fatalf("expected no error for empty caBundleSecret, got: %v", err)
	}
	if p.caCert != "" {
		t.Fatalf("expected empty caCert, got %q", p.caCert)
	}
}

func TestEnsureCABundle_MissingSource(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	p := newTestProvisioner(fc, testCASecret)

	err := p.ensureCABundle(context.Background())
	if err == nil {
		t.Fatal("expected error when source secret is missing")
	}
}

func TestEnsureCABundle_CopiesSecretToTenantNamespace(t *testing.T) {
	src := caSecret(testCASecret, testProviderNS, testCACert)
	fc := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(src).
		Build()
	p := newTestProvisioner(fc, testCASecret)

	if err := p.ensureCABundle(context.Background()); err != nil {
		t.Fatalf("ensureCABundle: %v", err)
	}

	// Secret must exist in the tenant namespace.
	dst := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{
		Name:      testCASecret,
		Namespace: testTenantNS,
	}, dst); err != nil {
		t.Fatalf("expected CA secret in tenant namespace, got: %v", err)
	}
	if string(dst.Data["ca.crt"]) != testCACert {
		t.Fatalf("ca.crt mismatch: got %q, want %q", string(dst.Data["ca.crt"]), testCACert)
	}
	// The synced copy carries the managed-by label.
	if dst.Labels[managedByLabel] != managedByArgoCDValue {
		t.Fatalf("expected managed-by label %q, got %q", managedByArgoCDValue, dst.Labels[managedByLabel])
	}
	// p.caCert is populated for Helm values injection.
	if p.caCert != testCACert {
		t.Fatalf("p.caCert = %q, want %q", p.caCert, testCACert)
	}
}

func TestEnsureCABundle_Idempotent(t *testing.T) {
	src := caSecret(testCASecret, testProviderNS, testCACert)
	fc := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(src).
		Build()
	p := newTestProvisioner(fc, testCASecret)

	for i := range 3 {
		if err := p.ensureCABundle(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if p.caCert != testCACert {
		t.Fatalf("p.caCert = %q after repeated calls", p.caCert)
	}
}

func TestEnsureCABundle_MissingCACrtKey(t *testing.T) {
	// Secret exists but has no "ca.crt" key — the provisioner should not error,
	// but caCert must be empty (no injection into ArgoCD values).
	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testCASecret, Namespace: testProviderNS},
		Data:       map[string][]byte{"other-key": []byte("data")},
	}
	fc := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(src).
		Build()
	p := newTestProvisioner(fc, testCASecret)

	if err := p.ensureCABundle(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.caCert != "" {
		t.Fatalf("expected empty caCert when ca.crt key is absent, got %q", p.caCert)
	}
}

// ── applyOCIRepository ────────────────────────────────────────────────────────

func TestApplyOCIRepository_SetsCertSecretRef(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	p := newTestProvisioner(fc, testCASecret)

	chartURL := "oci://example.com/charts/argocd"
	version := apiv1alpha1.ArgoCDVersion{
		Version:      "v1.0.0",
		ChartVersion: "1.0.0",
		ChartURL:     &chartURL,
	}

	if err := p.applyOCIRepository(context.Background(), version); err != nil {
		t.Fatalf("applyOCIRepository: %v", err)
	}

	repo := &sourcev1.OCIRepository{}
	if err := fc.Get(context.Background(), client.ObjectKey{
		Name:      ociRepositoryName,
		Namespace: testTenantNS,
	}, repo); err != nil {
		t.Fatalf("get OCIRepository: %v", err)
	}
	if repo.Spec.CertSecretRef == nil {
		t.Fatal("expected CertSecretRef to be set, got nil")
	}
	if repo.Spec.CertSecretRef.Name != testCASecret {
		t.Fatalf("CertSecretRef.Name = %q, want %q", repo.Spec.CertSecretRef.Name, testCASecret)
	}
}

func TestApplyOCIRepository_NoCertSecretRefWhenNoCA(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	p := newTestProvisioner(fc, "")

	chartURL := "oci://example.com/charts/argocd"
	version := apiv1alpha1.ArgoCDVersion{
		Version:      "v1.0.0",
		ChartVersion: "1.0.0",
		ChartURL:     &chartURL,
	}

	if err := p.applyOCIRepository(context.Background(), version); err != nil {
		t.Fatalf("applyOCIRepository: %v", err)
	}

	repo := &sourcev1.OCIRepository{}
	if err := fc.Get(context.Background(), client.ObjectKey{
		Name:      ociRepositoryName,
		Namespace: testTenantNS,
	}, repo); err != nil {
		t.Fatalf("get OCIRepository: %v", err)
	}
	if repo.Spec.CertSecretRef != nil {
		t.Fatalf("expected CertSecretRef to be nil when no CA configured, got %+v", repo.Spec.CertSecretRef)
	}
}

func TestApplyReloaderOCIRepository_SetsCertSecretRef(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	reloaderChartURL := "oci://example.com/charts/reloader"
	p := NewProvisioner(ProvisionerConfig{
		PlatformClient:          fc,
		TenantNamespace:         testTenantNS,
		CABundleSecret:          testCASecret,
		CABundleSourceNamespace: testProviderNS,
		Reloader: &apiv1alpha1.ReloaderConfig{
			ChartURL:     reloaderChartURL,
			ChartVersion: "1.0.0",
		},
	})

	if err := p.applyReloaderOCIRepository(context.Background()); err != nil {
		t.Fatalf("applyReloaderOCIRepository: %v", err)
	}

	repo := &sourcev1.OCIRepository{}
	if err := fc.Get(context.Background(), client.ObjectKey{
		Name:      reloaderOCIRepositoryName,
		Namespace: testTenantNS,
	}, repo); err != nil {
		t.Fatalf("get Reloader OCIRepository: %v", err)
	}
	if repo.Spec.CertSecretRef == nil {
		t.Fatal("expected CertSecretRef to be set on Reloader OCIRepository, got nil")
	}
	if repo.Spec.CertSecretRef.Name != testCASecret {
		t.Fatalf("CertSecretRef.Name = %q, want %q", repo.Spec.CertSecretRef.Name, testCASecret)
	}
}

func TestApplyReloaderOCIRepository_NoCertSecretRefWhenNoCA(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	reloaderChartURL := "oci://example.com/charts/reloader"
	p := NewProvisioner(ProvisionerConfig{
		PlatformClient:  fc,
		TenantNamespace: testTenantNS,
		Reloader: &apiv1alpha1.ReloaderConfig{
			ChartURL:     reloaderChartURL,
			ChartVersion: "1.0.0",
		},
	})

	if err := p.applyReloaderOCIRepository(context.Background()); err != nil {
		t.Fatalf("applyReloaderOCIRepository: %v", err)
	}

	repo := &sourcev1.OCIRepository{}
	if err := fc.Get(context.Background(), client.ObjectKey{
		Name:      reloaderOCIRepositoryName,
		Namespace: testTenantNS,
	}, repo); err != nil {
		t.Fatalf("get Reloader OCIRepository: %v", err)
	}
	if repo.Spec.CertSecretRef != nil {
		t.Fatalf("expected CertSecretRef to be nil when no CA configured, got %+v", repo.Spec.CertSecretRef)
	}
}

// ── removeCABundle ────────────────────────────────────────────────────────────

func TestRemoveCABundle_Noop(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	p := newTestProvisioner(fc, "")

	if err := p.removeCABundle(context.Background()); err != nil {
		t.Fatalf("expected no error for empty caBundleSecret, got: %v", err)
	}
}

func TestRemoveCABundle_DeletesTenantCopy(t *testing.T) {
	// Pre-create the synced copy in the tenant namespace (simulating a previous Install).
	tenantCopy := caSecret(testCASecret, testTenantNS, testCACert)
	fc := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(tenantCopy).
		Build()
	p := newTestProvisioner(fc, testCASecret)

	if err := p.removeCABundle(context.Background()); err != nil {
		t.Fatalf("removeCABundle: %v", err)
	}

	// Secret must be gone from the tenant namespace.
	got := &corev1.Secret{}
	err := fc.Get(context.Background(), client.ObjectKey{
		Name:      testCASecret,
		Namespace: testTenantNS,
	}, got)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after removeCABundle, got: %v", err)
	}
}

func TestRemoveCABundle_AlreadyGone(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	p := newTestProvisioner(fc, testCASecret)

	// Secret was never created — should be a no-op, not an error.
	if err := p.removeCABundle(context.Background()); err != nil {
		t.Fatalf("expected no error when secret is already absent, got: %v", err)
	}
}

func TestRemoveCABundle_DoesNotDeleteSourceSecret(t *testing.T) {
	// The source secret in the provider namespace must never be touched.
	providerSecret := caSecret(testCASecret, testProviderNS, testCACert)
	tenantCopy := caSecret(testCASecret, testTenantNS, testCACert)
	fc := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(providerSecret, tenantCopy).
		Build()
	p := newTestProvisioner(fc, testCASecret)

	if err := p.removeCABundle(context.Background()); err != nil {
		t.Fatalf("removeCABundle: %v", err)
	}

	// Source secret in the provider namespace must still be there.
	got := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{
		Name:      testCASecret,
		Namespace: testProviderNS,
	}, got); err != nil {
		t.Fatalf("source secret in provider namespace was unexpectedly deleted: %v", err)
	}
}
