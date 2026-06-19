package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

const (
	// readHeaderTimeout bounds how long a client may take to send request headers, capping the
	// slow-header (Slowloris) exposure without limiting body upload or response streaming.
	readHeaderTimeout = 15 * time.Second

	// idleTimeout closes a keep-alive connection that sends no new request within the window.
	idleTimeout = 120 * time.Second
)

// Server is the gateway's HTTP surface.
type Server struct {
	httpServer *http.Server // httpServer is the underlying stdlib server.
}

// New constructs a Server with its routes registered. The store backs the /v1/messages
// passthrough (project resolution, routing, usage recording); providerName labels the serving
// backend on every recorded usage_event; defaultProject is attributed to requests that omit
// the X-Project header (empty keeps the header required).
// The default provider is resolved once at construction via store.DefaultRoute.
func New(store domain.Store, providerName, defaultProject string) (*Server, error) {
	provider, err := store.DefaultRoute(context.Background())
	if err != nil {
		return nil, fmt.Errorf("resolve default provider:\n%w", err)
	}

	h := newHandler(store, provider, AnthropicWire{}, providerName, defaultProject)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /v1/messages", h.handle)

	// Some OpenAI-wire clients (e.g. Hermes Agent) hardcode POST /chat/completions as their
	// endpoint path even when configured for an Anthropic provider — they still send a native
	// Anthropic Messages body. Alias that path to the same handler so those clients work with no
	// format translation: the body is already Anthropic and auth is a no-op (local, trusted).
	mux.HandleFunc("POST /chat/completions", h.handle)

	return &Server{
		httpServer: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       idleTimeout,
			// No WriteTimeout on purpose: a streaming (SSE) response writes for the full, unbounded
			// duration of a generation. A WriteTimeout would abort long streams mid-flight.
		},
	}, nil
}

// Serve accepts connections on the given listener until the server is shut down.
func (s *Server) Serve(listener net.Listener) error {
	if err := s.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http:\n%w", err)
	}
	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown http server:\n%w", err)
	}
	return nil
}

// handleHealth reports that the gateway is up.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
