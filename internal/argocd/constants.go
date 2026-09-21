package argocd

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// DefaultNamespace is the namespace on the MCP into which ArgoCD is
	// installed when the ProviderConfig does not override it.
	DefaultNamespace = "argocd"

	// ociRepositoryName and helmReleaseName are the stable names of the Flux
	// resources this provider manages per ArgoCD instance. They live in the
	// tenant namespace on the platform cluster.
	ociRepositoryName = "argocd"
	helmReleaseName   = "argocd"

	// reloaderOCIRepositoryName and reloaderHelmReleaseName are the stable names
	// of the Flux resources for the optional Stakater Reloader addon. They share
	// the tenant namespace with the ArgoCD resources.
	reloaderOCIRepositoryName = "argocd-reloader"
	reloaderHelmReleaseName   = "argocd-reloader"

	// kubeConfigSecretKey is the key under which the MCP kubeconfig is stored
	// in the access secret referenced by the HelmRelease.
	kubeConfigSecretKey = "kubeconfig"

	// serverServiceName and serverTLSSecretName are the well-known ArgoCD
	// resources on the MCP used to resolve external-endpoint readiness.
	serverServiceName   = "argocd-server"
	serverTLSSecretName = "argocd-server-tls"

	// ArgoCD stamps these stable labels on the server components regardless of
	// the Helm release name.
	labelPartOf      = "app.kubernetes.io/part-of"
	labelPartOfValue = "argocd"
	labelComponent   = "app.kubernetes.io/component"
	labelServerValue = "server"

	// managedByLabel is the standard Kubernetes "managed-by" label key.
	// managedByValue is used on core ArgoCD Flux resources and is watched by the
	// opencontrolplane-runtime framework: a HelmRelease with this label entering
	// a Failed state causes the framework to delete the owning ArgoCD CR.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "service-provider-argocd"

	// managedByReloaderValue is used exclusively on Reloader addon resources.
	// It is intentionally different from managedByValue so the framework's watch
	// never sees Reloader HelmRelease failures and never cascades them into
	// ArgoCD CR deletion.
	managedByReloaderValue = "service-provider-argocd-reloader"
)

// applicationListGVK identifies the ArgoCD Application list kind. It is used as
// a deletion guard so that user workloads are never orphaned.
var applicationListGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "ApplicationList",
}
