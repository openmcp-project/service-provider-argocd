package argocd

import (
	"bytes"
	"context"
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

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1alpha1 "github.com/openmcp-project/service-provider-argocd/api/v1alpha1"
)

func TestImagePullSecretNames(t *testing.T) {
	tests := []struct {
		name    string
		values  *apiextensionsv1.JSON
		want    []string
		wantErr bool
	}{
		{
			name:   "nil values",
			values: nil,
			want:   nil,
		},
		{
			name:   "empty values",
			values: &apiextensionsv1.JSON{},
			want:   nil,
		},
		{
			name:   "no imagePullSecrets",
			values: &apiextensionsv1.JSON{Raw: []byte(`{"server":{"replicas":2}}`)},
			want:   nil,
		},
		{
			name: "single imagePullSecret",
			values: &apiextensionsv1.JSON{Raw: []byte(
				`{"global":{"imagePullSecrets":[{"name":"regcred"}]}}`,
			)},
			want: []string{"regcred"},
		},
		{
			name: "multiple imagePullSecrets, ignoring empty names",
			values: &apiextensionsv1.JSON{Raw: []byte(
				`{"global":{"imagePullSecrets":[{"name":"regcred"},{"name":""},{"name":"other"}]}}`,
			)},
			want: []string{"regcred", "other"},
		},
		{
			name:    "malformed JSON",
			values:  &apiextensionsv1.JSON{Raw: []byte(`{`)},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ImagePullSecretNames(tt.values)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ImagePullSecretNames() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ImagePullSecretNames() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("ImagePullSecretNames() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

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
	got, err := buildValues(nil, nil, ExposureValues{}, "")
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

	got, err := buildValues(nil, nil, ExposureValues{}, pem)
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

	got, err := buildValues(base, nil, ExposureValues{}, pem)
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

	got, err := buildValues(nil, nil, exposure, pem)
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

	got, err := buildValues(nil, nil, ExposureValues{}, pem)
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

	got, err := buildValues(nil, nil, ExposureValues{}, pem)
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
	_, err := buildValues(nil, nil, ExposureValues{}, "-----BEGIN CERTIFICATE-----\nMIItest\n-----END CERTIFICATE-----\n")
	if err == nil {
		t.Fatal("expected an error for an unparseable CA cert, got nil")
	}
}

func TestBuildValues_CANoUsableHostIsError(t *testing.T) {
	// A cert with neither SAN nor a hostname-shaped Common Name cannot key the
	// trusted-certificate ConfigMap, so it must surface as an error.
	pem := testCertPEM(t, "ACME Corporate Root CA")
	_, err := buildValues(nil, nil, ExposureValues{}, pem)
	if err == nil {
		t.Fatal("expected an error when the cert has no usable hostname, got nil")
	}
}

func TestBuildValues_NoCARegressionBaseOnly(t *testing.T) {
	base := rawValues(t, map[string]any{"server": map[string]any{"replicas": 2}})

	got, err := buildValues(base, nil, ExposureValues{}, "")
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

// newConfigMap builds a ConfigMap with string data for test setup.
func newConfigMap(name, namespace string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
	}
}

// newArgoCD builds a minimal ArgoCD CR for testing ResolveClientValues.
func newArgoCD(namespace string, values *apiextensionsv1.JSON, refs []apiv1alpha1.ValuesReference) *apiv1alpha1.ArgoCD {
	return &apiv1alpha1.ArgoCD{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: namespace},
		Spec: apiv1alpha1.ArgoCDSpec{
			Version:    "v3.1.0",
			Values:     values,
			ValuesFrom: refs,
		},
	}
}

// cmpMap fails the test when got and want differ, printing both as JSON.
// json.Marshal sorts map keys, so the comparison is deterministic.
func cmpMap(t *testing.T, got, want map[string]any) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("values mismatch:\n  got:  %s\n  want: %s", gotJSON, wantJSON)
	}
}

func TestResolveClientValues(t *testing.T) {
	const ns = "test-ns"
	ctx := context.Background()

	tests := []struct {
		name    string
		objects []client.Object
		inline  map[string]any
		refs    []apiv1alpha1.ValuesReference
		want    map[string]any
		wantErr bool
	}{
		// ── nil / empty ──────────────────────────────────────────────────────────
		{
			name: "both fields absent returns nil",
			want: nil,
		},
		{
			name:   "inline only",
			inline: map[string]any{"a": "inline"},
			want:   map[string]any{"a": "inline"},
		},

		// ── single reference ─────────────────────────────────────────────────────
		{
			name: "single ConfigMap",
			objects: []client.Object{
				newConfigMap("vals", ns, map[string]string{"values.yaml": "a: from-cm\n"}),
			},
			refs: []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "vals"}},
			want: map[string]any{"a": "from-cm"},
		},
		{
			name: "single Secret",
			objects: []client.Object{
				newSecret("sec", ns, map[string][]byte{"values.yaml": []byte("a: from-secret\n")}),
			},
			refs: []apiv1alpha1.ValuesReference{{Kind: "Secret", Name: "sec"}},
			want: map[string]any{"a": "from-secret"},
		},

		// ── merge order: pass 1 (non-targetPath) ─────────────────────────────────
		{
			name: "later non-targetPath entry beats earlier at same key",
			objects: []client.Object{
				newConfigMap("first", ns, map[string]string{"values.yaml": "x: first\n"}),
				newConfigMap("second", ns, map[string]string{"values.yaml": "x: second\n"}),
			},
			refs: []apiv1alpha1.ValuesReference{
				{Kind: "ConfigMap", Name: "first"},
				{Kind: "ConfigMap", Name: "second"},
			},
			want: map[string]any{"x": "second"},
		},

		// ── merge order: pass 2 (inline) ─────────────────────────────────────────
		{
			name: "inline beats non-targetPath ConfigMap",
			objects: []client.Object{
				newConfigMap("vals", ns, map[string]string{"values.yaml": "x: from-cm\n"}),
			},
			refs:   []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "vals"}},
			inline: map[string]any{"x": "inline"},
			want:   map[string]any{"x": "inline"},
		},

		// ── merge order: pass 3 (targetPath) ─────────────────────────────────────
		{
			name: "targetPath beats inline at same key",
			objects: []client.Object{
				newConfigMap("tp", ns, map[string]string{"values.yaml": "from-tp"}),
			},
			refs:   []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "tp", TargetPath: "x"}},
			inline: map[string]any{"x": "inline"},
			want:   map[string]any{"x": "from-tp"},
		},
		{
			// A non-targetPath entry listed after a targetPath entry must not win.
			name: "targetPath beats non-targetPath regardless of list position",
			objects: []client.Object{
				newConfigMap("plain-a", ns, map[string]string{"values.yaml": "x: A\n"}),
				newConfigMap("tp-b", ns, map[string]string{"values.yaml": "B"}),
				newConfigMap("plain-c", ns, map[string]string{"values.yaml": "x: C\n"}),
			},
			refs: []apiv1alpha1.ValuesReference{
				{Kind: "ConfigMap", Name: "plain-a"},
				{Kind: "ConfigMap", Name: "tp-b", TargetPath: "x"},
				{Kind: "ConfigMap", Name: "plain-c"},
			},
			want: map[string]any{"x": "B"},
		},
		{
			name: "later targetPath beats earlier targetPath",
			objects: []client.Object{
				newConfigMap("tp1", ns, map[string]string{"values.yaml": "first"}),
				newConfigMap("tp2", ns, map[string]string{"values.yaml": "second"}),
			},
			refs: []apiv1alpha1.ValuesReference{
				{Kind: "ConfigMap", Name: "tp1", TargetPath: "x"},
				{Kind: "ConfigMap", Name: "tp2", TargetPath: "x"},
			},
			want: map[string]any{"x": "second"},
		},

		// ── array index ──────────────────────────────────────────────────────────
		{
			name: "targetPath array index patches only the targeted element",
			objects: []client.Object{
				newConfigMap("base", ns, map[string]string{
					"values.yaml": "items:\n  - one\n  - two\n  - three\n",
				}),
				newConfigMap("patch", ns, map[string]string{"values.yaml": "replaced"}),
			},
			refs: []apiv1alpha1.ValuesReference{
				{Kind: "ConfigMap", Name: "base"},
				{Kind: "ConfigMap", Name: "patch", TargetPath: "items[1]"},
			},
			want: map[string]any{
				"items": []any{"one", "replaced", "three"},
			},
		},

		// ── optional / missing ────────────────────────────────────────────────────
		{
			name: "optional missing ConfigMap silently skipped",
			refs: []apiv1alpha1.ValuesReference{
				{Kind: "ConfigMap", Name: "missing", Optional: true},
			},
			want: nil,
		},
		{
			name: "non-optional missing ConfigMap is an error",
			refs: []apiv1alpha1.ValuesReference{
				{Kind: "ConfigMap", Name: "missing"},
			},
			wantErr: true,
		},
		{
			// optional does not suppress a missing key within an existing resource
			name: "optional reference exists but key absent is still an error",
			objects: []client.Object{
				newConfigMap("vals", ns, map[string]string{"other.yaml": "x: v\n"}),
			},
			refs:    []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "vals", Optional: true}},
			wantErr: true,
		},

		// ── valuesKey / literal ───────────────────────────────────────────────────
		{
			name: "custom valuesKey",
			objects: []client.Object{
				newConfigMap("vals", ns, map[string]string{"custom.yaml": "x: custom\n"}),
			},
			refs: []apiv1alpha1.ValuesReference{
				{Kind: "ConfigMap", Name: "vals", ValuesKey: "custom.yaml"},
			},
			want: map[string]any{"x": "custom"},
		},
		{
			// With Literal=true the string "true" is kept as a string, not coerced to bool.
			name: "literal:true keeps value as string instead of parsing YAML scalar",
			objects: []client.Object{
				newConfigMap("tp", ns, map[string]string{"values.yaml": "true"}),
			},
			refs: []apiv1alpha1.ValuesReference{
				{Kind: "ConfigMap", Name: "tp", TargetPath: "flag", Literal: true},
			},
			want: map[string]any{"flag": "true"},
		},
		{
			// Without Literal, ReplacePathValue uses the --set parser which coerces "true" to bool.
			name: "literal:false (default) parses scalar so 'true' becomes bool not string",
			objects: []client.Object{
				newConfigMap("tp", ns, map[string]string{"values.yaml": "true"}),
			},
			refs: []apiv1alpha1.ValuesReference{
				{Kind: "ConfigMap", Name: "tp", TargetPath: "flag"},
			},
			want: map[string]any{"flag": true},
		},
		// ── literal:true vs literal:false: symbol-by-symbol ─────────────────────

		// case 1: comma — strvals treats it as a multi-assignment separator
		{
			name: "literal:true stores value with comma verbatim",
			objects: []client.Object{
				newConfigMap("cm", ns, map[string]string{"values.yaml": "first,second"}),
			},
			refs: []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "cm", TargetPath: "cfg", Literal: true}},
			want: map[string]any{"cfg": "first,second"},
		},
		{
			// Comma causes an error: after splitting on it, "second" has no = and
			// is treated as a bare key, which strvals rejects.
			name: "literal:false errors when value contains a comma",
			objects: []client.Object{
				newConfigMap("cm", ns, map[string]string{"values.yaml": "first,second"}),
			},
			refs:    []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "cm", TargetPath: "cfg"}},
			wantErr: true,
		},

		// case 2: equals — strvals only splits at the FIRST = (path=value boundary),
		// so extra = signs inside the value are NOT metacharacters and are safe.
		{
			name: "literal:true stores value with equals verbatim",
			objects: []client.Object{
				newConfigMap("cm", ns, map[string]string{"values.yaml": "key=value=test"}),
			},
			refs: []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "cm", TargetPath: "cfg", Literal: true}},
			want: map[string]any{"cfg": "key=value=test"},
		},
		{
			// = in the value is NOT a strvals metacharacter: extra = signs are
			// treated as literals, so literal:false and literal:true agree here.
			name: "literal:false: equals inside value is not a metacharacter",
			objects: []client.Object{
				newConfigMap("cm", ns, map[string]string{"values.yaml": "key=value=test"}),
			},
			refs: []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "cm", TargetPath: "cfg"}},
			want: map[string]any{"cfg": "key=value=test"},
		},

		// case 3: curly brackets — strvals decodes {...} as list notation
		{
			name: "literal:true stores value with curly brackets verbatim",
			objects: []client.Object{
				newConfigMap("cm", ns, map[string]string{"values.yaml": "{a,b,c}"}),
			},
			refs: []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "cm", TargetPath: "cfg", Literal: true}},
			want: map[string]any{"cfg": "{a,b,c}"},
		},
		{
			// Curly brackets cause silent corruption: no error is returned, but the
			// string is decoded as a list instead of stored verbatim.
			name: "literal:false silently corrupts curly-bracket value into a slice",
			objects: []client.Object{
				newConfigMap("cm", ns, map[string]string{"values.yaml": "{a,b,c}"}),
			},
			refs: []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "cm", TargetPath: "cfg"}},
			want: map[string]any{"cfg": []any{"a", "b", "c"}},
		},

		// case 4: square brackets — strvals interprets [...] as array-index path syntax
		{
			name: "literal:true stores value with square brackets verbatim",
			objects: []client.Object{
				newConfigMap("cm", ns, map[string]string{"values.yaml": "[a,b,c]"}),
			},
			refs: []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "cm", TargetPath: "cfg", Literal: true}},
			want: map[string]any{"cfg": "[a,b,c]"},
		},
		{
			// Square brackets cause an error AND corrupt the destination map with
			// stray keys before the error is returned.
			name: "literal:false errors when value contains square brackets",
			objects: []client.Object{
				newConfigMap("cm", ns, map[string]string{"values.yaml": "[a,b,c]"}),
			},
			refs:    []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "cm", TargetPath: "cfg"}},
			wantErr: true,
		},

		// ── Secret with targetPath ────────────────────────────────────────────────
		{
			name: "Secret with targetPath",
			objects: []client.Object{
				newSecret("sec", ns, map[string][]byte{"values.yaml": []byte("from-secret")}),
			},
			refs: []apiv1alpha1.ValuesReference{{Kind: "Secret", Name: "sec", TargetPath: "key"}},
			want: map[string]any{"key": "from-secret"},
		},

		// ── unsupported Kind ──────────────────────────────────────────────────────
		{
			name:    "unsupported Kind is an error",
			refs:    []apiv1alpha1.ValuesReference{{Kind: "UnsupportedKind", Name: "p"}},
			wantErr: true,
		},

		// ── nested and escaped targetPath ─────────────────────────────────────────
		{
			name: "nested targetPath sets deep key",
			objects: []client.Object{
				newConfigMap("cm", ns, map[string]string{"values.yaml": "example.com"}),
			},
			refs: []apiv1alpha1.ValuesReference{{Kind: "ConfigMap", Name: "cm", TargetPath: "server.host"}},
			want: map[string]any{"server": map[string]any{"host": "example.com"}},
		},
		{
			// \. escapes a literal dot in the key name (helm --set path syntax).
			name: "escaped dot in targetPath sets key containing a literal dot",
			objects: []client.Object{
				newConfigMap("cm", ns, map[string]string{"values.yaml": "master"}),
			},
			refs: []apiv1alpha1.ValuesReference{
				{Kind: "ConfigMap", Name: "cm", TargetPath: `nodeSelector.kubernetes\.io/role`},
			},
			want: map[string]any{
				"nodeSelector": map[string]any{"kubernetes.io/role": "master"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().
				WithScheme(newTestScheme(t)).
				WithObjects(tt.objects...).
				Build()

			var inlineJSON *apiextensionsv1.JSON
			if tt.inline != nil {
				inlineJSON = rawValues(t, tt.inline)
			}

			got, err := ResolveClientValues(ctx, cl, newArgoCD(ns, inlineJSON, tt.refs))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			cmpMap(t, got, tt.want)
		})
	}
}

func TestBuildValues_ClientValuesOverriddenByOperator(t *testing.T) {
	clientVals := map[string]any{
		"replicas": float64(1),
		"server":   map[string]any{"name": "test-name"},
	}
	operator := rawValues(t, map[string]any{
		"replicas": float64(3),
	})

	got, err := buildValues(operator, clientVals, ExposureValues{}, "")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeValues(t, got)
	if m["replicas"] != float64(3) {
		t.Fatalf("replicas = %v, want 3 (operator must override client)", m["replicas"])
	}
	// Client-only key survives.
	if server, ok := m["server"].(map[string]any); !ok || server["name"] != "test-name" {
		t.Fatalf("server.name = %v, want test-name (client-only key must survive)", m["server"])
	}
}

func TestBuildValues_NilClientValues(t *testing.T) {
	operator := rawValues(t, map[string]any{"replicas": float64(2)})

	got, err := buildValues(operator, nil, ExposureValues{}, "")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeValues(t, got)
	if m["replicas"] != float64(2) {
		t.Fatalf("replicas = %v, want 2", m["replicas"])
	}
}

func TestBuildValues_OperatorDeepMergesSharedNestedKey(t *testing.T) {
	// Both client and operator set keys under server.*. Operator must win at the
	// conflicting key; the client-only sub-key must survive (deep merge, not replace).
	clientVals := map[string]any{
		"server": map[string]any{
			"port": float64(80),
			"name": "client-name",
		},
	}
	operator := rawValues(t, map[string]any{
		"server": map[string]any{"port": float64(443)},
	})

	got, err := buildValues(operator, clientVals, ExposureValues{}, "")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeValues(t, got)
	server, ok := m["server"].(map[string]any)
	if !ok {
		t.Fatalf("server is not a map: %v", m["server"])
	}
	if server["port"] != float64(443) {
		t.Fatalf("server.port = %v, want 443 (operator must override client at nested key)", server["port"])
	}
	if server["name"] != "client-name" {
		t.Fatalf("server.name = %v, want client-name (client sub-key must survive deep merge)", server["name"])
	}
}
