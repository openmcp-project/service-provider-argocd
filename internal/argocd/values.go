package argocd

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/fluxcd/pkg/chartutil"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	apiv1alpha1 "github.com/openmcp-project/service-provider-argocd/api/v1alpha1"
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

// imagePullSecretsValues mirrors the subset of Helm values this provider reads
// in order to discover image pull secrets that must be replicated from the
// service provider's namespace onto the ArgoCD namespace on the MCP, as
// documented on ArgoCDVersion.Values.
type imagePullSecretsValues struct {
	Global struct {
		ImagePullSecrets []struct {
			Name string `json:"name"`
		} `json:"imagePullSecrets,omitempty"`
	} `json:"global,omitempty"`
}

// ImagePullSecretNames extracts the names listed under
// values.global.imagePullSecrets[].name from the operator-supplied Helm
// values. It returns an empty slice when values is nil/empty or the path is
// not set.
func ImagePullSecretNames(values *apiextensionsv1.JSON) ([]string, error) {
	if values == nil || len(values.Raw) == 0 {
		return nil, nil
	}
	var parsed imagePullSecretsValues
	if err := json.Unmarshal(values.Raw, &parsed); err != nil {
		return nil, fmt.Errorf("decoding values to extract global.imagePullSecrets: %w", err)
	}
	names := make([]string, 0, len(parsed.Global.ImagePullSecrets))
	for _, ref := range parsed.Global.ImagePullSecrets {
		if ref.Name != "" {
			names = append(names, ref.Name)
		}
	}
	return names, nil
}

// buildValues merges client-supplied values, operator-supplied values, and the
// exposure fragment derived from the ArgoCD request and, when a custom CA is provided,
// injects it into ArgoCD's trusted-certificate configuration. Precedence (lowest to highest):
//
//	client values (spec.valuesFrom + spec.values)  <  operator values (ProviderConfig)  <  exposure fragment  <  CA fragment
//
// clientValues must be pre-resolved by ResolveClientValues before being passed
// in. A nil result means "no values", which is a valid input for a HelmRelease.
func buildValues(operatorValues *apiextensionsv1.JSON, clientValues map[string]any, exposure ExposureValues, caCert string) (*apiextensionsv1.JSON, error) {
	values := map[string]any{}

	// 1. client values — lowest priority
	if len(clientValues) > 0 {
		deepMerge(values, clientValues)
	}

	// 2. operator values (ProviderConfig) — wins over client
	if operatorValues != nil && len(operatorValues.Raw) > 0 {
		var opVals map[string]any
		if err := json.Unmarshal(operatorValues.Raw, &opVals); err != nil {
			return nil, fmt.Errorf("decoding operator-supplied values: %w", err)
		}
		deepMerge(values, opVals)
	}

	// 3. exposure fragment — always wins
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
// merged recursively; scalar/slice values from src overwrite those in dst.
// Map values from src are always deep-copied so that later mutations to dst
// do not affect src (and vice versa).
func deepMerge(dst, src map[string]any) {
	for key, srcVal := range src {
		if srcMap, ok := srcVal.(map[string]any); ok {
			if dstMap, ok := dst[key].(map[string]any); ok {
				deepMerge(dstMap, srcMap)
			} else {
				cp := make(map[string]any, len(srcMap))
				deepMerge(cp, srcMap)
				dst[key] = cp
			}
			continue
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

// ResolveClientValues reads and merges all client-supplied Helm values from the
// ArgoCD CR. Merge order (lowest to highest priority):
//
//  1. spec.valuesFrom entries without targetPath, in list order.
//  2. Inline spec.values.
//  3. spec.valuesFrom entries with targetPath, in list order (always wins).
//
// Returns nil when neither field is set.
func ResolveClientValues(ctx context.Context, cl client.Client, obj *apiv1alpha1.ArgoCD) (map[string]any, error) {
	result := map[string]any{}

	if err := mergeValuesRefs(ctx, cl, obj.Namespace, result, obj.Spec.ValuesFrom, false); err != nil {
		return nil, err
	}

	if obj.Spec.Values != nil && len(obj.Spec.Values.Raw) > 0 {
		var inline map[string]any
		if err := json.Unmarshal(obj.Spec.Values.Raw, &inline); err != nil {
			return nil, fmt.Errorf("decoding spec.values: %w", err)
		}
		deepMerge(result, inline)
	}

	if err := mergeValuesRefs(ctx, cl, obj.Namespace, result, obj.Spec.ValuesFrom, true); err != nil {
		return nil, err
	}

	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// mergeValuesRefs iterates refs and applies each one into dst.
// When withTargetPath is false, only entries without a targetPath are processed;
// when true, only entries with a targetPath are processed.
// targetPath entries are applied directly to dst (not via a temporary map) so
// that chartutil can navigate into existing arrays and modify only the targeted
// index, leaving other elements intact.
func mergeValuesRefs(ctx context.Context, cl client.Client, namespace string, dst map[string]any, refs []apiv1alpha1.ValuesReference, withTargetPath bool) error {
	for _, ref := range refs {
		if (ref.TargetPath != "") != withTargetPath {
			continue
		}
		if withTargetPath {
			if err := applyTargetPath(ctx, cl, namespace, dst, ref); err != nil {
				return err
			}
			continue
		}
		vals, err := readValuesReference(ctx, cl, namespace, ref)
		if err != nil {
			return err
		}
		deepMerge(dst, vals)
	}
	return nil
}

// applyTargetPath reads the scalar value from ref and writes it at
// ref.TargetPath directly into dst. Writing into dst rather than a fresh map
// preserves existing array contents at indices other than the targeted one.
func applyTargetPath(ctx context.Context, cl client.Client, namespace string, dst map[string]any, ref apiv1alpha1.ValuesReference) error {
	valuesKey := ref.ValuesKey
	if valuesKey == "" {
		valuesKey = "values.yaml"
	}
	raw, err := readRawValue(ctx, cl, namespace, ref, valuesKey)
	if err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	if ref.Literal {
		err = chartutil.ReplacePathLiteralValue(dst, ref.TargetPath, strings.TrimSpace(raw))
	} else {
		err = chartutil.ReplacePathValue(dst, ref.TargetPath, strings.TrimSpace(raw))
	}
	if err != nil {
		return fmt.Errorf("applying targetPath %q from %s %q key %q: %w", ref.TargetPath, ref.Kind, ref.Name, valuesKey, err)
	}
	return nil
}

// readValuesReference reads a single ConfigMap or Secret from the onboarding
// cluster and returns its parsed Helm values as a map. It is only called for
// entries without a targetPath; targetPath entries are handled by applyTargetPath.
// Optional references that are missing are silently ignored.
func readValuesReference(ctx context.Context, cl client.Client, namespace string, ref apiv1alpha1.ValuesReference) (map[string]any, error) {
	valuesKey := ref.ValuesKey
	if valuesKey == "" {
		valuesKey = "values.yaml"
	}

	raw, err := readRawValue(ctx, cl, namespace, ref, valuesKey)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}

	var values map[string]any
	if err := yaml.Unmarshal([]byte(raw), &values); err != nil {
		return nil, fmt.Errorf("parsing values from %s %q key %q: %w", ref.Kind, ref.Name, valuesKey, err)
	}
	return values, nil
}

// readRawValue fetches the raw string content of the given key from a ConfigMap
// or Secret. Returns ("", nil) when the reference is optional and not found.
func readRawValue(ctx context.Context, cl client.Client, namespace string, ref apiv1alpha1.ValuesReference, valuesKey string) (string, error) {
	switch ref.Kind {
	case "ConfigMap":
		return readRawFromConfigMap(ctx, cl, namespace, ref, valuesKey)
	case "Secret":
		return readRawFromSecret(ctx, cl, namespace, ref, valuesKey)
	default:
		return "", fmt.Errorf("unsupported spec.valuesFrom kind %q: must be ConfigMap or Secret", ref.Kind)
	}
}

func readRawFromConfigMap(ctx context.Context, cl client.Client, namespace string, ref apiv1alpha1.ValuesReference, valuesKey string) (string, error) {
	obj := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: namespace}, obj); err != nil {
		if apierrors.IsNotFound(err) && ref.Optional {
			return "", nil
		}
		return "", fmt.Errorf("reading ConfigMap %q from spec.valuesFrom: %w", ref.Name, err)
	}
	val, ok := obj.Data[valuesKey]
	if !ok {
		return "", fmt.Errorf("key %q not found in ConfigMap %q", valuesKey, ref.Name)
	}
	return val, nil
}

func readRawFromSecret(ctx context.Context, cl client.Client, namespace string, ref apiv1alpha1.ValuesReference, valuesKey string) (string, error) {
	obj := &corev1.Secret{}
	if err := cl.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: namespace}, obj); err != nil {
		if apierrors.IsNotFound(err) && ref.Optional {
			return "", nil
		}
		return "", fmt.Errorf("reading Secret %q from spec.valuesFrom: %w", ref.Name, err)
	}
	val, ok := obj.Data[valuesKey]
	if !ok {
		return "", fmt.Errorf("key %q not found in Secret %q", valuesKey, ref.Name)
	}
	return string(val), nil
}
