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

func runSetPoolLimitMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runSetPoolLimitCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided set-pool-limit:", err)
		os.Exit(1)
	}
}

type setPoolLimitConfig struct {
	DBPath                string
	PoolID                domain.UsagePoolID
	MaxParallelExecutions int
}

// setPoolLimitResult reports the revision. Recorded is false when the pool
// already carried the requested limit, so no revision was written.
type setPoolLimitResult struct {
	Pool                          domain.UsagePoolID `json:"pool"`
	PreviousMaxParallelExecutions int                `json:"previous_max_parallel_executions"`
	MaxParallelExecutions         int                `json:"max_parallel_executions"`
	Recorded                      bool               `json:"recorded"`
}

// runSetPoolLimitCommand records a newer revision of an existing usage pool
// that changes only its parallel-execution limit. The admission gate reads the
// current revision on every admission, so a running daemon applies it to the
// next launch without a restart. An identity with no pool of its own is
// addressed by its effective pool, identity:<id>, as auth list prints it.
func runSetPoolLimitCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer,
) (err error) {
	cfg, err := parseSetPoolLimitConfig(args, stderr)
	if err != nil {
		return err
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, "/usage-pools/limit", args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	var result setPoolLimitResult
	if err := handle.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		pool, err := tx.GetUsagePool(ctx, cfg.PoolID)
		if err != nil {
			return err
		}
		result = setPoolLimitResult{
			Pool:                          pool.ID,
			PreviousMaxParallelExecutions: pool.MaxParallelExecutions,
			MaxParallelExecutions:         cfg.MaxParallelExecutions,
		}
		if pool.MaxParallelExecutions == cfg.MaxParallelExecutions {
			return nil
		}
		pool.MaxParallelExecutions = cfg.MaxParallelExecutions
		result.Recorded = true
		return tx.RecordUsagePool(ctx, pool, time.Now().UTC())
	}); err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write pool limit result: %w", err)
	}
	return nil
}

func parseSetPoolLimitConfig(args []string, output io.Writer) (setPoolLimitConfig, error) {
	var cfg setPoolLimitConfig
	var pool string
	flags := flag.NewFlagSet("freesided set-pool-limit", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DBPath, "db", "", "existing SQLite database path (required)")
	flags.StringVar(&pool, "pool", "", "recorded usage pool id, as auth list prints it (required)")
	flags.IntVar(&cfg.MaxParallelExecutions, "max-parallel-executions", 0,
		"launches the pool may run at once, at least 1 (required)")
	if err := flags.Parse(args); err != nil {
		return setPoolLimitConfig{}, err
	}
	if flags.NArg() != 0 {
		return setPoolLimitConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if cfg.DBPath == "" {
		return setPoolLimitConfig{}, errors.New("-db is required")
	}
	if pool == "" {
		return setPoolLimitConfig{}, errors.New("-pool is required")
	}
	if cfg.MaxParallelExecutions < 1 {
		return setPoolLimitConfig{}, errors.New("-max-parallel-executions must be at least 1")
	}
	cfg.PoolID = domain.UsagePoolID(pool)
	return cfg, nil
}
