package command

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseAuthPriorityArguments proves argument validation happens in the parser,
// with no configuration, filesystem or database access.
func TestParseAuthPriorityArguments(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantErr      bool
		wantPriority int
	}{
		{name: "missing both", args: []string{"priority"}, wantErr: true},
		{name: "missing priority", args: []string{"priority", "claude-a.json"}, wantErr: true},
		{name: "extra argument", args: []string{"priority", "claude-a.json", "5", "6"}, wantErr: true},
		{name: "not an integer", args: []string{"priority", "claude-a.json", "high"}, wantErr: true},
		{name: "fractional", args: []string{"priority", "claude-a.json", "1.5"}, wantErr: true},
		{name: "positive", args: []string{"priority", "claude-a.json", "10"}, wantPriority: 10},
		{name: "negative last resort", args: []string{"priority", "claude-a.json", "-3"}, wantPriority: -3},
		{name: "zero clears", args: []string{"priority", "claude-a.json", "0"}, wantPriority: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			leaf, err := parseAuthLeaf(test.args, Streams{Err: &stderr})
			if test.wantErr {
				if err == nil || leaf != nil {
					t.Fatalf("parseAuthLeaf(%v) = (%v, %v), want an error", test.args, leaf != nil, err)
				}
				if !strings.Contains(stderr.String(), "priority") {
					t.Fatalf("usage output = %q, want it to mention priority", stderr.String())
				}
				return
			}
			if err != nil || leaf == nil {
				t.Fatalf("parseAuthLeaf(%v) = (%v, %v), want a leaf", test.args, leaf != nil, err)
			}
			parsed, err := parseAuthPriority(test.args[1:], Streams{Err: &stderr})
			if err != nil || parsed.id != "claude-a.json" || parsed.priority != test.wantPriority {
				t.Fatalf("parseAuthPriority = (%+v, %v), want priority %d", parsed, err, test.wantPriority)
			}
		})
	}
}

// TestAuthUsageListsPriority proves the usage text names the new leaf.
func TestAuthUsageListsPriority(t *testing.T) {
	var stderr bytes.Buffer
	_ = authUsage(Streams{Err: &stderr}, "boom")
	if !strings.Contains(stderr.String(), "priority ID N") {
		t.Fatalf("usage = %q, want it to list priority ID N", stderr.String())
	}
}

// TestRunAuthPriorityRejectsBadArgumentsBeforeConfiguration proves a malformed
// invocation fails on its arguments even when the configuration is unusable.
func TestRunAuthPriorityRejectsBadArgumentsBeforeConfiguration(t *testing.T) {
	var stderr bytes.Buffer
	streams := Streams{Err: &stderr, ConfigPath: filepath.Join(t.TempDir(), "missing.yaml")}
	err := runAuth(context.Background(), []string{"priority", "claude-a.json", "high"}, streams)
	if err == nil || !strings.Contains(err.Error(), "integer") {
		t.Fatalf("runAuth error = %v, want the integer validation error", err)
	}
}

// TestRunAuthListReportsPriority proves auth list prints a priority line per account.
func TestRunAuthListReportsPriority(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auth")
	if err := os.Mkdir(authDir, 0o700); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	raw, err := json.Marshal(map[string]any{"type": "claude", "email": "main@example.com", "access_token": "t", "priority": 7})
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "claude-main.json"), raw, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(authListConfig(authDir)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var stdout, stderr bytes.Buffer
	streams := Streams{Out: &stdout, Err: &stderr, ConfigPath: cfgPath, Getenv: func(string) string { return "" }}
	if err := runAuth(context.Background(), []string{"list"}, streams); err != nil {
		t.Fatalf("auth list: %v\n%s", err, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "disabled\tfalse\npriority\t7\n") {
		t.Fatalf("auth list output = %q, want a priority line after disabled", output)
	}
}

// authListConfig is the minimal valid shared configuration for a local auth leaf.
func authListConfig(authDir string) string {
	return "auth-dir: " + authDir + `
disable-image-generation: true
request-retry: 0
max-retry-credentials: 1
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
home:
  enabled: false
pprof:
  enable: false
routing:
  session-affinity: false
`
}
