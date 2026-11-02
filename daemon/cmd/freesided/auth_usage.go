package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
)

type authUsageConfig struct {
	DBPath     string
	Pool       domain.UsagePoolID
	Enrollment domain.ClientEnrollmentID
}

// runAuthRefreshUsageCommand refreshes one pool's usage record through the
// running daemon (plan §10, usage collection rules): it joins the collection
// in flight or starts one, then prints the record with each window's
// freshness. Collection runs only in the daemon, so with none running it
// fails rather than collect on its own.
func runAuthRefreshUsageCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	cfg, err := parseAuthUsageConfig("refresh-usage", args, stderr, false)
	if err != nil {
		return err
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, "/auth/usage/refresh", args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	usage := runningUsageCollection(ctx)
	if usage == nil {
		return errors.New("refresh-usage needs the running daemon, which collects usage; no daemon holds this database")
	}
	report, err := usage.Refresh(ctx, cfg.Pool)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		return fmt.Errorf("write usage report: %w", err)
	}
	switch report.Outcome {
	case usagecollect.OutcomeCollected:
		return nil
	case usagecollect.OutcomeFailed:
		return fmt.Errorf("usage collection for %s failed: %s", cfg.Pool, report.Observation.LastAttempt.Detail)
	case usagecollect.OutcomeBackingOff:
		return fmt.Errorf("usage collection for %s is backing off after a failure until %s",
			cfg.Pool, report.BackoffUntil.Format(time.RFC3339))
	case usagecollect.OutcomeDiscarded:
		return fmt.Errorf("usage collection for %s was discarded: its collector or generation changed; refresh again", cfg.Pool)
	}
	return fmt.Errorf("usage collection for %s ended with outcome %q", cfg.Pool, report.Outcome)
}

// runAuthSetCollectorCommand makes an enrollment on the pool its collector,
// as a new record revision: the explicit operator action that is the only way
// the collector changes. Through the running daemon it also starts a
// collection under the new collector.
func runAuthSetCollectorCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	cfg, err := parseAuthUsageConfig("set-collector", args, stderr, true)
	if err != nil {
		return err
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, "/auth/collector", args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	var result usagecollect.Reassignment
	if usage := runningUsageCollection(ctx); usage != nil {
		result, err = usage.SetCollector(ctx, cfg.Pool, cfg.Enrollment)
	} else {
		result, err = usagecollect.Reassign(ctx, handle.store, cfg.Pool, cfg.Enrollment, time.Now().UTC())
	}
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write collector result: %w", err)
	}
	return nil
}

// runningUsageCollection is the running daemon's coordinator, nil when the
// command opened the store itself.
func runningUsageCollection(ctx context.Context) *usagecollect.Coordinator {
	daemon, ok := ctx.Value(daemonStoreContextKey{}).(daemonStoreContext)
	if !ok {
		return nil
	}
	return daemon.usageCollection
}

func parseAuthUsageConfig(verb string, args []string, output io.Writer, needsEnrollment bool) (authUsageConfig, error) {
	var cfg authUsageConfig
	var pool, enrollment string
	flags := flag.NewFlagSet("freesided auth "+verb, flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DBPath, "db", "", "existing SQLite database path (required)")
	flags.StringVar(&pool, "pool", "", "usage pool id, as auth list prints it (required)")
	if needsEnrollment {
		flags.StringVar(&enrollment, "enrollment", "", "client enrollment on the pool to collect for it (required)")
	}
	if err := flags.Parse(args); err != nil {
		return authUsageConfig{}, err
	}
	if flags.NArg() != 0 {
		return authUsageConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if cfg.DBPath == "" {
		return authUsageConfig{}, errors.New("-db is required")
	}
	if pool == "" {
		return authUsageConfig{}, errors.New("-pool is required")
	}
	if needsEnrollment && enrollment == "" {
		return authUsageConfig{}, errors.New("-enrollment is required")
	}
	cfg.Pool, cfg.Enrollment = domain.UsagePoolID(pool), domain.ClientEnrollmentID(enrollment)
	return cfg, nil
}
