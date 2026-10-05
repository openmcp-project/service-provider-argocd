package argocd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// testCertPEM generates a self-signed certificate with the given Common Name
// and DNS SANs and returns its PEM encoding. It lets the value-rendering tests
// exercise the real hostname-extraction path instead of a fake blob.
func testCertPEM(t *testing.T, commonName string, dnsNames ...string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		DNSNames:              dnsNames,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	var buf bytes.Buffer
	if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	return buf.String()
}

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
	const host = "git.example.com"
	pem := testCertPEM(t, "test-ca", host)

	got, err := buildValues(nil, ExposureValues{}, pem)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected non-nil values when CA cert is provided")
	}

	m := decodeValues(t, got)
	gotPEM := nestedString(t, m, "configs", "tls", "certificates", host)
	if gotPEM != pem {
		t.Fatalf("configs.tls.certificates[%q] = %q, want %q", host, gotPEM, pem)
	}
}

func TestBuildValues_CAPreservesBaseValues(t *testing.T) {
	const host = "git.example.com"
	pem := testCertPEM(t, "test-ca", host)
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
	// CA is injected alongside the existing configs, keyed by hostname.
	gotPEM := nestedString(t, m, "configs", "tls", "certificates", host)
	if gotPEM != pem {
		t.Fatalf("configs.tls.certificates[%q] = %q, want %q", host, gotPEM, pem)
	}
}

func TestBuildValues_ExposureAndCA(t *testing.T) {
	const host = "git.example.com"
	pem := testCertPEM(t, "test-ca", host)
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
	// CA fragment is present alongside it, keyed by hostname.
	gotPEM := nestedString(t, m, "configs", "tls", "certificates", host)
	if gotPEM != pem {
		t.Fatalf("configs.tls.certificates[%q] = %q, want %q", host, gotPEM, pem)
	}
}

func TestBuildValues_CAMultipleHosts(t *testing.T) {
	hosts := []string{"git.example.com", "registry.example.com"}
	pem := testCertPEM(t, "test-ca", hosts...)

	got, err := buildValues(nil, ExposureValues{}, pem)
	if err != nil {
		t.Fatal(err)
	}

	m := decodeValues(t, got)
	for _, host := range hosts {
		gotPEM := nestedString(t, m, "configs", "tls", "certificates", host)
		if gotPEM != pem {
			t.Fatalf("configs.tls.certificates[%q] = %q, want %q", host, gotPEM, pem)
		}
	}
}

func TestBuildValues_CAFallsBackToCommonName(t *testing.T) {
	// No SAN; the Common Name is a valid hostname and is used as the key.
	const cn = "git.internal"
	pem := testCertPEM(t, cn)

	got, err := buildValues(nil, ExposureValues{}, pem)
	if err != nil {
		t.Fatal(err)
	}

	m := decodeValues(t, got)
	gotPEM := nestedString(t, m, "configs", "tls", "certificates", cn)
	if gotPEM != pem {
		t.Fatalf("configs.tls.certificates[%q] = %q, want %q", cn, gotPEM, pem)
	}
}

func TestBuildValues_CAUnparseableIsError(t *testing.T) {
	_, err := buildValues(nil, ExposureValues{}, "-----BEGIN CERTIFICATE-----\nMIItest\n-----END CERTIFICATE-----\n")
	if err == nil {
		t.Fatal("expected an error for an unparseable CA cert, got nil")
	}
}

func TestBuildValues_CANoUsableHostIsError(t *testing.T) {
	// A cert with neither SAN nor a hostname-shaped Common Name cannot key the
	// trusted-certificate ConfigMap, so it must surface as an error.
	pem := testCertPEM(t, "ACME Corporate Root CA")
	_, err := buildValues(nil, ExposureValues{}, pem)
	if err == nil {
		t.Fatal("expected an error when the cert has no usable hostname, got nil")
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

func TestCertHostnames(t *testing.T) {
	t.Run("single SAN", func(t *testing.T) {
		got, err := certHostnames(testCertPEM(t, "test-ca", "git.example.com"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "git.example.com" {
			t.Fatalf("got %v, want [git.example.com]", got)
		}
	})

	t.Run("dedups across a chain", func(t *testing.T) {
		// Two concatenated certs sharing a SAN must yield one key.
		chain := testCertPEM(t, "intermediate", "git.example.com") +
			testCertPEM(t, "root", "git.example.com", "registry.example.com")
		got, err := certHostnames(chain)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{"git.example.com": true, "registry.example.com": true}
		if len(got) != len(want) {
			t.Fatalf("got %v, want 2 unique hosts", got)
		}
		for _, h := range got {
			if !want[h] {
				t.Fatalf("unexpected host %q in %v", h, got)
			}
		}
	})

	t.Run("wildcard SAN is skipped", func(t *testing.T) {
		// A wildcard cannot be an argocd-tls-certs-cm key, so it is skipped and
		// the usable SAN remains.
		got, err := certHostnames(testCertPEM(t, "test-ca", "*.example.com", "git.example.com"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "git.example.com" {
			t.Fatalf("got %v, want [git.example.com]", got)
		}
	})

	t.Run("errors on no usable host", func(t *testing.T) {
		if _, err := certHostnames(testCertPEM(t, "ACME Corporate Root CA")); err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}
