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
	DBPath string
	PoolID domain.UsagePoolID
	// MaxParallelExecutions and MaxCallsPerSite are the limits to set; zero
	// leaves that limit as it is. At least one is given.
	MaxParallelExecutions int
	MaxCallsPerSite       int
}

// setPoolLimitResult reports the revision: each limit before and after it, so
// a limit the command left alone reads the same in both. Recorded is false
// when the pool already carried the requested limits, so no revision was
// written.
type setPoolLimitResult struct {
	Pool                          domain.UsagePoolID `json:"pool"`
	PreviousMaxParallelExecutions int                `json:"previous_max_parallel_executions"`
	MaxParallelExecutions         int                `json:"max_parallel_executions"`
	PreviousMaxCallsPerSite       int                `json:"previous_max_calls_per_site"`
	MaxCallsPerSite               int                `json:"max_calls_per_site"`
	Recorded                      bool               `json:"recorded"`
}

// runSetPoolLimitCommand records a newer revision of an existing usage pool
// that changes only the limits given: its parallel-execution limit, its
// judgment-call limit per site, or both. The admission gate and the inference
// client read the current revision on every admission and every call, so a
// running daemon applies it to the next launch or call without a restart. An identity with no pool of its own is
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
	if err := handle.store.Write(ctx, func(tx *store.WriteTx) error {
		pool, err := tx.GetUsagePool(ctx, cfg.PoolID)
		if err != nil {
			return err
		}
		updated := pool
		if cfg.MaxParallelExecutions != 0 {
			updated.MaxParallelExecutions = cfg.MaxParallelExecutions
		}
		if cfg.MaxCallsPerSite != 0 {
			updated.MaxCallsPerSite = cfg.MaxCallsPerSite
		}
		result = setPoolLimitResult{
			Pool:                          pool.ID,
			PreviousMaxParallelExecutions: pool.MaxParallelExecutions,
			MaxParallelExecutions:         updated.MaxParallelExecutions,
			PreviousMaxCallsPerSite:       pool.MaxCallsPerSite,
			MaxCallsPerSite:               updated.MaxCallsPerSite,
		}
		if updated == pool {
			return nil
		}
		pool = updated
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
		"launches the pool may run at once, at least 1")
	flags.IntVar(&cfg.MaxCallsPerSite, "max-calls-per-site", 0,
		"judgment calls one inference site may run at once on the pool, at least 1")
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
	given := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if !given["max-parallel-executions"] && !given["max-calls-per-site"] {
		return setPoolLimitConfig{}, errors.New("-max-parallel-executions or -max-calls-per-site is required")
	}
	if given["max-parallel-executions"] && cfg.MaxParallelExecutions < 1 {
		return setPoolLimitConfig{}, errors.New("-max-parallel-executions must be at least 1")
	}
	if given["max-calls-per-site"] && cfg.MaxCallsPerSite < 1 {
		return setPoolLimitConfig{}, errors.New("-max-calls-per-site must be at least 1")
	}
	cfg.PoolID = domain.UsagePoolID(pool)
	return cfg, nil
}
