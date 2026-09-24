package argocd

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Endpoint resolves the external URL of the ArgoCD server and reports whether
// it is ready to serve traffic. Readiness requires both the Gardener-managed
// TLS certificate to be issued (the argocd-server-tls secret exists) and the
// LoadBalancer Service to have been assigned an address.
//
// When exposure is disabled the endpoint is empty and considered ready, so the
// reconciler does not block on it.
func (p *Provisioner) Endpoint(ctx context.Context) (endpoint string, ready bool, err error) {
	if !p.exposure.Enabled {
		return "", true, nil
	}

	endpoint = "https://" + p.exposure.Host

	// The TLS secret is created by the shoot-cert-service once the certificate
	// has been issued. Its name is fixed by the cert.gardener.cloud/secretname
	// annotation we set, so a name lookup is reliable here.
	tlsKey := client.ObjectKey{Name: serverTLSSecretName, Namespace: p.mcpNamespace}
	if err := p.mcpClient.Get(ctx, tlsKey, &corev1.Secret{}); err != nil {
		if apierrors.IsNotFound(err) {
			return endpoint, false, nil
		}
		return endpoint, false, fmt.Errorf("getting ArgoCD server TLS secret: %w", err)
	}

	// The Service must have an assigned LoadBalancer address before DNS can
	// resolve to it. We locate the server Service by ArgoCD's stable labels
	// rather than a fixed name: Flux derives the Helm release name (and thus the
	// resource-name prefix) from the target namespace when no explicit release
	// name is set, so the Service is not necessarily named "argocd-server".
	svcList := &corev1.ServiceList{}
	if err := p.mcpClient.List(ctx, svcList,
		client.InNamespace(p.mcpNamespace),
		client.MatchingLabels{
			labelPartOf:    labelPartOfValue,
			labelComponent: labelServerValue,
		},
	); err != nil {
		return endpoint, false, fmt.Errorf("listing ArgoCD server Services: %w", err)
	}

	for i := range svcList.Items {
		svc := &svcList.Items[i]
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		if len(svc.Status.LoadBalancer.Ingress) > 0 {
			return endpoint, true, nil
		}
	}

	// No LoadBalancer Service has an address yet.
	return endpoint, false, nil
}
