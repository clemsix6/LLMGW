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

	RefreshTokens []RefreshToken // RefreshTokens seeds the per-account OAuth refresh tokens.

	ClaudeCodeVersion string // ClaudeCodeVersion is the spoofed Claude Code client version.
}

// RefreshToken is a single seed OAuth refresh token bound to an account label.
type RefreshToken struct {
	Label string // Label identifies the account (e.g. "acct1").

	Token string // Token is the seed OAuth refresh token for the account.
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

	tokens, err := parseRefreshTokens(os.Getenv("LLMGW_OAUTH_REFRESH_TOKENS"))
	if err != nil {
		return Config{}, fmt.Errorf("load config:\n%w", err)
	}

	return Config{
		Listen:            valueOr(os.Getenv("LLMGW_LISTEN"), defaultListen),
		PostgresDSN:       dsn,
		RefreshTokens:     tokens,
		ClaudeCodeVersion: valueOr(os.Getenv("LLMGW_CLAUDE_CODE_VERSION"), defaultClaudeCodeVersion),
	}, nil
}

// valueOr returns value when it is non-empty, otherwise fallback.
func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// parseRefreshTokens parses a comma-separated list of "label=token" pairs.
// An empty input yields no tokens; a malformed pair returns an error.
func parseRefreshTokens(raw string) ([]RefreshToken, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var tokens []RefreshToken
	for _, pair := range strings.Split(raw, ",") {
		token, err := parsePair(strings.TrimSpace(pair))
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, nil
}

// parsePair splits a single "label=token" pair into a RefreshToken.
func parsePair(pair string) (RefreshToken, error) {
	label, token, ok := strings.Cut(pair, "=")
	label, token = strings.TrimSpace(label), strings.TrimSpace(token)

	if !ok || label == "" || token == "" {
		return RefreshToken{}, fmt.Errorf("invalid refresh token pair %q (want label=token)", pair)
	}
	return RefreshToken{Label: label, Token: token}, nil
}
