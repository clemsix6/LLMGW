package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/clemsix6/LLMGW/internal/command"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := command.Run(ctx, os.Args[1:], command.Streams{
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Getenv: os.Getenv,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "llmgw: %v\n", err)
		os.Exit(1)
	}
}
