package main

import (
	"context"
	"fmt"
	"log"
	"net"

	"github.com/clemsix6/LLMGW/internal/adapter/httpserver"
	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/config"
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

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s:\n%w", cfg.Listen, err)
	}

	log.Printf("llmgw listening on %s", listener.Addr())
	return httpserver.New().Serve(listener)
}
