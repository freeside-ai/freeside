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
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Exit codes of freesided undo-update. The installer branches on them: a
// lifted drain means the new build reopened, so it starts that build again
// instead of keeping the undo pending.
const (
	undoUpdateExitRefused = 1
	undoUpdateExitUsage   = 2
	undoUpdateExitLifted  = 3
)

var errUndoUpdateUsage = errors.New("usage")

func runUndoUpdateMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err := runUndoUpdateCommand(ctx, args, os.Stdout, os.Stderr)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	fmt.Fprintln(os.Stderr, "freesided undo-update:", err)
	os.Exit(undoUpdateExitCode(err))
}

func undoUpdateExitCode(err error) int {
	switch {
	case errors.Is(err, errUndoUpdateUsage):
		return undoUpdateExitUsage
	case errors.Is(err, store.ErrUpdateDrainLifted):
		return undoUpdateExitLifted
	default:
		return undoUpdateExitRefused
	}
}

// runUndoUpdateCommand implements
// `freesided undo-update -db <db> -checkpoint <file> -sha256 <hex>` (issue
// #3108): it puts back the database an installer checkpointed before an
// update, once the updated daemon failed to start. It holds the daemon lock,
// so it refuses while any daemon runs on -db, and store.UndoUpdate's lift
// check refuses once the new build has lifted the update drain, so it never
// downgrades a database a new build reopened.
//
// Exit 0 means restored, 3 refused because the drain was lifted, 1 any other
// refusal or failure, and 2 a usage error. It prints the checkpoint's time
// and the restored schema version, never row content.
func runUndoUpdateCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("freesided undo-update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "the live SQLite database to restore (required)")
	checkpoint := flags.String("checkpoint", "", "the checkpoint the installer took before the update (required)")
	checkpointSHA256 := flags.String("sha256", "", "the checkpoint's sha256 the installer recorded, in hex (required)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %w", errUndoUpdateUsage, err)
	}
	if *dbPath == "" || *checkpoint == "" || *checkpointSHA256 == "" || flags.NArg() != 0 {
		return fmt.Errorf("%w: -db, -checkpoint, and -sha256 are required, and nothing else", errUndoUpdateUsage)
	}
	// Absolute, so the lock and every file beside the database name the
	// same paths a daemon started with -db would.
	db, err := filepath.Abs(*dbPath)
	if err != nil {
		return fmt.Errorf("%w: -db: %w", errUndoUpdateUsage, err)
	}
	checkpointPath, err := filepath.Abs(*checkpoint)
	if err != nil {
		return fmt.Errorf("%w: -checkpoint: %w", errUndoUpdateUsage, err)
	}

	lock, err := requireNoDaemon("undo-update", db)
	if err != nil {
		return err
	}
	result, err := store.UndoUpdate(ctx, db, checkpointPath, *checkpointSHA256)
	err = errors.Join(err, lock.Close())
	if err != nil {
		if errors.Is(err, store.ErrUpdateDrainLifted) {
			return fmt.Errorf("nothing was restored: %w", err)
		}
		return err
	}
	resumed := ""
	if result.Resumed {
		resumed = ", finishing an interrupted undo"
	}
	_, err = fmt.Fprintf(stdout,
		"restored %s from the checkpoint taken %s%s; schema version %d; changes made after the checkpoint were discarded\n",
		db, result.CheckpointTime.Format(time.RFC3339), resumed, result.SchemaVersion)
	return err
}
