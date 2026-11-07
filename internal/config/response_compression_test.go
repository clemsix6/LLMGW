package config

import (
	"strings"
	"testing"
)

// TestResponseCompressionIsDisabled verifies the loaded SDK configuration never
// compresses client responses, whether the operator leaves the setting unset or
// disables it explicitly.
func TestResponseCompressionIsDisabled(t *testing.T) {
	tests := []struct {
		name  string
		extra string
	}{
		{name: "unset", extra: ""},
		{name: "explicitly disabled", extra: "response-compression:\n  enabled: false\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, secureConfig+test.extra), mapEnv(nil))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Proxy.EffectiveResponseCompression().IsEnabled() {
				t.Fatal("response compression is enabled, want disabled")
			}
		})
	}
}

// TestResponseCompressionCannotBeEnabled verifies an operator enabling response
// compression is refused at load instead of silently overridden.
func TestResponseCompressionCannotBeEnabled(t *testing.T) {
	path := writeConfig(t, secureConfig+"response-compression:\n  enabled: true\n")

	_, err := Load(path, mapEnv(nil))

	if err == nil || !strings.Contains(err.Error(), "response-compression.enabled must be false") {
		t.Fatalf("Load() error = %v, want response-compression rejection", err)
	}
}
