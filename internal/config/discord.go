package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// DiscordWebhookURL resolves the configured Discord webhook URL without
// retaining its value in Config. An empty environment variable name, or a
// resolved value that is empty after trimming whitespace, means alerting is
// disabled — not an error. A non-empty value must parse as an absolute http
// or https URL with a non-empty host; the resolver does not judge the host
// itself, so a self-hosted relay is accepted like a Discord webhook.
func (c Config) DiscordWebhookURL(getenv func(string) string) (string, bool, error) {
	envName := c.LLMGW.DiscordWebhookURLEnv
	if envName == "" {
		return "", false, nil
	}
	value := strings.TrimSpace(getenv(envName))
	if value == "" {
		return "", false, nil
	}
	if err := validateWebhookURL(value); err != nil {
		return "", false, fmt.Errorf("resolve Discord webhook URL from %s:\n%w", envName, err)
	}
	return value, true, nil
}

// validateWebhookURL accepts only an absolute http or https URL that carries
// a host, without inspecting which host it is.
func validateWebhookURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return errors.New("value is not a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("value must be an absolute http or https URL")
	}
	if parsed.Host == "" {
		return errors.New("value must include a host")
	}
	return nil
}
