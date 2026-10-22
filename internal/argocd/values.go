package argocd

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

//go:embed configurations/argocd-vault.yaml
var argocdVaultYAML []byte

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

// vaultPluginValues carries the ArgoCD Vault Plugin settings resolved from the
// ProviderConfig. When enabled is false the other fields are ignored.
type vaultPluginValues struct {
	enabled   bool
	registry  string
	tag       string
	namespace string
}

// buildValues merges the operator-supplied chart values with the exposure
// fragment derived from the ArgoCD request. Precedence is:
//
//	chart defaults  <  operator values (version.Values)  <  vault plugin fragment  <  exposure fragment
//
// so that platform-provided values cannot silently drop the annotations that
// exposure depends on. A nil result means "no values", which is a valid input
// for a HelmRelease.
func buildValues(base *apiextensionsv1.JSON, exposure ExposureValues, vaultPlugin vaultPluginValues, version string) (*apiextensionsv1.JSON, error) {
	values := map[string]any{}
	if base != nil && len(base.Raw) > 0 {
		if err := json.Unmarshal(base.Raw, &values); err != nil {
			return nil, fmt.Errorf("decoding operator-supplied values: %w", err)
		}
	}

	if vaultPlugin.enabled {
		repository, err := globalImageRepository(values)
		if err != nil {
			return nil, fmt.Errorf("loading vault plugin values: %w", err)
		}
		vaultFragment, err := vaultPluginFragment(version, repository, vaultPlugin.registry, vaultPlugin.tag, vaultPlugin.namespace)
		if err != nil {
			return nil, fmt.Errorf("loading vault plugin values: %w", err)
		}
		deepMerge(values, vaultFragment)
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

// globalImageRepository extracts global.image.repository from the
// operator-supplied values map, falling back to the chart default when the key
// is unset. It returns an error if the key is present but not a non-empty
// string, since the argocd-vault.yaml template renders with missingkey=error
// and must always receive a usable repository.
func globalImageRepository(values map[string]any) (string, error) {
	var defaultGlobalImageRepository = "ghcr-remote.common.repositories.cloud.sap/argoproj/argo-helm/argo-cd"

	global, ok := values["global"].(map[string]any)
	if !ok {
		return defaultGlobalImageRepository, nil
	}
	image, ok := global["image"].(map[string]any)
	if !ok {
		return defaultGlobalImageRepository, nil
	}
	raw, ok := image["repository"]
	if !ok {
		return defaultGlobalImageRepository, nil
	}
	repository, ok := raw.(string)
	if !ok || repository == "" {
		return "", fmt.Errorf("global.image.repository must be a non-empty string, got %T", raw)
	}
	return repository, nil
}

// vaultPluginFragment parses the bundled argocd-vault.yaml and returns it as
// an untyped map suitable for deep-merging into the HelmRelease values.
// The YAML is first rendered as a Go template using the requested ArgoCD version so that {{ .Values.global.image.repository }}
// and {{ .Values.global.image.tag }} references are resolved before parsing.
// The AVP initContainer container has its image set to registry:tag so
// that the plugin binary comes from the operator-configured image.
func vaultPluginFragment(version, repository, registry, tag, namespace string) (map[string]any, error) {
	tmpl, err := template.New("argocd-vault").Option("missingkey=error").Parse(string(argocdVaultYAML))
	if err != nil {
		return nil, fmt.Errorf("parsing argocd-vault.yaml template: %w", err)
	}

	data := map[string]any{
		"Release": map[string]any{
			"Namespace": namespace,
		},
		"Values": map[string]any{
			"global": map[string]any{
				"image": map[string]any{
					"repository": repository,
					"tag":        version,
				},
			},
		},
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering argocd-vault.yaml template: %w", err)
	}

	jsonBytes, err := yaml.YAMLToJSON(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("converting argocd-vault.yaml to JSON: %w", err)
	}
	var fragment map[string]any
	if err := json.Unmarshal(jsonBytes, &fragment); err != nil {
		return nil, fmt.Errorf("decoding argocd-vault.yaml: %w", err)
	}
	avpImage := registry + ":" + tag
	repoServer, _ := fragment["repoServer"].(map[string]any)
	initContainers, _ := repoServer["initContainers"].([]any)
	for _, c := range initContainers {
		container, _ := c.(map[string]any)
		if container["name"] == "avp-binary" {
			container["image"] = avpImage
		}
	}
	return fragment, nil
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
