package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Version is the CLI/daemon build version. Override via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := newRootCmd(defaultApp())
	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
