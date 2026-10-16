package argocd

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
