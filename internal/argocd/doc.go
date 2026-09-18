// Package argocd encapsulates the domain logic for provisioning ArgoCD onto a
// Managed Control Plane (MCP). Following the Flux service-provider pattern, it
// does not install anything directly: it declares Flux resources (an
// OCIRepository and a HelmRelease) on the platform cluster, and the Flux
// runtime pulls the ArgoCD Helm chart and installs it into the MCP. This keeps
// the reconciler focused on orchestrating desired-state transitions while this
// package owns the "how".
package argocd
