package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/macverify"
)

// runGoCacheMain is freesided go-cache populate: it installs the pinned Go
// toolchain and fills the offline Go module cache entry for a checkout, for
// the macOS verification executor (#3155). runCacheCommand parses its flags.
func runGoCacheMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runCacheCommand(ctx, "go-cache", args, os.Stdout, os.Stderr, macverify.PopulateGoCache); err != nil {
		fmt.Fprintln(os.Stderr, "freesided go-cache:", err)
		os.Exit(1)
	}
}
