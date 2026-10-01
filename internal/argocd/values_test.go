package argocd

import (
	"encoding/json"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// rawValues is a test helper that encodes a Go map as the JSON blob that
// ProviderConfig.spec.versions[].values carries.
func rawValues(t *testing.T, v map[string]any) *apiextensionsv1.JSON {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal test values: %v", err)
	}
	return &apiextensionsv1.JSON{Raw: b}
}

// decodeValues is a test helper that unmarshals a JSON blob into a plain map.
func decodeValues(t *testing.T, j *apiextensionsv1.JSON) map[string]any {
	t.Helper()
	if j == nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(j.Raw, &out); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}
	return out
}

// nestedString navigates a nested map[string]any and returns the string value
// at the given path, failing the test if any intermediate key is absent or the
// final value is not a string.
func nestedString(t *testing.T, m map[string]any, keys ...string) string {
	t.Helper()
	cur := m
	for i, k := range keys[:len(keys)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("key %q not found or not a map at path[%d]", k, i)
		}
		cur = next
	}
	last := keys[len(keys)-1]
	v, ok := cur[last].(string)
	if !ok {
		t.Fatalf("key %q not found or not a string", last)
	}
	return v
}

func TestBuildValues_EmptyNoCA(t *testing.T) {
	got, err := buildValues(nil, ExposureValues{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil values when nothing is configured, got %s", got.Raw)
	}
}

func TestBuildValues_CAOnly(t *testing.T) {
	const pem = "-----BEGIN CERTIFICATE-----\nMIItest\n-----END CERTIFICATE-----\n"

	got, err := buildValues(nil, ExposureValues{}, pem)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected non-nil values when CA cert is provided")
	}

	m := decodeValues(t, got)
	gotPEM := nestedString(t, m, "configs", "tls", "certificates", "custom-ca.crt")
	if gotPEM != pem {
		t.Fatalf("configs.tls.certificates.custom-ca.crt = %q, want %q", gotPEM, pem)
	}
}

func TestBuildValues_CAPreservesBaseValues(t *testing.T) {
	const pem = "--- CA PEM ---"
	base := rawValues(t, map[string]any{
		"redis": map[string]any{"enabled": true},
		"configs": map[string]any{
			"cm": map[string]any{"url": "https://argocd.example.com"},
		},
	})

	got, err := buildValues(base, ExposureValues{}, pem)
	if err != nil {
		t.Fatal(err)
	}

	m := decodeValues(t, got)

	// Base values survive.
	if redis, ok := m["redis"].(map[string]any); !ok || redis["enabled"] != true {
		t.Fatalf("expected redis.enabled=true to survive CA merge, got %v", m["redis"])
	}
	// Pre-existing configs.cm key survives deep merge.
	gotURL := nestedString(t, m, "configs", "cm", "url")
	if gotURL != "https://argocd.example.com" {
		t.Fatalf("configs.cm.url = %q, want original value", gotURL)
	}
	// CA is injected alongside the existing configs.
	gotPEM := nestedString(t, m, "configs", "tls", "certificates", "custom-ca.crt")
	if gotPEM != pem {
		t.Fatalf("configs.tls.certificates.custom-ca.crt = %q, want %q", gotPEM, pem)
	}
}

func TestBuildValues_ExposureAndCA(t *testing.T) {
	const pem = "--- CA PEM ---"
	exposure := ExposureValues{
		Enabled:     true,
		Host:        "argocd.example.com",
		DNSClass:    "garden",
		DNSTTL:      3600,
		CertPurpose: "managed",
	}

	got, err := buildValues(nil, exposure, pem)
	if err != nil {
		t.Fatal(err)
	}

	m := decodeValues(t, got)

	// Exposure fragment is present.
	gotURL := nestedString(t, m, "configs", "cm", "url")
	if gotURL != "https://argocd.example.com" {
		t.Fatalf("configs.cm.url = %q, want https://argocd.example.com", gotURL)
	}
	// CA fragment is present alongside it.
	gotPEM := nestedString(t, m, "configs", "tls", "certificates", "custom-ca.crt")
	if gotPEM != pem {
		t.Fatalf("configs.tls.certificates.custom-ca.crt = %q, want %q", gotPEM, pem)
	}
}

func TestBuildValues_NoCARegressionBaseOnly(t *testing.T) {
	base := rawValues(t, map[string]any{"server": map[string]any{"replicas": 2}})

	got, err := buildValues(base, ExposureValues{}, "")
	if err != nil {
		t.Fatal(err)
	}

	m := decodeValues(t, got)
	if _, ok := m["configs"]; ok {
		t.Fatalf("expected no configs key when CA is empty and exposure is disabled, got %v", m["configs"])
	}
	if server, ok := m["server"].(map[string]any); !ok || server["replicas"] != float64(2) {
		t.Fatalf("expected server.replicas=2 to be preserved, got %v", m["server"])
	}
}
