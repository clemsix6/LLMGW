package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// Server is the gateway's HTTP surface.
type Server struct {
	httpServer *http.Server // httpServer is the underlying stdlib server.
}

// New constructs a Server with its routes registered. The store backs the /v1/messages
// passthrough (project resolution, routing, usage recording); providerName labels the
// serving backend on every recorded usage_event.
func New(store domain.Store, providerName string) *Server {
	messages := &messagesHandler{store: store, providerName: providerName}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /v1/messages", messages.handle)

	return &Server{
		httpServer: &http.Server{Handler: mux},
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
