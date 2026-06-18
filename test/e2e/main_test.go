package e2e

// TestMain is the gated suite's coordinator. It bootstraps a Claude Code OAuth token set ONCE from
// the configured claude.ai session key (LLMGW_TEST_SESSION_KEY) and shares the resulting access
// token across every gated test, so the whole suite runs in a single invocation. Unlike an OAuth
// refresh token, a session key is durable and does not rotate on use, so there is nothing to write
// back — the same secret stays valid across runs.
//
//	set -a; . ./.env; set +a; go test ./test/e2e -v

import (
	"context"
	"log"
	"os"
	"testing"

	"github.com/clemsix6/LLMGW/internal/adapter/provider/claudemax"
	"github.com/clemsix6/LLMGW/internal/domain"
)

// shared holds the single OAuth bootstrap performed once per `go test` run, so the whole gated
// suite runs in one invocation off a single durable session key.
var shared struct {
	enabled bool // enabled reports whether LLMGW_TEST_SESSION_KEY was set (suite should run).

	ready bool // ready reports whether the up-front bootstrap succeeded.

	token domain.Token // token is the shared access token (+ session key) for gated tests.

	bootstrapErr error // bootstrapErr is the up-front bootstrap failure, surfaced by gated tests.
}

// TestMain bootstraps once from the session key, then runs the suite.
func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite performs the one-shot bootstrap (when a session key is present) and runs the tests. It
// never aborts the run: a missing or dead session key is surfaced per-test (skip vs fail) rather
// than panicking the whole binary.
func runSuite(m *testing.M) int {
	seed := os.Getenv("LLMGW_TEST_SESSION_KEY")
	if seed == "" {
		return m.Run()
	}
	shared.enabled = true

	token, err := claudemax.Bootstrap(context.Background(), seed, testClaudeCodeVersion)
	if err != nil {
		shared.bootstrapErr = err
		log.Printf("e2e: up-front bootstrap from session key failed: %v", err)
		return m.Run()
	}
	shared.ready = true
	shared.token = token

	return m.Run()
}

// requireSharedToken gates a real-API test: it skips when no session key is configured, and fails
// loudly when one is present but the up-front bootstrap failed (a dead session key must not be
// silently skipped — the operator re-seeds). Otherwise it returns the shared access token.
func requireSharedToken(t *testing.T) domain.Token {
	t.Helper()

	if !shared.enabled {
		t.Skip("LLMGW_TEST_SESSION_KEY not set; skipping real-API test")
	}
	if !shared.ready {
		t.Fatalf("up-front bootstrap failed (session key may be dead, re-seed required): %v", shared.bootstrapErr)
	}
	return shared.token
}
