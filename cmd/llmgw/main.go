package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/httpserver"
	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/adapter/provider/claudemax"
	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain"
)

const (
	// usageRetention bounds usage_event so the windowed-aggregate hot path stays cheap (design §6).
	// The longest budget window is a day, so 35 days leaves a wide safety margin.
	usageRetention = 35 * 24 * time.Hour

	// pruneInterval is how often the background retention sweep runs.
	pruneInterval = time.Hour

	// shutdownTimeout bounds graceful drain of in-flight requests before the server forces close.
	shutdownTimeout = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("llmgw: %v", err)
	}
}

// run loads configuration, opens the store (applying migrations), wires the provider, and serves
// HTTP until a shutdown signal arrives.
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	store, err := postgres.New(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.Ping(ctx); err != nil {
		return err
	}

	if err := seedTokens(ctx, store, cfg.RefreshTokens); err != nil {
		return err
	}

	provider := claudemax.New(store, cfg.ClaudeCodeVersion)
	store.SetDefaultProvider(provider)

	return serve(ctx, cfg, store)
}

// serve binds the listener, starts the background pruner, runs the HTTP server, and on a shutdown
// signal drains connections gracefully. It always stops the pruner and waits for it to finish
// before returning, so no prune query races the deferred pool close.
func serve(ctx context.Context, cfg config.Config, store *postgres.Store) error {
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s:\n%w", cfg.Listen, err)
	}

	pruneCtx, stopPruner := context.WithCancel(context.Background())
	prunerDone := startPruner(pruneCtx, store)

	server := httpserver.New(store, postgres.DefaultProviderName)
	serveErr := serveAsync(server, listener)

	var result error
	select {
	case result = <-serveErr:
	case <-ctx.Done():
		result = shutdown(server)
	}

	stopPruner()
	<-prunerDone
	return result
}

// serveAsync runs the server on listener in a goroutine and reports its exit on the returned
// channel, so the caller can select between a server crash and a shutdown signal.
func serveAsync(server *httpserver.Server, listener net.Listener) <-chan error {
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("llmgw listening on %s", listener.Addr())
		serveErr <- server.Serve(listener)
	}()
	return serveErr
}

// startPruner runs the retention sweep immediately, then on every tick until ctx is cancelled. It
// returns a channel closed once the goroutine has exited.
func startPruner(ctx context.Context, store *postgres.Store) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		prune(ctx, store)

		ticker := time.NewTicker(pruneInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune(ctx, store)
			}
		}
	}()
	return done
}

// prune removes aged usage_event rows and expired reservations, logging the counts.
func prune(ctx context.Context, store *postgres.Store) {
	usageDeleted, resvDeleted, err := store.PruneOlderThan(ctx, usageRetention)
	if err != nil {
		log.Printf("llmgw: retention prune failed: %v", err)
		return
	}
	log.Printf("llmgw: retention prune removed %d usage events, %d expired reservations", usageDeleted, resvDeleted)
}

// shutdown drains in-flight HTTP requests with a bounded timeout.
func shutdown(server *httpserver.Server) error {
	log.Print("llmgw: shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	return server.Shutdown(ctx)
}

// seedTokens persists the configured refresh tokens for accounts that have no row yet.
func seedTokens(ctx context.Context, store *postgres.Store, seeds []config.RefreshToken) error {
	for _, seed := range seeds {
		if err := seedToken(ctx, store, seed); err != nil {
			return err
		}
	}
	return nil
}

// seedToken persists one seed refresh token only when the account has no stored token.
// A persisted token may already be rotated, so an existing row is never overwritten.
func seedToken(ctx context.Context, store *postgres.Store, seed config.RefreshToken) error {
	_, err := store.LoadToken(ctx, seed.Label)
	if err == nil {
		return nil
	}
	if !errors.Is(err, domain.ErrTokenNotFound) {
		return fmt.Errorf("check seed token %q:\n%w", seed.Label, err)
	}

	if err := store.SaveToken(ctx, seed.Label, domain.Token{RefreshToken: seed.Token}); err != nil {
		return fmt.Errorf("seed token %q:\n%w", seed.Label, err)
	}
	return nil
}
