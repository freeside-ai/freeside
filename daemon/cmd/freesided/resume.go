package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe"
	"github.com/freeside-ai/freeside/daemon/internal/observe/observedb"
)

// runResumeMain reattaches observation to one exact non-terminal run. It
// never derives or allocates another identity; terminal runs explicitly point
// the operator to reattempt.
func runResumeMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err := runResumeCommand(ctx, args, os.Stdout, os.Stderr)
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "freesided resume:", err)
	stop()
	if errors.Is(err, observe.ErrUsage) {
		os.Exit(2)
	}
	os.Exit(1)
}

func runResumeCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("freesided resume", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	runID := flags.String("run", "", "exact live run id to resume (required)")
	interval := flags.Duration("interval", observe.DefaultInterval, "observation read cadence")
	window := flags.Duration("freshness-window", domain.DefaultObservationFreshnessWindow,
		"age past which the last observation reads as an observation gap")
	once := flags.Bool("once", false, "print one snapshot instead of following")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", observe.ErrUsage, err)
	}
	switch {
	case flags.NArg() != 0:
		return fmt.Errorf("%w: unexpected positional arguments: %v", observe.ErrUsage, flags.Args())
	case *dbPath == "":
		return fmt.Errorf("%w: -db is required", observe.ErrUsage)
	case *runID == "":
		return fmt.Errorf("%w: -run is required", observe.ErrUsage)
	case *interval <= 0:
		return fmt.Errorf("%w: -interval must be positive, got %s", observe.ErrUsage, *interval)
	case *window <= 0:
		return fmt.Errorf("%w: -freshness-window must be positive, got %s", observe.ErrUsage, *window)
	}
	st, err := observedb.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	_, conclusion, readErr := st.ObserveConclusion(ctx, domain.RunID(*runID))
	closeErr := st.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if err := terminalResumeError(domain.RunID(*runID), conclusion); err != nil {
		return err
	}
	followArgs := []string{
		"-db", *dbPath, "-run", *runID,
		"-interval", interval.String(), "-freshness-window", window.String(),
	}
	if *once {
		followArgs = append(followArgs, "-once")
	}
	return observe.Run(ctx, followArgs, stdout, stderr)
}

func terminalResumeError(runID domain.RunID, conclusion domain.RunConclusion) error {
	if !conclusion.Final {
		return nil
	}
	return fmt.Errorf("run %q is terminal in state %q; use freesided reattempt to create a new attempt",
		runID, conclusion.Outcome)
}
