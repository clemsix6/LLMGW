package e2e

import (
	"strings"
	"testing"
)

// TestParseLiveSettingsUsesLLMGWNamespace catches a parser that accepts a second top-level owner
// for live-only bindings or loses the deterministic failover fixture declaration.
func TestParseLiveSettingsUsesLLMGWNamespace(t *testing.T) {
	settings, err := parseLiveSettings([]byte(`
llmgw:
  live:
    claude:
      model: claude-alias
      resolved-model: claude-upstream
      provider: claude
    codex:
      model: codex-alias
      resolved-model: codex-upstream
      provider: codex
    failover:
      model: failover-alias
      resolved-model: failover-upstream
      provider: openai-compatibility
      safe-test-accounts: 2
      deterministic-safe-fixture: true
`))
	if err != nil {
		t.Fatalf("parse nested live settings: %v", err)
	}
	want := liveSettings{
		Claude: liveProtocolSettings{
			Model: "claude-alias", ResolvedModel: "claude-upstream", Provider: "claude",
		},
		Codex: liveProtocolSettings{
			Model: "codex-alias", ResolvedModel: "codex-upstream", Provider: "codex",
		},
		Failover: liveFailoverSettings{
			liveProtocolSettings: liveProtocolSettings{
				Model: "failover-alias", ResolvedModel: "failover-upstream",
				Provider: "openai-compatibility",
			},
			SafeTestAccounts: 2,
			Deterministic:    true,
		},
	}
	if settings != want {
		t.Fatalf("nested live settings = %#v, want %#v", settings, want)
	}

	legacy, err := parseLiveSettings([]byte(`
llmgw-live:
  claude-model: must-not-bind
`))
	if err != nil {
		t.Fatalf("parse unowned top-level mapping: %v", err)
	}
	if legacy != (liveSettings{}) {
		t.Fatalf("top-level llmgw-live unexpectedly bound settings: %#v", legacy)
	}
}

// TestValidateLiveProtocolResult catches aggregate-only checks that overlook a wrong normalized
// method, path, model, provider, or opaque upstream-auth field.
func TestValidateLiveProtocolResult(t *testing.T) {
	expectation := liveProtocolExpectation{
		Path: "/v1/responses",
		liveProtocolSettings: liveProtocolSettings{
			Model: "codex-alias", ResolvedModel: "codex-upstream", Provider: "codex",
		},
	}
	valid := liveRequestResult{
		Method:           "POST",
		Path:             "/v1/responses",
		RequestedModel:   "codex-alias",
		State:            "completed",
		AccountingState:  "observed",
		DownstreamStatus: 200,
		Attempts: []liveAttemptResult{{
			Provider:         "codex",
			ResolvedModel:    "codex-upstream",
			RequestedAlias:   "codex-alias",
			UpstreamAuthID:   "opaque-auth-secret",
			UpstreamAuthType: "codex",
			Failed:           false,
		}},
	}
	if err := validateLiveProtocolResult(valid, expectation); err != nil {
		t.Fatalf("valid normalized result: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*liveRequestResult)
	}{
		{name: "method", mutate: func(result *liveRequestResult) { result.Method = "GET" }},
		{name: "path", mutate: func(result *liveRequestResult) { result.Path = "/wrong" }},
		{name: "requested model", mutate: func(result *liveRequestResult) { result.RequestedModel = "wrong" }},
		{name: "request state", mutate: func(result *liveRequestResult) { result.State = "in_flight" }},
		{name: "accounting state", mutate: func(result *liveRequestResult) { result.AccountingState = "pending" }},
		{name: "downstream status", mutate: func(result *liveRequestResult) { result.DownstreamStatus = 500 }},
		{name: "attempt missing", mutate: func(result *liveRequestResult) { result.Attempts = nil }},
		{name: "provider", mutate: func(result *liveRequestResult) { result.Attempts[0].Provider = "wrong" }},
		{name: "resolved model", mutate: func(result *liveRequestResult) { result.Attempts[0].ResolvedModel = "wrong" }},
		{name: "requested alias", mutate: func(result *liveRequestResult) { result.Attempts[0].RequestedAlias = "wrong" }},
		{name: "auth id", mutate: func(result *liveRequestResult) { result.Attempts[0].UpstreamAuthID = "" }},
		{name: "auth type", mutate: func(result *liveRequestResult) { result.Attempts[0].UpstreamAuthType = "" }},
		{name: "failed terminal attempt", mutate: func(result *liveRequestResult) { result.Attempts[0].Failed = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			result.Attempts = append([]liveAttemptResult(nil), valid.Attempts...)
			test.mutate(&result)
			err := validateLiveProtocolResult(result, expectation)
			if err == nil {
				t.Fatal("invalid normalized result passed")
			}
			if strings.Contains(err.Error(), "opaque-auth-secret") {
				t.Fatal("normalized validation error leaked upstream auth ID")
			}
		})
	}
}

// TestValidateLiveFailoverResult catches attempt-count checks that accept retries on one
// credential, reversed ordering, or two terminal failures as account failover.
func TestValidateLiveFailoverResult(t *testing.T) {
	expectation := liveProtocolExpectation{
		Path: "/v1/responses",
		liveProtocolSettings: liveProtocolSettings{
			Model: "failover-alias", ResolvedModel: "failover-upstream",
			Provider: "openai-compatibility",
		},
	}
	valid := liveRequestResult{
		Method:           "POST",
		Path:             "/v1/responses",
		RequestedModel:   "failover-alias",
		State:            "completed",
		AccountingState:  "observed",
		DownstreamStatus: 200,
		Attempts: []liveAttemptResult{
			{
				Provider: "openai-compatibility", ResolvedModel: "failover-upstream",
				RequestedAlias: "failover-alias", UpstreamAuthID: "opaque-first-secret",
				UpstreamAuthType: "openai-compatibility", Failed: true,
			},
			{
				Provider: "openai-compatibility", ResolvedModel: "failover-upstream",
				RequestedAlias: "failover-alias", UpstreamAuthID: "opaque-second-secret",
				UpstreamAuthType: "openai-compatibility", Failed: false,
			},
		},
	}
	if err := validateLiveFailoverResult(valid, expectation); err != nil {
		t.Fatalf("valid failover result: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*liveRequestResult)
	}{
		{name: "one attempt", mutate: func(result *liveRequestResult) { result.Attempts = result.Attempts[:1] }},
		{name: "same credential", mutate: func(result *liveRequestResult) {
			result.Attempts[1].UpstreamAuthID = result.Attempts[0].UpstreamAuthID
		}},
		{name: "success first", mutate: func(result *liveRequestResult) { result.Attempts[0].Failed = false }},
		{name: "failure second", mutate: func(result *liveRequestResult) { result.Attempts[1].Failed = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			result.Attempts = append([]liveAttemptResult(nil), valid.Attempts...)
			test.mutate(&result)
			err := validateLiveFailoverResult(result, expectation)
			if err == nil {
				t.Fatal("invalid failover result passed")
			}
			for _, secret := range []string{"opaque-first-secret", "opaque-second-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("failover validation error leaked upstream auth ID")
				}
			}
		})
	}
}
