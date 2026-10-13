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
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// runAbandonMain implements `freesided abandon --task <id>` (issue #1318 D6):
// it records an explicit task abandonment, releasing the task's WIP admission
// slot, then expires its open approval and execution-recovery cards. Repeating
// it retires any eligible cards raised since the previous call.
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
	Released     bool            `json:"released"`
	RetiredItems []domain.ItemID `json:"retired_items"`
}

var errAbandonUnchanged = errors.New("abandon: unchanged")

func runAbandonCommand(ctx context.Context, cfg abandonCommandConfig) (abandonResult, error) {
	if err := validateAbandonConfig(cfg); err != nil {
		return abandonResult{}, fmt.Errorf("abandon: %w", err)
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeMigrating)
	if err != nil {
		return abandonResult{}, fmt.Errorf("abandon: open store: %w", err)
	}
	if client != nil {
		defer client.Close()
		cfg.DBPath = client.dbPath
		var result abandonResult
		err := client.call(ctx, "/tasks/abandon", cfg, &result)
		return result, err
	}
	defer func() { _ = handle.Close() }()
	st := handle.store
	var released bool
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		before, err := tx.GetTaskSnapshot(ctx, cfg.TaskID)
		if err != nil {
			return err
		}
		released, err = tx.AbandonTask(ctx, cfg.TaskID, time.Now().UTC())
		if err != nil {
			return err
		}
		after, err := tx.GetTaskSnapshot(ctx, cfg.TaskID)
		if err != nil {
			return err
		}
		if before.Snapshot.EntityVersion == after.Snapshot.EntityVersion {
			return errAbandonUnchanged
		}
		return nil
	}); err != nil && !errors.Is(err, errAbandonUnchanged) {
		return abandonResult{}, fmt.Errorf("abandon: task %q: %w", cfg.TaskID, err)
	}
	// A stale card can fail its persistence policy gate. Keep the slot release
	// committed even then; the operator can retry retirement with abandon.
	retired := []domain.ItemID{}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		retired, err = engine.RetireAbandonedTaskItems(ctx, tx, cfg.TaskID)
		if err != nil {
			return err
		}
		if len(retired) == 0 {
			return errAbandonUnchanged
		}
		return nil
	}); err != nil && !errors.Is(err, errAbandonUnchanged) {
		return abandonResult{}, fmt.Errorf("abandon: task %q: slot-release step committed; retire items (retry abandon): %w", cfg.TaskID, err)
	}
	return abandonResult{TaskID: cfg.TaskID, Released: released, RetiredItems: retired}, nil
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
