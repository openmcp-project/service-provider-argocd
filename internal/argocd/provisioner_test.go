package argocd

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	return scheme
}

func newSecret(name, namespace string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       data,
	}
}

func TestProvisionerReconcileImagePullSecrets_CopiesAndPrunes(t *testing.T) {
	scheme := newTestScheme(t)
	ctx := context.Background()

	podNamespace := "sp-argocd"
	mcpNamespace := "argocd"

	sourceSecret := newSecret("regcred", podNamespace, map[string][]byte{".dockerconfigjson": []byte(`{"auths":{}}`)})
	// Pre-existing orphaned secret on the MCP that should be pruned away.
	orphan := newSecret("stale", mcpNamespace, nil)
	orphan.Labels = map[string]string{managedByLabel: imagePullSecretManagedByValue}
	// A secret on the MCP that is not managed by us and must be left alone.
	unmanaged := newSecret("unmanaged", mcpNamespace, nil)

	platformClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sourceSecret).Build()
	mcpClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(orphan, unmanaged).Build()

	p := &Provisioner{
		platformClient: platformClient,
		mcpClient:      mcpClient,
		mcpNamespace:   mcpNamespace,
		podNamespace:   podNamespace,
	}

	values := &apiextensionsv1.JSON{Raw: []byte(`{"global":{"imagePullSecrets":[{"name":"regcred"}]}}`)}
	if err := p.reconcileImagePullSecrets(ctx, values); err != nil {
		t.Fatalf("reconcileImagePullSecrets() error = %v", err)
	}

	// The referenced secret must now exist on the MCP, labeled as managed.
	copied := &corev1.Secret{}
	if err := mcpClient.Get(ctx, client.ObjectKey{Name: "regcred", Namespace: mcpNamespace}, copied); err != nil {
		t.Fatalf("expected secret to be copied to MCP: %v", err)
	}
	if copied.Labels[managedByLabel] != imagePullSecretManagedByValue {
		t.Fatalf("expected copied secret to carry managed-by label, got %v", copied.Labels)
	}
	if string(copied.Data[".dockerconfigjson"]) != `{"auths":{}}` {
		t.Fatalf("expected copied secret data to match source, got %v", copied.Data)
	}

	// The orphaned, previously-managed secret must be pruned.
	if err := mcpClient.Get(ctx, client.ObjectKey{Name: "stale", Namespace: mcpNamespace}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected orphaned managed secret to be deleted, got err=%v", err)
	}

	// The unmanaged secret must be untouched.
	if err := mcpClient.Get(ctx, client.ObjectKey{Name: "unmanaged", Namespace: mcpNamespace}, &corev1.Secret{}); err != nil {
		t.Fatalf("expected unmanaged secret to remain, got err=%v", err)
	}
}

func TestProvisionerReconcileImagePullSecrets_MissingSourceSecretErrors(t *testing.T) {
	scheme := newTestScheme(t)
	ctx := context.Background()

	platformClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	mcpClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	p := &Provisioner{
		platformClient: platformClient,
		mcpClient:      mcpClient,
		mcpNamespace:   "argocd",
		podNamespace:   "sp-argocd",
	}

	values := &apiextensionsv1.JSON{Raw: []byte(`{"global":{"imagePullSecrets":[{"name":"missing"}]}}`)}
	if err := p.reconcileImagePullSecrets(ctx, values); err == nil {
		t.Fatal("expected error when source secret is missing, got nil")
	}
}

func TestProvisionerRemoveImagePullSecrets_DeletesAllManaged(t *testing.T) {
	scheme := newTestScheme(t)
	ctx := context.Background()
	mcpNamespace := "argocd"

	managed1 := newSecret("regcred", mcpNamespace, nil)
	managed1.Labels = map[string]string{managedByLabel: imagePullSecretManagedByValue}
	managed2 := newSecret("other-cred", mcpNamespace, nil)
	managed2.Labels = map[string]string{managedByLabel: imagePullSecretManagedByValue}
	unmanaged := newSecret("unmanaged", mcpNamespace, nil)

	mcpClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(managed1, managed2, unmanaged).Build()

	p := &Provisioner{
		mcpClient:    mcpClient,
		mcpNamespace: mcpNamespace,
	}

	if err := p.removeImagePullSecrets(ctx); err != nil {
		t.Fatalf("removeImagePullSecrets() error = %v", err)
	}

	for _, name := range []string{"regcred", "other-cred"} {
		if err := mcpClient.Get(ctx, client.ObjectKey{Name: name, Namespace: mcpNamespace}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatalf("expected managed secret %q to be deleted, got err=%v", name, err)
		}
	}
	if err := mcpClient.Get(ctx, client.ObjectKey{Name: "unmanaged", Namespace: mcpNamespace}, &corev1.Secret{}); err != nil {
		t.Fatalf("expected unmanaged secret to remain, got err=%v", err)
	}
}
