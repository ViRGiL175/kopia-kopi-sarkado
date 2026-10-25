package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)

	os.Exit(code)
}
