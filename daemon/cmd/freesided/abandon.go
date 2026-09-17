package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// runAbandonMain implements `freesided abandon --task <id>` (issue #1318 D6):
// it records an explicit task abandonment, releasing the task's WIP admission
// slot, and is idempotent on a task whose current episode is already
// abandoned.
//
// It does not stop an in-flight provider execution or certify quiescence.
// Routine submission milestones cannot reopen the released episode. A later
// explicit, uncancelled reattempt needs cap-checked admission in the same write
// as its new start; a cancellation fence always refuses it.
func runAbandonMain(args []string) {
	cfg, err := parseAbandonCommand(args, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided abandon:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	result, err := runAbandonCommand(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

func parseAbandonCommand(args []string, stderr io.Writer) (abandonCommandConfig, error) {
	flags := flag.NewFlagSet("freesided abandon", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	task := flags.String("task", "", "task to abandon (required)")
	if err := flags.Parse(args); err != nil {
		return abandonCommandConfig{}, err
	}
	if flags.NArg() != 0 {
		return abandonCommandConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	cfg := abandonCommandConfig{DBPath: *dbPath, TaskID: domain.TaskID(*task)}
	if err := validateAbandonConfig(cfg); err != nil {
		return abandonCommandConfig{}, err
	}
	return cfg, nil
}

type abandonCommandConfig struct {
	DBPath string
	TaskID domain.TaskID
}

type abandonResult struct {
	TaskID domain.TaskID `json:"task_id"`
	// Released is true when this call released a held WIP slot, false when the
	// task held none (already abandoned, completed, or never started).
	Released bool `json:"released"`
}

func runAbandonCommand(ctx context.Context, cfg abandonCommandConfig) (abandonResult, error) {
	if err := validateAbandonConfig(cfg); err != nil {
		return abandonResult{}, fmt.Errorf("abandon: %w", err)
	}
	st, _, err := openStoreWithTopicKey(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		return abandonResult{}, fmt.Errorf("abandon: open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	var released bool
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		released, err = tx.AbandonTask(ctx, cfg.TaskID, time.Now().UTC())
		return err
	}); err != nil {
		return abandonResult{}, fmt.Errorf("abandon: task %q: %w", cfg.TaskID, err)
	}
	return abandonResult{TaskID: cfg.TaskID, Released: released}, nil
}

func validateAbandonConfig(cfg abandonCommandConfig) error {
	switch {
	case cfg.DBPath == "":
		return errors.New("-db is required")
	case cfg.TaskID == "":
		return errors.New("-task is required")
	default:
		return nil
	}
}
