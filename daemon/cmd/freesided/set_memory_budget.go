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

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func runSetMemoryBudgetMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runSetMemoryBudgetCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided set-memory-budget:", err)
		os.Exit(1)
	}
}

type setMemoryBudgetConfig struct {
	DBPath          string
	MemoryBudgetMiB int
}

// setMemoryBudgetResult reports the change. PreviousMemoryBudgetMiB is null
// when no budget was recorded, so the instance ran under its declared one.
// Recorded is false when the requested budget was already recorded, so
// nothing was written.
type setMemoryBudgetResult struct {
	PreviousMemoryBudgetMiB *int `json:"previous_memory_budget_mib"`
	MemoryBudgetMiB         int  `json:"memory_budget_mib"`
	Recorded                bool `json:"recorded"`
}

// runSetMemoryBudgetCommand records the instance's memory budget (plan §5.4).
// Every launch reads the recorded budget in the transaction that gates it, so
// a running daemon applies it at the next admission pass without a restart,
// and it wins over -memory-budget-mib on every later start. A lower budget
// limits only new reservations: memory already reserved stays reserved.
func runSetMemoryBudgetCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer,
) (err error) {
	cfg, err := parseSetMemoryBudgetConfig(args, stderr)
	if err != nil {
		return err
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, "/memory-budget", args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	result := setMemoryBudgetResult{MemoryBudgetMiB: cfg.MemoryBudgetMiB}
	if err := handle.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		previous, recorded, err := tx.MemoryBudget(ctx)
		if err != nil {
			return err
		}
		if recorded {
			result.PreviousMemoryBudgetMiB = &previous
			if previous == cfg.MemoryBudgetMiB {
				return nil
			}
		}
		result.Recorded = true
		return tx.RecordMemoryBudget(ctx, cfg.MemoryBudgetMiB, time.Now().UTC())
	}); err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write memory budget result: %w", err)
	}
	return nil
}

func parseSetMemoryBudgetConfig(args []string, output io.Writer) (setMemoryBudgetConfig, error) {
	var cfg setMemoryBudgetConfig
	flags := flag.NewFlagSet("freesided set-memory-budget", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DBPath, "db", "", "existing SQLite database path (required)")
	flags.IntVar(&cfg.MemoryBudgetMiB, "memory-budget-mib", 0,
		"memory every ward launch of this instance reserves against, in MiB, at least 1 (required)")
	if err := flags.Parse(args); err != nil {
		return setMemoryBudgetConfig{}, err
	}
	if flags.NArg() != 0 {
		return setMemoryBudgetConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if cfg.DBPath == "" {
		return setMemoryBudgetConfig{}, errors.New("-db is required")
	}
	if cfg.MemoryBudgetMiB < 1 {
		return setMemoryBudgetConfig{}, errors.New("-memory-budget-mib must be at least 1")
	}
	return cfg, nil
}
