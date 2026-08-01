package observe

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe/observedb"
)

// The `freesided follow` verb lives here rather than in the command package
// so its containment boundary is a real one. A file in package main can call
// every unexported helper its siblings define without importing anything, so
// no assertion over that file proves what it can reach; a package can only
// reach what it imports, and containment_test.go holds this package's imports
// to a closed list that names no filesystem, process, or network capability.

// ErrUsage marks an invocation mistake (an unknown flag, a missing or
// malformed required value, a stray positional argument) as distinct from a
// failure to observe, so the command can exit 2 for the first and 1 for the
// second.
var ErrUsage = errors.New("invalid invocation")

// Run parses the follow verb's flags, opens the daemon's store, and follows
// the selected run to a final outcome, an interrupt, or (with -once) a single
// snapshot. Observing a run whose own outcome is blocked or failed succeeds:
// the command reports what the daemon saw, and the printed outcome line
// carries the run's verdict.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("freesided follow", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	runID := flags.String("run", "", "run id to follow (required)")
	interval := flags.Duration("interval", DefaultInterval, "observation read cadence")
	window := flags.Duration("freshness-window", domain.DefaultObservationFreshnessWindow,
		"age past which the last observation reads as an observation gap")
	once := flags.Bool("once", false, "print one snapshot instead of following")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	switch {
	case flags.NArg() != 0:
		return fmt.Errorf("%w: unexpected positional arguments: %v", ErrUsage, flags.Args())
	case *dbPath == "":
		return fmt.Errorf("%w: -db is required", ErrUsage)
	case *runID == "":
		return fmt.Errorf("%w: -run is required", ErrUsage)
	case *interval <= 0:
		return fmt.Errorf("%w: -interval must be positive, got %s", ErrUsage, *interval)
	case *window <= 0:
		return fmt.Errorf("%w: -freshness-window must be positive, got %s", ErrUsage, *window)
	}
	// observedb is the only database access this package has, and its whole
	// exported surface is open, observe, close: no write, checkpoint,
	// restore, or backup-file capability reaches the follow path.
	db, err := observedb.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	return Follow(ctx, db, stdout, Config{
		RunID: domain.RunID(*runID), Interval: *interval, Window: *window, Once: *once,
	})
}

// ExitCode maps a Run result onto the command's exit contract: 0 observed
// (including an interrupted follow, and including a run whose own outcome is
// blocked or failed), 1 could not observe, 2 invocation mistake.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrUsage):
		return 2
	default:
		return 1
	}
}
