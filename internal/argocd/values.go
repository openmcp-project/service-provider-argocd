package argocd

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// ExposureValues carries the fully-resolved exposure intent, combining the
// tenant's request (host, allowed IPs) with the platform policy (DNS class,
// TTL, certificate purpose). It is the single input to the Gardener values
// fragment, which keeps the rendering logic pure and unit-testable.
type ExposureValues struct {
	Enabled         bool
	Host            string
	AllowedIPs      []string
	DNSClass        string
	DNSTTL          int
	CertPurpose     string
	ReloaderEnabled bool
}

// buildValues merges the operator-supplied chart values with the exposure
// fragment derived from the ArgoCD request and, when a custom CA is provided,
// injects it into ArgoCD's trusted-certificate configuration. Precedence is:
//
//	chart defaults  <  operator values (version.Values)  <  exposure fragment  <  CA fragment
//
// so that platform-provided values cannot silently drop the annotations that
// exposure depends on. A nil result means "no values", which is a valid input
// for a HelmRelease.
func buildValues(base *apiextensionsv1.JSON, exposure ExposureValues, caCert string) (*apiextensionsv1.JSON, error) {
	values := map[string]any{}
	if base != nil && len(base.Raw) > 0 {
		if err := json.Unmarshal(base.Raw, &values); err != nil {
			return nil, fmt.Errorf("decoding operator-supplied values: %w", err)
		}
	}

	if exposure.Enabled {
		deepMerge(values, exposureFragment(exposure))
	}

	if caCert != "" {
		fragment, err := caBundleFragment(caCert)
		if err != nil {
			return nil, fmt.Errorf("building CA bundle values: %w", err)
		}
		deepMerge(values, fragment)
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

	server := map[string]any{
		"service": service,
	}
	// Only wire the Reloader restart annotation when Reloader is actually
	// installed. Otherwise it is inert and would hide the fact that a rotated
	// TLS certificate is not picked up until argocd-server is restarted.
	if e.ReloaderEnabled {
		server["deploymentAnnotations"] = map[string]any{
			"secret.reloader.stakater.com/reload": strings.Join([]string{
				"argocd-secret",
				serverTLSSecretName,
			}, ", "),
		}
	}

	return map[string]any{
		"server": server,
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

// caBundleFragment renders the ArgoCD Helm values that inject the custom CA
// bundle into ArgoCD's trusted-certificate configuration. This populates the
// argocd-tls-certs-cm ConfigMap that ArgoCD's repo-server reads when connecting
// to Git repositories and other TLS endpoints.
//
// ArgoCD keys that ConfigMap by the server hostname: when it connects to
// https://<host>, it trusts the certificate stored under the key <host>. The
// hostnames are therefore extracted from the provided certificate's DNS SANs
// (or, when it carries none, its Common Name). The convention is that the
// platform team embeds the target Git/registry hostnames as SANs on the CA
// bundle so the trust anchor is wired to exactly those endpoints.
func caBundleFragment(caCert string) (map[string]any, error) {
	hosts, err := certHostnames(caCert)
	if err != nil {
		return nil, err
	}

	certificates := make(map[string]any, len(hosts))
	for _, host := range hosts {
		certificates[host] = caCert
	}

	return map[string]any{
		"configs": map[string]any{
			"tls": map[string]any{
				"certificates": certificates,
			},
		},
	}, nil
}

// hostKeyPattern matches a hostname that is also a valid ConfigMap data key
// (RFC 1123 labels joined by dots). Wildcards and other characters that ArgoCD
// could never match against a concrete connection host are rejected.
var hostKeyPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// isValidHostKey reports whether host can be used as an argocd-tls-certs-cm key.
func isValidHostKey(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	return hostKeyPattern.MatchString(host)
}

// certHostnames parses one or more PEM-encoded certificates and returns the
// hostnames to key the trusted-certificate ConfigMap by. DNS SANs across all
// certificates in the bundle are preferred; when none are present it falls back
// to the first certificate's Common Name. It returns an error when the input
// cannot be parsed or yields no usable hostname.
func certHostnames(caCert string) ([]string, error) {
	certs, err := parseCertificates(caCert)
	if err != nil {
		return nil, err
	}

	hosts := collectDNSHosts(certs)
	if len(hosts) == 0 {
		if cn := certs[0].Subject.CommonName; isValidHostKey(cn) {
			hosts = append(hosts, cn)
		}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("certificate has no DNS SAN and no usable common name to key argocd-tls-certs-cm by hostname")
	}
	return hosts, nil
}

// parseCertificates decodes every CERTIFICATE block in a PEM bundle. It errors
// if a block fails to parse or if the bundle contains no certificate.
func parseCertificates(caCert string) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := []byte(caCert)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no PEM certificate block found in CA bundle")
	}
	return certs, nil
}

// collectDNSHosts returns the deduplicated, ConfigMap-key-safe DNS SANs across
// all certificates, preserving first-seen order.
func collectDNSHosts(certs []*x509.Certificate) []string {
	var hosts []string
	seen := map[string]bool{}
	for _, cert := range certs {
		for _, dns := range cert.DNSNames {
			if isValidHostKey(dns) && !seen[dns] {
				seen[dns] = true
				hosts = append(hosts, dns)
			}
		}
	}
	return hosts
}
