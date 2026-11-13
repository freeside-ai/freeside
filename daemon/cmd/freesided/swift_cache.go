package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/macverify"
)

func runSwiftCacheMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runCacheCommand(ctx, "swift-cache", args, os.Stdout, os.Stderr, macverify.PopulateSwiftCache); err != nil {
		fmt.Fprintln(os.Stderr, "freesided swift-cache:", err)
		os.Exit(1)
	}
}

// runCacheCommand is the operator's door to one of the macOS verification
// executor's offline caches, named by verb: swift-cache for Swift
// dependencies (#2880), go-cache for the Go toolchain and modules (#3155).
// It only parses flags; macverify owns every check and write, so this file
// and go_cache.go stay off the Tier A list.
func runCacheCommand(ctx context.Context, verb string, args []string, stdout, stderr io.Writer,
	populate func(context.Context, macverify.PopulateOptions) (macverify.PopulateResult, error),
) error {
	if len(args) == 0 || args[0] != "populate" {
		return fmt.Errorf("usage: freesided %s populate --executor-root <root> --source <checkout>", verb)
	}
	flags := flag.NewFlagSet("freesided "+verb+" populate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("executor-root", "", "macOS verification executor root (required)")
	source := flags.String("source", "", "checkout of the project at the commit to serve (required)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %v", flags.Args())
	}
	if *root == "" || *source == "" {
		return errors.New("--executor-root and --source are required")
	}
	sourcePath, err := filepath.Abs(*source)
	if err != nil {
		return err
	}
	res, err := populate(ctx, macverify.PopulateOptions{
		ExecutorRoot: *root, Source: sourcePath, Progress: stderr,
	})
	if err != nil {
		return err
	}
	state := "already cached"
	if res.Populated {
		state = "populated"
	}
	_, err = fmt.Fprintf(stdout, "%s %s for %s\n", state, res.Key, strings.Join(res.Files, ", "))
	return err
}
