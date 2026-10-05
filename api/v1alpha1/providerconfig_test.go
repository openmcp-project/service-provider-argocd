package v1alpha1

import "testing"

func TestProviderConfig_CABundleSecret(t *testing.T) {
	tests := []struct {
		name string
		pc   *ProviderConfig
		want string
	}{
		{
			name: "nil receiver",
			pc:   nil,
			want: "",
		},
		{
			name: "empty spec",
			pc:   &ProviderConfig{},
			want: "",
		},
		{
			name: "secret name set",
			pc: &ProviderConfig{
				Spec: ProviderConfigSpec{CABundleSecret: "my-ca"},
			},
			want: "my-ca",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.pc.CABundleSecret()
			if got != tt.want {
				t.Fatalf("CABundleSecret() = %q, want %q", got, tt.want)
			}
		})
	}
}
