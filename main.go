package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/cli"
)

func main() {
	code := cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	if code != 0 {
		fmt.Fprint(os.Stderr, "")
	}

	os.Exit(code)
}
