package cliproxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readAuthJSON decodes one auth file of the directory.
func readAuthJSON(t *testing.T, dir, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode auth file: %v", err)
	}
	return decoded
}

func TestSetAuthPrioritySetsAndReportsValue(t *testing.T) {
	dir := t.TempDir()
	name := writeClaudeAuthFile(t, dir, "main", nil)

	info, err := SetAuthPriority(context.Background(), dir, name, 5)
	if err != nil {
		t.Fatalf("set priority: %v", err)
	}
	if info.ID != name || info.Priority != 5 {
		t.Fatalf("info = %+v, want id %q priority 5", info, name)
	}
	if got := readAuthJSON(t, dir, name)["priority"]; got != float64(5) {
		t.Fatalf("stored priority = %v, want 5", got)
	}
	listed, err := ListAuth(context.Background(), dir)
	if err != nil || len(listed) != 1 || listed[0].Priority != 5 {
		t.Fatalf("list = %+v, %v, want priority 5", listed, err)
	}
}

func TestSetAuthPriorityZeroRemovesKey(t *testing.T) {
	dir := t.TempDir()
	name := writeClaudeAuthFile(t, dir, "main", map[string]any{"priority": 9})

	info, err := SetAuthPriority(context.Background(), dir, name, 0)
	if err != nil {
		t.Fatalf("set priority: %v", err)
	}
	if info.Priority != 0 {
		t.Fatalf("priority = %d, want 0", info.Priority)
	}
	if _, present := readAuthJSON(t, dir, name)["priority"]; present {
		t.Fatalf("priority key still present after reset")
	}
}

func TestSetAuthPriorityUnknownIDFailsWithoutLeakingSecrets(t *testing.T) {
	dir := t.TempDir()
	writeClaudeAuthFile(t, dir, "main", nil)

	_, err := SetAuthPriority(context.Background(), dir, "missing.json", 3)
	if err == nil {
		t.Fatal("expected an error for an unknown id")
	}
	if got := err.Error(); containsAny(got, "dummy-main", "main@") {
		t.Fatalf("error leaks credential data: %q", got)
	}
}

func TestSetAuthPriorityPreservesOtherKeysAndMode(t *testing.T) {
	dir := t.TempDir()
	name := writeClaudeAuthFile(t, dir, "main", map[string]any{
		"refresh_token": "r-1", "disabled": false, "proxy_url": "http://proxy.invalid", "custom": map[string]any{"a": 1.0},
	})
	before := readAuthJSON(t, dir, name)

	if _, err := SetAuthPriority(context.Background(), dir, name, 4); err != nil {
		t.Fatalf("set priority: %v", err)
	}
	after := readAuthJSON(t, dir, name)
	for key, want := range before {
		if got, _ := json.Marshal(after[key]); string(got) != mustMarshal(t, want) {
			t.Fatalf("key %q = %s, want %s", key, got, mustMarshal(t, want))
		}
	}
	if len(after) != len(before)+1 {
		t.Fatalf("keys after = %v, want the previous keys plus priority", after)
	}
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v, want 0600", info, err)
	}
}

func TestListAuthReportsZeroForMissingOrInvalidPriority(t *testing.T) {
	dir := t.TempDir()
	writeClaudeAuthFile(t, dir, "none", nil)
	writeClaudeAuthFile(t, dir, "text", map[string]any{"priority": "abc"})
	writeClaudeAuthFile(t, dir, "bool", map[string]any{"priority": true})
	writeClaudeAuthFile(t, dir, "numeric-string", map[string]any{"priority": " 12 "})

	listed, err := ListAuth(context.Background(), dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := map[string]int{
		"claude-none.json": 0, "claude-text.json": 0, "claude-bool.json": 0, "claude-numeric-string.json": 12,
	}
	for _, info := range listed {
		if info.Priority != want[info.ID] {
			t.Fatalf("%s priority = %d, want %d", info.ID, info.Priority, want[info.ID])
		}
	}
}

// containsAny reports whether text contains any non-empty needle.
func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if len(needle) > 0 && strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// mustMarshal returns the JSON encoding of value, failing the test when it cannot be encoded.
func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestSetAuthPriorityOnDisabledCredentialKeepsItDisabled(t *testing.T) {
	dir := t.TempDir()
	name := writeClaudeAuthFile(t, dir, "off", map[string]any{"disabled": true})

	info, err := SetAuthPriority(context.Background(), dir, name, 6)
	if err != nil {
		t.Fatalf("set priority: %v", err)
	}
	if !info.Disabled || info.Priority != 6 {
		t.Fatalf("info = %+v, want disabled with priority 6", info)
	}
	stored := readAuthJSON(t, dir, name)
	if stored["priority"] != float64(6) || stored["disabled"] != true {
		t.Fatalf("stored file = %v, want priority 6 and disabled kept", stored)
	}
}
