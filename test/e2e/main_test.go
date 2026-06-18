package e2e

// TestMain is the gated suite's coordinator. The Claude Max refresh token is single-use: every
// OAuth refresh rotates it, so if each gated test refreshed independently only the first would
// succeed and the rest would fail with invalid_grant. Instead this runs ONE refresh up front,
// caches the resulting access token (valid ~8h) plus the rotated refresh token, and every gated
// test seeds its store with that access token (already fresh) so no per-test refresh occurs. The
// rotated refresh token is written back to .env exactly once, after the suite, so the next run
// (re-source .env first) starts from a live token.
//
//	set -a; . ./.env; set +a; go test ./test/e2e -v

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/clemsix6/LLMGW/internal/adapter/provider/claudemax"
	"github.com/clemsix6/LLMGW/internal/domain"
)

// shared holds the single OAuth refresh performed once per `go test` run, so the whole gated
// suite runs in one invocation without exhausting the single-use refresh token.
var shared struct {
	enabled bool // enabled reports whether LLMGW_TEST_REFRESH_TOKEN was set (suite should run).

	ready bool // ready reports whether the up-front refresh succeeded.

	token domain.Token // token is the shared access token (+ rotated refresh) for gated tests.

	refreshErr error // refreshErr is the up-front refresh failure, surfaced by gated tests.
}

// TestMain refreshes once, runs the suite, then writes the rotated refresh token back to .env.
func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite performs the one-shot refresh (when credentials are present), runs the tests, and
// persists the rotated refresh token. It never aborts the run: a missing or dead seed token is
// surfaced per-test (skip vs fail) rather than panicking the whole binary.
func runSuite(m *testing.M) int {
	seed := os.Getenv("LLMGW_TEST_REFRESH_TOKEN")
	if seed == "" {
		return m.Run()
	}
	shared.enabled = true

	token, err := claudemax.Refresh(context.Background(), seed)
	if err != nil {
		shared.refreshErr = err
		log.Printf("e2e: up-front OAuth refresh failed: %v", err)
		return m.Run()
	}
	shared.ready = true
	shared.token = token

	code := m.Run()

	persistRotatedToken(token.RefreshToken)
	return code
}

// requireSharedToken gates a real-API test: it skips when no credentials are configured, and
// fails loudly when credentials are present but the up-front refresh failed (a dead seed token
// must not be silently skipped — the orchestrator re-seeds). Otherwise it returns the shared
// access token the test seeds into its store.
func requireSharedToken(t *testing.T) domain.Token {
	t.Helper()

	if !shared.enabled {
		t.Skip("LLMGW_TEST_REFRESH_TOKEN not set; skipping real-API test")
	}
	if !shared.ready {
		t.Fatalf("up-front OAuth refresh failed (seed token may be dead, re-seed required): %v", shared.refreshErr)
	}
	return shared.token
}

// persistRotatedToken rewrites the refresh-token lines of the repo .env in place so a re-run
// (after re-sourcing .env) starts from the rotated, still-live token. A missing or empty token,
// or an I/O failure, is logged and ignored — it must never fail the suite.
func persistRotatedToken(newToken string) {
	if newToken == "" {
		log.Print("e2e: skip token write-back: rotated refresh token is empty")
		return
	}

	path := envFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("e2e: skip token write-back: read %s: %v", path, err)
		return
	}

	updated := rewriteTokenLines(string(data), newToken)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		log.Printf("e2e: skip token write-back: write %s: %v", path, err)
		return
	}
	log.Printf("e2e: rotated refresh token written back to %s", path)
}

// rewriteTokenLines replaces the seed/test refresh-token assignments with newToken, leaving every
// other .env line untouched.
func rewriteTokenLines(content, newToken string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "LLMGW_TEST_REFRESH_TOKEN="):
			lines[i] = "LLMGW_TEST_REFRESH_TOKEN=" + newToken
		case strings.HasPrefix(line, "LLMGW_OAUTH_REFRESH_TOKENS="):
			lines[i] = "LLMGW_OAUTH_REFRESH_TOKENS=" + testAccount + "=" + newToken
		}
	}
	return strings.Join(lines, "\n")
}

// envFilePath returns the repo-root .env path, resolved relative to this test file so it is
// independent of the working directory.
func envFilePath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", ".env")
}
