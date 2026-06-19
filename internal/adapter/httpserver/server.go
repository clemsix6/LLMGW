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

// Route describes a single LLM gateway endpoint: the HTTP path to register, the upstream
// provider to forward to, the Wire that parses request bodies for this protocol, and the
// provider name label written to every usage_event.
type Route struct {
	Path string // Path is the HTTP path pattern (e.g. "/v1/messages").

	Provider domain.Provider // Provider is the upstream LLM backend for this route.

	Wire Wire // Wire parses request bodies and supplies the default budget tag.

	ProviderName string // ProviderName labels the backend on every recorded usage_event.
}

// Server is the gateway's HTTP surface.
type Server struct {
	httpServer *http.Server // httpServer is the underlying stdlib server.
}

// New constructs a Server with its routes registered. For each Route in routes, a POST handler
// is registered at Route.Path. The store backs project resolution, usage recording, and budget
// enforcement; defaultProject is attributed to requests that omit the X-Project header (empty
// keeps the header required).
func New(store domain.Store, defaultProject string, routes []Route) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)

	for _, rt := range routes {
		h := newHandler(store, rt.Provider, rt.Wire, rt.ProviderName, defaultProject)
		mux.HandleFunc("POST "+rt.Path, h.handle)
	}

	return &Server{
		httpServer: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       idleTimeout,
			// No WriteTimeout on purpose: a streaming (SSE) response writes for the full, unbounded
			// duration of a generation. A WriteTimeout would abort long streams mid-flight.
		},
	}
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
