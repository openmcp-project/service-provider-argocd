package argocd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// ExposureValues carries the fully-resolved exposure intent, combining the
// tenant's request (host, allowed IPs) with the platform policy (DNS class,
// TTL, certificate purpose). It is the single input to the Gardener values
// fragment, which keeps the rendering logic pure and unit-testable.
type ExposureValues struct {
	Enabled     bool
	Host        string
	AllowedIPs  []string
	DNSClass    string
	DNSTTL      int
	CertPurpose string
}

// buildValues merges the operator-supplied chart values with the exposure
// fragment derived from the ArgoCD request. Precedence is:
//
//	chart defaults  <  operator values (version.Values)  <  exposure fragment
//
// so that platform-provided values cannot silently drop the annotations that
// exposure depends on. A nil result means "no values", which is a valid input
// for a HelmRelease.
func buildValues(base *apiextensionsv1.JSON, exposure ExposureValues) (*apiextensionsv1.JSON, error) {
	values := map[string]any{}
	if base != nil && len(base.Raw) > 0 {
		if err := json.Unmarshal(base.Raw, &values); err != nil {
			return nil, fmt.Errorf("decoding operator-supplied values: %w", err)
		}
	}

	if exposure.Enabled {
		deepMerge(values, exposureFragment(exposure))
	}

	if len(values) == 0 {
		return nil, nil
	}

	raw, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encoding merged values: %w", err)
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// exposureFragment renders the ArgoCD Helm values that wire up a Gardener
// LoadBalancer Service with managed DNS and TLS. It mirrors the annotations
// consumed by the shoot-dns-service and shoot-cert-service extensions.
func exposureFragment(e ExposureValues) map[string]any {
	annotations := map[string]any{
		"dns.gardener.cloud/dnsnames":    e.Host,
		"dns.gardener.cloud/class":       e.DNSClass,
		"dns.gardener.cloud/ttl":         strconv.Itoa(e.DNSTTL),
		"cert.gardener.cloud/purpose":    e.CertPurpose,
		"cert.gardener.cloud/commonname": e.Host,
		"cert.gardener.cloud/dnsnames":   e.Host,
		"cert.gardener.cloud/secretname": serverTLSSecretName,
	}

	service := map[string]any{
		"type":        "LoadBalancer",
		"annotations": annotations,
	}
	if len(e.AllowedIPs) > 0 {
		service["loadBalancerSourceRanges"] = toAnySlice(e.AllowedIPs)
	}

	return map[string]any{
		"server": map[string]any{
			"service": service,
			// Restart argocd-server when the managed TLS secret rotates so the
			// renewed certificate is picked up without manual intervention.
			"deploymentAnnotations": map[string]any{
				"secret.reloader.stakater.com/reload": strings.Join([]string{
					"argocd-secret",
					serverTLSSecretName,
				}, ", "),
			},
		},
		// Ensure ArgoCD emits correct absolute URLs (redirects, OIDC callbacks).
		"configs": map[string]any{
			"cm": map[string]any{
				"url": "https://" + e.Host,
			},
		},
	}
}

// toAnySlice converts a string slice to an []any so it can live inside an
// untyped values map that marshals cleanly to JSON.
func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// deepMerge recursively merges src into dst. On key conflicts, nested maps are
// merged and scalar/slice values from src overwrite those in dst.
func deepMerge(dst, src map[string]any) {
	for key, srcVal := range src {
		if srcMap, ok := srcVal.(map[string]any); ok {
			if dstMap, ok := dst[key].(map[string]any); ok {
				deepMerge(dstMap, srcMap)
				continue
			}
		}
		dst[key] = srcVal
	}
}
