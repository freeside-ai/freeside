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
// it releases the task's WIP slot, durably closes that workflow episode, then
// retires its decisions. Each step commits independently; -episode repairs
// exactly the captured episode after a partial failure.
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
		if result.Episode != 0 {
			_ = json.NewEncoder(os.Stdout).Encode(result)
		}
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
	episode := flags.Int("episode", 0, "repair this already-abandoned start ordinal without releasing a newer episode")
	if err := flags.Parse(args); err != nil {
		return abandonCommandConfig{}, err
	}
	if flags.NArg() != 0 {
		return abandonCommandConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	cfg := abandonCommandConfig{DBPath: *dbPath, TaskID: domain.TaskID(*task), Episode: *episode}
	if err := validateAbandonConfig(cfg); err != nil {
		return abandonCommandConfig{}, err
	}
	return cfg, nil
}

type abandonCommandConfig struct {
	DBPath  string
	TaskID  domain.TaskID
	Episode int
}

type abandonResult struct {
	TaskID domain.TaskID `json:"task_id"`
	// Released is true when this call released a held WIP slot, false when the
	// task held none (already abandoned, completed, or never started).
	Released        bool            `json:"released"`
	RetiredItems    []domain.ItemID `json:"retired_items"`
	Episode         int             `json:"episode"`
	WorkflowClosed  bool            `json:"workflow_closed"`
	CleanupComplete bool            `json:"cleanup_complete"`
	Error           string          `json:"error,omitempty"`
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
		if err == nil && result.Error != "" {
			err = errors.New(result.Error)
		}
		return result, err
	}
	defer func() { _ = handle.Close() }()
	st := handle.store
	result := abandonResult{TaskID: cfg.TaskID, Episode: cfg.Episode, RetiredItems: []domain.ItemID{}}
	if cfg.Episode == 0 {
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			before, err := tx.GetTaskSnapshot(ctx, cfg.TaskID)
			if err != nil {
				return err
			}
			if start := before.Value.CurrentStart(); start != nil {
				result.Episode = start.Ordinal
			}
			result.Released, err = tx.AbandonTask(ctx, cfg.TaskID, time.Now().UTC())
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
	}
	if result.Episode == 0 {
		result.CleanupComplete = true
		return result, nil
	}
	partial := func(step string, cause error) (abandonResult, error) {
		err := fmt.Errorf("abandon: task %q episode %d: released_by_this_call=%t; workflow_closed=%t; %s: %w; repair with abandon -task %s -episode %d",
			cfg.TaskID, result.Episode, result.Released, result.WorkflowClosed, step, cause, cfg.TaskID, result.Episode)
		result.Error = err.Error()
		return result, err
	}
	commitClosure := func() error {
		return st.Write(ctx, func(tx *store.WriteTx) error {
			_, created, err := tx.CloseTaskWorkflow(ctx, cfg.TaskID, result.Episode, time.Now().UTC())
			if err != nil {
				return err
			}
			if !created {
				return errAbandonUnchanged
			}
			return nil
		})
	}
	var closureErr error
	if daemon, ok := ctx.Value(daemonStoreContextKey{}).(daemonStoreContext); ok && daemon.commitWorkflowClosure != nil {
		closureErr = daemon.commitWorkflowClosure(ctx, cfg.TaskID, commitClosure)
	} else {
		closureErr = commitClosure()
	}
	if closureErr != nil && !errors.Is(closureErr, errAbandonUnchanged) {
		return partial("close workflow", closureErr)
	}
	result.WorkflowClosed = true
	// Retirement policy cannot undo either the slot release or the closure.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		result.RetiredItems, err = engine.RetireClosedTaskWorkflowItems(ctx, tx, cfg.TaskID, result.Episode)
		if err != nil {
			return err
		}
		if len(result.RetiredItems) == 0 {
			return errAbandonUnchanged
		}
		return nil
	}); err != nil && !errors.Is(err, errAbandonUnchanged) {
		result.RetiredItems = []domain.ItemID{}
		return partial("retire items", err)
	}
	result.CleanupComplete = true
	return result, nil
}

func validateAbandonConfig(cfg abandonCommandConfig) error {
	switch {
	case cfg.DBPath == "":
		return errors.New("-db is required")
	case cfg.TaskID == "":
		return errors.New("-task is required")
	case cfg.Episode < 0:
		return errors.New("-episode must be a positive start ordinal")
	default:
		return nil
	}
}
