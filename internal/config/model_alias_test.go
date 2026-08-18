package config

import (
	"testing"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestLoadBackfillsClaudeShortAliases verifies every undated Claude alias is
// registered on the OAuth channel when the operator configured none.
func TestLoadBackfillsClaudeShortAliases(t *testing.T) {
	cfg, err := Load(writeConfig(t, secureConfig), mapEnv(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	entries := cfg.Proxy.OAuthModelAlias[claudeOAuthChannel]
	if len(entries) != len(claudeShortAliases) {
		t.Fatalf("claude alias entries = %d, want %d", len(entries), len(claudeShortAliases))
	}
	haiku, ok := findAlias(entries, "claude-haiku-4-5")
	if !ok {
		t.Fatal("claude-haiku-4-5 alias missing")
	}
	if haiku.Name != "claude-haiku-4-5-20251001" || !haiku.Fork {
		t.Fatalf("claude-haiku-4-5 entry = %+v, want dated forked mapping", haiku)
	}
}

// TestLoadKeepsOperatorAliasEntries verifies an operator entry claiming an
// alias name is preserved verbatim while the remaining aliases still backfill.
func TestLoadKeepsOperatorAliasEntries(t *testing.T) {
	path := writeConfig(t, secureConfig+`
oauth-model-alias:
  claude:
    - name: claude-opus-4-8
      alias: claude-haiku-4-5
`)

	cfg, err := Load(path, mapEnv(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	entries := cfg.Proxy.OAuthModelAlias[claudeOAuthChannel]
	if len(entries) != len(claudeShortAliases) {
		t.Fatalf("claude alias entries = %d, want %d", len(entries), len(claudeShortAliases))
	}
	haiku, ok := findAlias(entries, "claude-haiku-4-5")
	if !ok {
		t.Fatal("claude-haiku-4-5 alias missing")
	}
	if haiku.Name != "claude-opus-4-8" || haiku.Fork {
		t.Fatalf("claude-haiku-4-5 entry = %+v, want the operator mapping kept", haiku)
	}
	if _, ok := findAlias(entries, "claude-3-5-haiku"); !ok {
		t.Fatal("claude-3-5-haiku alias missing beside the operator entry")
	}
}

// TestLoadBackfillLeavesOtherChannelsUntouched verifies the backfill never
// writes outside the Claude OAuth channel.
func TestLoadBackfillLeavesOtherChannelsUntouched(t *testing.T) {
	path := writeConfig(t, secureConfig+`
oauth-model-alias:
  gemini:
    - name: gemini-dated-model
      alias: gemini-short-model
`)

	cfg, err := Load(path, mapEnv(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	gemini := cfg.Proxy.OAuthModelAlias["gemini"]
	if len(gemini) != 1 || gemini[0].Alias != "gemini-short-model" {
		t.Fatalf("gemini channel entries = %+v, want the operator entry alone", gemini)
	}
}

// findAlias returns the first entry carrying the alias name.
func findAlias(
	entries []sdkconfig.OAuthModelAlias,
	alias string,
) (sdkconfig.OAuthModelAlias, bool) {
	for _, entry := range entries {
		if entry.Alias == alias {
			return entry, true
		}
	}
	return sdkconfig.OAuthModelAlias{}, false
}
