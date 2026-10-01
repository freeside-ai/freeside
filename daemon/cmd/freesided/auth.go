package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// runAuthMain is the enrollment lifecycle verb (plan §5.4, §10): `auth add`
// enrolls one harness client for an identity, `auth list` shows identities
// and their enrollments, and `auth adopt` enrolls the flag-selected identities
// over the stores they already hold and emits the tree patch selecting them.
func runAuthMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runAuthCommand(ctx, args, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided auth:", err)
		os.Exit(1)
	}
}

func runAuthCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: freesided auth add|adopt|list [flags]")
	}
	switch args[0] {
	case "add":
		return runAuthAddCommand(ctx, args[1:], stdin, stdout, stderr, productionAuthAddDeps())
	case "adopt":
		return runAuthAdoptCommand(ctx, args[1:], stdout, stderr, productionAuthAdoptDeps())
	case "list":
		return runAuthListCommand(ctx, args[1:], stdout, stderr)
	}
	return fmt.Errorf("unknown auth command %q (want add, adopt, or list)", args[0])
}
