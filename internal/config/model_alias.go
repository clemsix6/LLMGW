package config

import (
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// claudeOAuthChannel is the oauth-model-alias channel that covers Claude OAuth
// credentials.
const claudeOAuthChannel = "claude"

// claudeShortAliases lists the Claude models the pinned SDK catalog registers
// only under a dated id, each paired with the undated alias Anthropic also
// serves. The SDK resolves models by exact registry key, so a request for an
// undated id is refused before any executor runs unless the alias is
// registered. Entries whose Name the catalog no longer carries are inert.
var claudeShortAliases = []sdkconfig.OAuthModelAlias{
	{Name: "claude-haiku-4-5-20251001", Alias: "claude-haiku-4-5", Fork: true},
	{Name: "claude-3-5-haiku-20241022", Alias: "claude-3-5-haiku", Fork: true},
	{Name: "claude-sonnet-4-5-20250929", Alias: "claude-sonnet-4-5", Fork: true},
	{Name: "claude-opus-4-5-20251101", Alias: "claude-opus-4-5", Fork: true},
	{Name: "claude-opus-4-1-20250805", Alias: "claude-opus-4-1", Fork: true},
	{Name: "claude-opus-4-20250514", Alias: "claude-opus-4", Fork: true},
	{Name: "claude-sonnet-4-20250514", Alias: "claude-sonnet-4", Fork: true},
	{Name: "claude-3-7-sonnet-20250219", Alias: "claude-3-7-sonnet", Fork: true},
}

// ensureClaudeShortAliases backfills the Claude OAuth channel with the alias
// entries the catalog lacks, so every deployment routes the undated ids
// without operator configuration. An operator entry that already claims an
// alias name wins, and every injected entry forks so the dated id keeps
// routing beside its alias.
func ensureClaudeShortAliases(proxy *sdkconfig.Config) {
	entries := proxy.OAuthModelAlias[claudeOAuthChannel]
	claimed := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		claimed[entry.Alias] = struct{}{}
	}

	for _, candidate := range claudeShortAliases {
		if _, taken := claimed[candidate.Alias]; taken {
			continue
		}
		entries = append(entries, candidate)
	}

	if proxy.OAuthModelAlias == nil {
		proxy.OAuthModelAlias = make(map[string][]sdkconfig.OAuthModelAlias, 1)
	}
	proxy.OAuthModelAlias[claudeOAuthChannel] = entries
}
