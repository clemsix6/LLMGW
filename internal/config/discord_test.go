package config

import (
	"strings"
	"testing"
)

// TestDiscordWebhookURLDisabled covers every input that must disable
// alerting without an error: an unset or empty environment variable name,
// and an unset, empty or whitespace-only resolved value.
func TestDiscordWebhookURLDisabled(t *testing.T) {
	tests := []struct {
		name   string
		envVar string
		env    map[string]string
	}{
		{name: "environment variable name unset"},
		{name: "environment variable name empty", envVar: ""},
		{name: "value unset", envVar: "LLMGW_DISCORD_WEBHOOK_URL"},
		{name: "value empty", envVar: "LLMGW_DISCORD_WEBHOOK_URL", env: map[string]string{"LLMGW_DISCORD_WEBHOOK_URL": ""}},
		{name: "value whitespace only", envVar: "LLMGW_DISCORD_WEBHOOK_URL", env: map[string]string{"LLMGW_DISCORD_WEBHOOK_URL": "   "}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{LLMGW: LLMGW{DiscordWebhookURLEnv: test.envVar}}

			url, enabled, err := cfg.DiscordWebhookURL(mapEnv(test.env))

			if err != nil {
				t.Fatalf("DiscordWebhookURL() error = %v, want nil", err)
			}
			if enabled {
				t.Fatal("DiscordWebhookURL() enabled = true, want false")
			}
			if url != "" {
				t.Fatalf("DiscordWebhookURL() url = %q, want empty", url)
			}
		})
	}
}

// TestDiscordWebhookURLAccepted covers absolute http and https URLs with a
// host. The resolver never judges the host: a non-Discord relay is accepted
// exactly like a Discord webhook.
func TestDiscordWebhookURLAccepted(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "discord webhook shape", value: "https://discord.com/api/webhooks/000000000000000000/placeholder-token"},
		{name: "non-discord https host", value: "https://relay.example.com/hook"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{LLMGW: LLMGW{DiscordWebhookURLEnv: "LLMGW_DISCORD_WEBHOOK_URL"}}
			env := map[string]string{"LLMGW_DISCORD_WEBHOOK_URL": test.value}

			url, enabled, err := cfg.DiscordWebhookURL(mapEnv(env))

			if err != nil {
				t.Fatalf("DiscordWebhookURL() error = %v, want nil", err)
			}
			if !enabled {
				t.Fatal("DiscordWebhookURL() enabled = false, want true")
			}
			if url != test.value {
				t.Fatalf("DiscordWebhookURL() url = %q, want %q", url, test.value)
			}
		})
	}
}

// TestDiscordWebhookURLRejected covers malformed values: not a URL at all,
// a non-http(s) scheme, and an http(s) scheme with no host.
func TestDiscordWebhookURLRejected(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "not a url", value: "not-a-url"},
		{name: "non-http scheme", value: "ftp://host/x"},
		{name: "missing host", value: "https:///nohost"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{LLMGW: LLMGW{DiscordWebhookURLEnv: "LLMGW_DISCORD_WEBHOOK_URL"}}
			env := map[string]string{"LLMGW_DISCORD_WEBHOOK_URL": test.value}

			url, enabled, err := cfg.DiscordWebhookURL(mapEnv(env))

			if err == nil {
				t.Fatal("DiscordWebhookURL() error = nil, want error")
			}
			if !strings.Contains(err.Error(), "LLMGW_DISCORD_WEBHOOK_URL") {
				t.Fatalf("DiscordWebhookURL() error = %v, want it to name the environment variable", err)
			}
			if strings.Contains(err.Error(), test.value) {
				t.Fatalf("DiscordWebhookURL() error = %v, must not contain the malformed value", err)
			}
			if enabled {
				t.Fatal("DiscordWebhookURL() enabled = true, want false")
			}
			if url != "" {
				t.Fatalf("DiscordWebhookURL() url = %q, want empty", url)
			}
		})
	}
}

// TestApplyLLMGWDefaultsSetsDiscordWebhookURLEnv verifies the default
// environment variable name is applied only when unset or blank.
func TestApplyLLMGWDefaultsSetsDiscordWebhookURLEnv(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "unset", in: "", want: defaultDiscordWebhookURLEnv},
		{name: "whitespace", in: "   ", want: defaultDiscordWebhookURLEnv},
		{name: "overridden", in: "CUSTOM_DISCORD_WEBHOOK", want: "CUSTOM_DISCORD_WEBHOOK"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := LLMGW{DiscordWebhookURLEnv: test.in}
			applyLLMGWDefaults(&settings)

			if settings.DiscordWebhookURLEnv != test.want {
				t.Fatalf("DiscordWebhookURLEnv = %q, want %q", settings.DiscordWebhookURLEnv, test.want)
			}
		})
	}
}
