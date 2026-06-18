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

	return Config{
		Listen:            valueOr(os.Getenv("LLMGW_LISTEN"), defaultListen),
		PostgresDSN:       dsn,
		SessionKeys:       keys,
		ClaudeCodeVersion: valueOr(os.Getenv("LLMGW_CLAUDE_CODE_VERSION"), defaultClaudeCodeVersion),
		DefaultProject:    os.Getenv("LLMGW_DEFAULT_PROJECT"),
	}, nil
}

// valueOr returns value when it is non-empty, otherwise fallback.
func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
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
