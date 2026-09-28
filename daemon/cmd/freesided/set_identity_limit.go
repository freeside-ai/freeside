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

func runSetIdentityLimitMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runSetIdentityLimitCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided set-identity-limit:", err)
		os.Exit(1)
	}
}

type setIdentityLimitConfig struct {
	DBPath                string
	IdentityID            domain.AuthIdentityID
	MaxParallelExecutions int
}

// setIdentityLimitResult reports the revision. Recorded is false when the
// identity already carried the requested limit, so no revision was written.
type setIdentityLimitResult struct {
	Identity                      domain.AuthIdentityID `json:"identity"`
	PreviousMaxParallelExecutions int                   `json:"previous_max_parallel_executions"`
	MaxParallelExecutions         int                   `json:"max_parallel_executions"`
	Recorded                      bool                  `json:"recorded"`
}

// runSetIdentityLimitCommand records a newer revision of an existing Claude
// identity that changes only its parallel-execution limit. The admission gate
// reads the current revision on every admission, so a running daemon applies
// it to the next dispatch without a restart. Only Claude identities are
// accepted: the Codex reviewer produces no admission records yet, so its
// limit would gate nothing (devlog/2026-09-28-1220-identity-limit-rollout.md).
func runSetIdentityLimitCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer,
) (err error) {
	cfg, err := parseSetIdentityLimitConfig(args, stderr)
	if err != nil {
		return err
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, "/auth-identities/limit", args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	var result setIdentityLimitResult
	if err := handle.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		identity, err := tx.GetAuthIdentity(ctx, cfg.IdentityID)
		if err != nil {
			return err
		}
		if identity.Provider != "claude" {
			return fmt.Errorf("identity %s has provider %q; only a Claude identity's limit can be set",
				identity.ID, identity.Provider)
		}
		result = setIdentityLimitResult{
			Identity:                      identity.ID,
			PreviousMaxParallelExecutions: identity.MaxParallelExecutions,
			MaxParallelExecutions:         cfg.MaxParallelExecutions,
		}
		if identity.MaxParallelExecutions == cfg.MaxParallelExecutions {
			return nil
		}
		identity.MaxParallelExecutions = cfg.MaxParallelExecutions
		result.Recorded = true
		return tx.RecordAuthIdentity(ctx, identity, time.Now().UTC())
	}); err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write identity limit result: %w", err)
	}
	return nil
}

func parseSetIdentityLimitConfig(args []string, output io.Writer) (setIdentityLimitConfig, error) {
	var cfg setIdentityLimitConfig
	var identity string
	flags := flag.NewFlagSet("freesided set-identity-limit", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DBPath, "db", "", "existing SQLite database path (required)")
	flags.StringVar(&identity, "identity", "", "recorded Claude auth identity id (required)")
	flags.IntVar(&cfg.MaxParallelExecutions, "max-parallel-executions", 0,
		"executions the identity may run at once, at least 1 (required)")
	if err := flags.Parse(args); err != nil {
		return setIdentityLimitConfig{}, err
	}
	if flags.NArg() != 0 {
		return setIdentityLimitConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if cfg.DBPath == "" {
		return setIdentityLimitConfig{}, errors.New("-db is required")
	}
	if identity == "" {
		return setIdentityLimitConfig{}, errors.New("-identity is required")
	}
	if cfg.MaxParallelExecutions < 1 {
		return setIdentityLimitConfig{}, errors.New("-max-parallel-executions must be at least 1")
	}
	cfg.IdentityID = domain.AuthIdentityID(identity)
	return cfg, nil
}
