package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadSharedConfig verifies that one YAML document configures CLIProxyAPI and LLMGW.
func TestLoadSharedConfig(t *testing.T) {
	path := writeConfig(t, `
host: 127.0.0.1
port: 8088
auth-dir: /tmp/auth
disable-image-generation: true
request-retry: 1
max-retry-credentials: 2
routing:
  strategy: round-robin
  session-affinity: false
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
llmgw:
  postgres-dsn-env: TEST_DSN
  key-pepper-env: TEST_PEPPER
  usage-retention-days: 35
`)
	cfg, err := Load(path, mapEnv(map[string]string{
		"TEST_DSN":    "postgres://example",
		"TEST_PEPPER": strings.Repeat("p", 32),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy.Host != "127.0.0.1" || cfg.Proxy.Port != 8088 {
		t.Fatalf("proxy address = %s:%d", cfg.Proxy.Host, cfg.Proxy.Port)
	}
	if cfg.UsageRetention != 35*24*time.Hour {
		t.Fatalf("retention = %s", cfg.UsageRetention)
	}
	if cfg.LLMGW.UsageOutstandingCapacity != 64 {
		t.Fatalf("usage outstanding capacity = %d, want 64", cfg.LLMGW.UsageOutstandingCapacity)
	}
	if cfg.MaxUsageRecords != 8 {
		t.Fatalf("max usage records = %d, want 8", cfg.MaxUsageRecords)
	}
	if got, err := cfg.DatabaseDSN(mapEnv(map[string]string{"TEST_DSN": "postgres://example"})); err != nil || got != "postgres://example" {
		t.Fatalf("database DSN = %q, %v", got, err)
	}
	if got, err := cfg.KeyPepper(mapEnv(map[string]string{"TEST_PEPPER": strings.Repeat("p", 32)})); err != nil || len(got) != 32 {
		t.Fatalf("key pepper length = %d, %v", len(got), err)
	}
}

func TestUsageBackpressureRequiresImageGenerationFullyDisabled(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "missing", value: ""},
		{name: "false boolean", value: "disable-image-generation: false\n"},
		{name: "false string", value: "disable-image-generation: \"false\"\n"},
		{name: "zero", value: "disable-image-generation: 0\n"},
		{name: "zero string", value: "disable-image-generation: \"0\"\n"},
		{name: "chat", value: "disable-image-generation: chat\n"},
		{name: "chat string", value: "disable-image-generation: \"chat\"\n"},
		{name: "passthrough", value: "disable-image-generation: passthrough\n"},
		{name: "passthrough string", value: "disable-image-generation: \"passthrough\"\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := strings.Replace(
				secureConfig,
				"disable-image-generation: true\n",
				test.value,
				1,
			)
			_, err := Load(writeConfig(t, value), mapEnv(nil))
			if err == nil || !strings.Contains(err.Error(), "disable-image-generation must be true") {
				t.Fatalf("Load() error = %v, want image-generation rejection", err)
			}
		})
	}
}

func TestUsageBackpressureRejectsPayloadRulesThatCanRestoreImageGeneration(t *testing.T) {
	path := writeConfig(t, secureConfig+`
payload:
  override-raw:
    - models:
        - name: gpt-*
          protocol: openai-response
      params:
        tools: '[{"type":"image_generation"}]'
`)

	_, err := Load(path, mapEnv(nil))

	if err == nil || !strings.Contains(err.Error(), "payload write rules must be empty") {
		t.Fatalf("Load() error = %v, want payload write-rule rejection", err)
	}
}

// TestSecurityRejectsUnsafeConfiguration verifies unsafe inbound options are rejected before the SDK can rewrite the file.
func TestSecurityRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		env  map[string]string
	}{
		{
			name: "api keys",
			yaml: secureConfig + "api-keys:\n  - forbidden\n",
		},
		{
			name: "remote management listener",
			yaml: strings.Replace(secureConfig, "allow-remote: false", "allow-remote: true", 1),
		},
		{
			name: "remote management plaintext secret",
			yaml: strings.Replace(secureConfig, "secret-key: \"\"", "secret-key: plaintext", 1),
		},
		{
			name: "remote management whitespace secret",
			yaml: strings.Replace(secureConfig, "secret-key: \"\"", "secret-key: \" \"", 1),
		},
		{
			name: "control panel enabled",
			yaml: strings.Replace(secureConfig, "disable-control-panel: true", "disable-control-panel: false", 1),
		},
		{
			name: "home enabled",
			yaml: secureConfig + "home:\n  enabled: true\n",
		},
		{
			name: "pprof enabled",
			yaml: secureConfig + "pprof:\n  enable: true\n",
		},
		{
			name: "management password environment",
			yaml: secureConfig,
			env:  map[string]string{"MANAGEMENT_PASSWORD": "forbidden"},
		},
		{
			name: "empty auth directory",
			yaml: strings.Replace(secureConfig, "auth-dir: /tmp/auth", "auth-dir: \"\"", 1),
		},
		{
			name: "too short usage retention",
			yaml: secureConfig + "llmgw:\n  usage-retention-days: 1\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, test.yaml)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			_, err = Load(path, mapEnv(test.env))
			if err == nil {
				t.Fatal("Load() error = nil")
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("configuration file changed:\nwant %q\n got %q", before, after)
			}
		})
	}
}

// TestSecretsRejectMissingAndShortValues verifies database and pepper secrets are resolved only when needed.
func TestSecretsRejectMissingAndShortValues(t *testing.T) {
	path := writeConfig(t, secureConfig+`
llmgw:
  postgres-dsn-env: TEST_DSN
  key-pepper-env: TEST_PEPPER
`)
	cfg, err := Load(path, mapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := cfg.DatabaseDSN(mapEnv(nil)); err == nil {
		t.Fatal("DatabaseDSN() error = nil")
	}
	if _, err := cfg.KeyPepper(mapEnv(nil)); err == nil {
		t.Fatal("KeyPepper() missing error = nil")
	}
	if _, err := cfg.KeyPepper(mapEnv(map[string]string{"TEST_PEPPER": strings.Repeat("p", 31)})); err == nil {
		t.Fatal("KeyPepper() short value error = nil")
	}
}

// secureConfig is the minimum accepted shared configuration.
const secureConfig = `
host: 127.0.0.1
port: 8088
auth-dir: /tmp/auth
disable-image-generation: true
request-retry: 1
max-retry-credentials: 2
routing:
  strategy: round-robin
  session-affinity: false
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
`

// writeConfig writes a configuration fixture and returns its path.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mapEnv adapts a map to getenv's function shape.
func mapEnv(values map[string]string) func(string) string {
	return func(name string) string {
		return values[name]
	}
}
