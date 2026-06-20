package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Config holds the gateway's runtime configuration, sourced from environment variables.
type Config struct {
	Listen string // Listen is the host:port the HTTP server binds to.

	PostgresDSN string // PostgresDSN is the connection string for the state database.

	SessionKeys []SessionKey // SessionKeys seeds the per-account claude.ai session keys that bootstrap OAuth tokens.

	ClaudeCodeVersion string // ClaudeCodeVersion is the spoofed Claude Code client version.

	DefaultProject string // DefaultProject is attributed to requests that omit the X-Project header; empty keeps X-Project required.

	CodexAccounts []CodexAccount // CodexAccounts seeds the per-account Codex refresh tokens and account identifiers.

	CodexVersion string // CodexVersion is the spoofed Codex client version sent in provider request headers.

	CodexWebSearch bool // CodexWebSearch advertises OpenAI's native web_search built-in tool on every Codex request when true.
}

// CodexAccount is a single seed Codex account bound to a label.
type CodexAccount struct {
	Label string // Label identifies the account (e.g. "main").

	RefreshToken string // RefreshToken is the durable OAuth refresh token for the account.

	AccountID string // AccountID is the ChatGPT account identifier (e.g. "acct_…").
}

// SessionKey is a single seed claude.ai session key bound to an account label.
type SessionKey struct {
	Label string // Label identifies the account (e.g. "acct1").

	Key string // Key is the durable claude.ai session key (sk-ant-sid…) for the account.
}

const (
	// defaultListen is the local address the server binds to when LLMGW_LISTEN is unset.
	defaultListen = "127.0.0.1:8088"

	// defaultClaudeCodeVersion is the spoofed client version when LLMGW_CLAUDE_CODE_VERSION is unset.
	defaultClaudeCodeVersion = "2.1.181"

	// defaultCodexVersion is the spoofed Codex client version when LLMGW_CODEX_VERSION is unset.
	defaultCodexVersion = "1.0.0"
)

// errMissingDSN signals that the required Postgres DSN environment variable is unset.
var errMissingDSN = errors.New("LLMGW_POSTGRES_DSN is required")

// Load reads the configuration from the environment, applying defaults and
// validating that required variables are present.
func Load() (Config, error) {
	dsn := os.Getenv("LLMGW_POSTGRES_DSN")
	if dsn == "" {
		return Config{}, fmt.Errorf("load config:\n%w", errMissingDSN)
	}

	keys, err := parseSessionKeys(os.Getenv("LLMGW_SESSION_KEYS"))
	if err != nil {
		return Config{}, fmt.Errorf("load config:\n%w", err)
	}

	codexAccounts, err := parseCodexAccounts(os.Getenv("LLMGW_CODEX_ACCOUNTS"))
	if err != nil {
		return Config{}, fmt.Errorf("load config:\n%w", err)
	}

	return Config{
		Listen:            valueOr(os.Getenv("LLMGW_LISTEN"), defaultListen),
		PostgresDSN:       dsn,
		SessionKeys:       keys,
		ClaudeCodeVersion: valueOr(os.Getenv("LLMGW_CLAUDE_CODE_VERSION"), defaultClaudeCodeVersion),
		DefaultProject:    os.Getenv("LLMGW_DEFAULT_PROJECT"),
		CodexAccounts:     codexAccounts,
		CodexVersion:      valueOr(os.Getenv("LLMGW_CODEX_VERSION"), defaultCodexVersion),
		CodexWebSearch:    boolEnv("LLMGW_CODEX_WEB_SEARCH"),
	}, nil
}

// valueOr returns value when it is non-empty, otherwise fallback.
func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// boolEnv reports whether the named environment variable is set to a truthy value ("true" or "1").
func boolEnv(name string) bool {
	switch strings.TrimSpace(os.Getenv(name)) {
	case "true", "1":
		return true
	default:
		return false
	}
}

// parseSessionKeys parses a comma-separated list of "label=key" pairs.
// An empty input yields no keys; a malformed pair returns an error.
func parseSessionKeys(raw string) ([]SessionKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var keys []SessionKey
	for _, pair := range strings.Split(raw, ",") {
		key, err := parsePair(strings.TrimSpace(pair))
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// parsePair splits a single "label=key" pair into a SessionKey.
func parsePair(pair string) (SessionKey, error) {
	label, key, ok := strings.Cut(pair, "=")
	label, key = strings.TrimSpace(label), strings.TrimSpace(key)

	if !ok || label == "" || key == "" {
		return SessionKey{}, fmt.Errorf("invalid session key pair %q (want label=key)", pair)
	}
	return SessionKey{Label: label, Key: key}, nil
}

// parseCodexAccounts parses a comma-separated list of "label:refresh_token:account_id" triplets.
// An empty input yields no accounts; a malformed triplet returns an error.
func parseCodexAccounts(raw string) ([]CodexAccount, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var accounts []CodexAccount
	for _, triplet := range strings.Split(raw, ",") {
		account, err := parseCodexTriplet(strings.TrimSpace(triplet))
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

// parseCodexTriplet splits a single "label:refresh_token:account_id" triplet into a CodexAccount.
// It rejects any entry that does not contain exactly three colon-separated fields, so a token
// or account_id that contains a colon (which would make the split ambiguous) is caught early.
func parseCodexTriplet(triplet string) (CodexAccount, error) {
	parts := strings.Split(triplet, ":")
	if len(parts) != 3 {
		return CodexAccount{}, fmt.Errorf("invalid codex account triplet %q (want label:refresh_token:account_id)", triplet)
	}

	label := strings.TrimSpace(parts[0])
	refreshToken := strings.TrimSpace(parts[1])
	accountID := strings.TrimSpace(parts[2])

	if label == "" || refreshToken == "" || accountID == "" {
		return CodexAccount{}, fmt.Errorf("invalid codex account triplet %q (want label:refresh_token:account_id)", triplet)
	}

	return CodexAccount{Label: label, RefreshToken: refreshToken, AccountID: accountID}, nil
}
