package argocd

import (
	"context"
	"encoding/json"
	"fmt"
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

// buildValues merges client-supplied values, operator-supplied values, and the
// exposure fragment derived from the ArgoCD request. Precedence (lowest to highest):
//
//	client values (spec.valuesFrom + spec.values)  <  operator values (ProviderConfig)  <  exposure fragment
//
// clientValues must be pre-resolved by ResolveClientValues before being passed
// in. A nil result means "no values", which is a valid input for a HelmRelease.
func buildValues(operatorValues *apiextensionsv1.JSON, clientValues map[string]any, exposure ExposureValues) (*apiextensionsv1.JSON, error) {
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

// mergeValuesRefs iterates refs and deep-merges each resolved value into dst.
// When withTargetPath is false, only entries without a targetPath are processed;
// when true, only entries with a targetPath are processed.
func mergeValuesRefs(ctx context.Context, cl client.Client, namespace string, dst map[string]any, refs []apiv1alpha1.ValuesReference, withTargetPath bool) error {
	for _, ref := range refs {
		if (ref.TargetPath != "") != withTargetPath {
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

// readValuesReference reads a single ConfigMap or Secret from the onboarding
// cluster and returns its parsed Helm values. When TargetPath is set the
// referent's key content is treated as a scalar placed at that path rather
// than as a full values object. Optional references that are missing are
// silently ignored.
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

	if ref.TargetPath != "" {
		result := map[string]any{}
		var err error
		if ref.Literal {
			err = chartutil.ReplacePathLiteralValue(result, ref.TargetPath, strings.TrimSpace(raw))
		} else {
			err = chartutil.ReplacePathValue(result, ref.TargetPath, strings.TrimSpace(raw))
		}
		if err != nil {
			return nil, fmt.Errorf("parsing targetPath value from %s %q key %q: %w", ref.Kind, ref.Name, valuesKey, err)
		}
		return result, nil
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
