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

	// kubeConfigSecretKey is the key under which the MCP kubeconfig is stored
	// in the access secret referenced by the HelmRelease.
	kubeConfigSecretKey = "kubeconfig"

	// managedByLabel marks resources created by this provider.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "service-provider-argocd"
)

// applicationListGVK identifies the ArgoCD Application list kind. It is used as
// a deletion guard so that user workloads are never orphaned.
var applicationListGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "ApplicationList",
}
