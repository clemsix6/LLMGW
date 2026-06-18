package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"

	"github.com/clemsix6/LLMGW/internal/adapter/httpserver"
	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/adapter/provider/claudemax"
	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("llmgw: %v", err)
	}
}

// run loads configuration, opens the store (applying migrations), and serves HTTP.
func run() error {
	ctx := context.Background()

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

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s:\n%w", cfg.Listen, err)
	}

	log.Printf("llmgw listening on %s", listener.Addr())
	return httpserver.New(store, postgres.DefaultProviderName).Serve(listener)
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
