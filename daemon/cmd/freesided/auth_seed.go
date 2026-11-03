package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// seededUsageDetail is the collection attempt detail a seeded reading carries,
// so the record says where its reading came from.
const seededUsageDetail = "seeded by freesided auth seed-exhausted"

type authSeedExhaustedConfig struct {
	DBPath   string
	Pool     domain.UsagePoolID
	Window   string
	Model    string
	ResetsAt time.Time
}

// runAuthSeedExhaustedCommand is an exit-run fixture (#2393): it marks one
// pool's model-scoped window exhausted until a reset, so the router passes the
// pool over without spending real allowance. It writes the pool's usage record
// as a collection would: the window mapped to the route model id, an
// exhausted reading with the reset under the collector's current generation,
// and a succeeded attempt whose detail says it was seeded. A later collection
// replaces the reading. Through the running daemon it runs there, like the
// other auth verbs.
func runAuthSeedExhaustedCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	cfg, err := parseAuthSeedExhaustedConfig(args, stderr)
	if err != nil {
		return err
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, "/auth/usage/seed-exhausted", args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	now := time.Now().UTC()
	if !cfg.ResetsAt.After(now) {
		return fmt.Errorf("-resets-at %s is not in the future", cfg.ResetsAt.Format(time.RFC3339))
	}
	var record domain.PoolUsageObservation
	if err := handle.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		record, err = seedExhaustedWindow(ctx, tx, cfg, now)
		return err
	}); err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(record); err != nil {
		return fmt.Errorf("write usage record: %w", err)
	}
	return nil
}

func seedExhaustedWindow(
	ctx context.Context, tx *store.WriteTx, cfg authSeedExhaustedConfig, now time.Time,
) (domain.PoolUsageObservation, error) {
	record, err := tx.GetPoolUsageObservation(ctx, cfg.Pool)
	if errors.Is(err, store.ErrNotFound) {
		var collector domain.ClientEnrollmentID
		if collector, err = tx.DefaultPoolCollector(ctx, cfg.Pool); err == nil {
			record = domain.PoolUsageObservation{Pool: cfg.Pool, Collector: collector}
		}
	}
	if err != nil {
		return domain.PoolUsageObservation{}, err
	}
	generation, err := tx.CurrentEnrollmentGeneration(ctx, record.Collector)
	if err != nil {
		return domain.PoolUsageObservation{}, fmt.Errorf("collector %s: %w", record.Collector, err)
	}
	mapping := domain.ModelWindow{RouteModelID: cfg.Model, Window: cfg.Window}
	if !slices.Contains(record.ModelWindows, mapping) {
		record.ModelWindows = append(slices.Clone(record.ModelWindows), mapping)
		slices.SortFunc(record.ModelWindows, func(a, b domain.ModelWindow) int {
			return cmp.Or(cmp.Compare(a.RouteModelID, b.RouteModelID), cmp.Compare(a.Window, b.Window))
		})
	}
	reading := domain.WindowReading{
		Window: cfg.Window, EnrollmentID: record.Collector, Generation: generation.Ordinal,
		Status: domain.WindowStatusExhausted, ResetsAt: new(cfg.ResetsAt), ObservedAt: now,
	}
	record.Windows = slices.DeleteFunc(slices.Clone(record.Windows), func(held domain.WindowReading) bool {
		return held.Window == cfg.Window
	})
	record.Windows = append(record.Windows, reading)
	slices.SortFunc(record.Windows, func(a, b domain.WindowReading) int { return cmp.Compare(a.Window, b.Window) })
	record.LastAttempt = &domain.CollectionAttempt{
		Status: domain.CollectionAttemptSucceeded, At: now, Detail: seededUsageDetail,
	}
	if err := tx.RecordPoolUsageObservation(ctx, record, now); err != nil {
		return domain.PoolUsageObservation{}, err
	}
	return tx.GetPoolUsageObservation(ctx, cfg.Pool)
}

func parseAuthSeedExhaustedConfig(args []string, output io.Writer) (authSeedExhaustedConfig, error) {
	var (
		cfg            authSeedExhaustedConfig
		pool, resetsAt string
	)
	flags := flag.NewFlagSet("freesided auth seed-exhausted", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DBPath, "db", "", "existing SQLite database path (required)")
	flags.StringVar(&pool, "pool", "", "usage pool id, as auth list prints it (required)")
	flags.StringVar(&cfg.Window, "window", "", "provider window to mark exhausted (required)")
	flags.StringVar(&cfg.Model, "model", "", "route model id the window limits (required)")
	flags.StringVar(&resetsAt, "resets-at", "", "RFC 3339 instant the window resets, in the future (required)")
	if err := flags.Parse(args); err != nil {
		return authSeedExhaustedConfig{}, err
	}
	if flags.NArg() != 0 {
		return authSeedExhaustedConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	for _, required := range []struct{ name, value string }{
		{"-db", cfg.DBPath}, {"-pool", pool}, {"-window", cfg.Window}, {"-model", cfg.Model}, {"-resets-at", resetsAt},
	} {
		if required.value == "" {
			return authSeedExhaustedConfig{}, fmt.Errorf("%s is required", required.name)
		}
	}
	at, err := time.Parse(time.RFC3339, resetsAt)
	if err != nil {
		return authSeedExhaustedConfig{}, fmt.Errorf("-resets-at: %w", err)
	}
	cfg.Pool, cfg.ResetsAt = domain.UsagePoolID(pool), at.UTC()
	return cfg, nil
}
