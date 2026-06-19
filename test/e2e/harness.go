package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/clemsix6/LLMGW/internal/adapter/httpserver"
	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/adapter/provider/claudemax"
	"github.com/clemsix6/LLMGW/internal/adapter/provider/codex"
	"github.com/clemsix6/LLMGW/internal/domain"
)

// Harness is a booted gateway backed by an ephemeral Postgres container.
type Harness struct {
	BaseURL string // BaseURL is the gateway's HTTP base URL.

	DSN string // DSN is the connection string of the ephemeral database.

	store *postgres.Store // store is the gateway's state store (migrations applied).

	server *httpserver.Server // server is the running HTTP surface.

	listener net.Listener // listener is the random-port socket the server accepts on.

	container *tcpostgres.PostgresContainer // container is the ephemeral Postgres instance.
}

// Start launches Postgres, applies migrations, and boots the gateway on a random port with
// no LLM routes registered (only GET /health). Call SeedClaudeMax to register /v1/messages.
func Start(ctx context.Context) (*Harness, error) {
	container, dsn, err := startPostgres(ctx)
	if err != nil {
		return nil, err
	}

	store, err := postgres.New(ctx, dsn)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("open store:\n%w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		store.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("listen:\n%w", err)
	}

	server := httpserver.New(store, "", nil)
	go func() { _ = server.Serve(listener) }()

	return &Harness{
		BaseURL:   "http://" + listener.Addr().String(),
		DSN:       dsn,
		store:     store,
		server:    server,
		listener:  listener,
		container: container,
	}, nil
}

// startPostgres runs an ephemeral Postgres container and returns its DSN.
func startPostgres(ctx context.Context) (*tcpostgres.PostgresContainer, string, error) {
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("llmgw"),
		tcpostgres.WithUsername("llmgw"),
		tcpostgres.WithPassword("llmgw"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(time.Minute),
		),
	)
	if err != nil {
		return nil, "", fmt.Errorf("start postgres container:\n%w", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, "", fmt.Errorf("postgres connection string:\n%w", err)
	}
	return container, dsn, nil
}

// SeedClaudeMax persists the account's OAuth token and rebuilds the server with the Claude Max
// route wired to /v1/messages. It replaces the no-route server started by Start, so the gateway
// can forward to the real Anthropic API. The token carries the shared access token (refreshed
// once per run by the coordinator), so the provider serves it directly without triggering a
// per-test refresh of the single-use refresh token.
func (h *Harness) SeedClaudeMax(ctx context.Context, account string, token domain.Token, version string) error {
	if err := h.store.SaveToken(ctx, postgres.DefaultProviderName, account, token); err != nil {
		return fmt.Errorf("seed token:\n%w", err)
	}

	// Shut down the no-route server; this closes the old listener.
	if err := h.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown old server:\n%w", err)
	}

	// Open a new listener on a fresh random port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("rebind listener:\n%w", err)
	}
	h.listener = listener
	h.BaseURL = "http://" + listener.Addr().String()

	claude := claudemax.New(h.store, version)
	routes := []httpserver.Route{{
		Path:         "/v1/messages",
		Provider:     claude,
		Wire:         httpserver.AnthropicWire{},
		ProviderName: postgres.DefaultProviderName,
	}}
	h.server = httpserver.New(h.store, "", routes)
	go func() { _ = h.server.Serve(h.listener) }()
	return nil
}

// SeedCodex persists the Codex account credentials and rebuilds the server with the
// /v1/chat/completions route wired to the codex provider. It replaces the no-route (or
// previous) server started by Start, so the gateway can forward to the real Codex backend.
func (h *Harness) SeedCodex(ctx context.Context, account, refreshToken, accountID, version string) error {
	if err := h.store.SeedCodexAccount(ctx, account, refreshToken, accountID); err != nil {
		return fmt.Errorf("seed codex account:\n%w", err)
	}

	if err := h.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown old server:\n%w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("rebind listener:\n%w", err)
	}
	h.listener = listener
	h.BaseURL = "http://" + listener.Addr().String()

	codexProv := codex.New(h.store, version)
	routes := []httpserver.Route{{
		Path:         "/v1/chat/completions",
		Provider:     codexProv,
		Wire:         httpserver.OpenAIWire{},
		ProviderName: postgres.CodexProviderName,
	}}
	h.server = httpserver.New(h.store, "", routes)
	go func() { _ = h.server.Serve(h.listener) }()
	return nil
}

// Post issues a single POST against the gateway with the given body and headers. Retries are
// the caller's responsibility (transient upstream errors are not the gateway's own assertions).
func (h *Harness) Post(ctx context.Context, path string, body []byte, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request:\n%w", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s:\n%w", path, err)
	}
	return resp, nil
}

// Stop shuts the gateway down and terminates the Postgres container.
func (h *Harness) Stop(ctx context.Context) {
	_ = h.server.Shutdown(ctx)
	h.store.Close()
	_ = h.container.Terminate(ctx)
}

// Get issues a GET against the gateway, retrying transient connection errors during boot.
func (h *Harness) Get(ctx context.Context, path string) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt < 10; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+path, nil)
		if err != nil {
			return nil, fmt.Errorf("build request:\n%w", err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			return resp, nil
		}

		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("GET %s failed after retries:\n%w", path, lastErr)
}
