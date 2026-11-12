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

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const releaseVerificationMemoryRoute = "/verification-memory/release"

func runReleaseVerificationMemoryMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runReleaseVerificationMemoryCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided release-verification-memory:", err)
		os.Exit(1)
	}
}

type releaseVerificationMemoryConfig struct {
	DBPath      string
	TaskID      domain.TaskID
	CommandGone bool
}

// runReleaseVerificationMemoryCommand re-runs the absence proof for a task's
// held verification reservations and releases each one it proves (#2977).
// The proof and the release run inside the running daemon, under its
// publication lane, so the command refuses when no daemon runs and reports
// the lane busy rather than waiting for a pass. -command-gone is the
// operator's word that no CLI of a crash-interrupted command still runs; the
// daemon still proves every owned container absent.
func runReleaseVerificationMemoryCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer,
) (err error) {
	cfg, err := parseReleaseVerificationMemoryConfig(args, stderr)
	if err != nil {
		return err
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, releaseVerificationMemoryRoute, args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	daemon, inDaemon := ctx.Value(daemonStoreContextKey{}).(daemonStoreContext)
	if !inDaemon {
		return errors.New("no daemon is running for this database; start the daemon, since the release runs inside it")
	}
	if daemon.releaseVerificationMemory == nil {
		return errors.New("the running daemon has no publication lane to release verification memory through")
	}
	result, err := daemon.releaseVerificationMemory(ctx, cfg.TaskID, cfg.CommandGone)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write release result: %w", err)
	}
	return nil
}

func parseReleaseVerificationMemoryConfig(args []string, output io.Writer) (releaseVerificationMemoryConfig, error) {
	var cfg releaseVerificationMemoryConfig
	var task string
	flags := flag.NewFlagSet("freesided release-verification-memory", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DBPath, "db", "", "existing SQLite database path (required)")
	flags.StringVar(&task, "task", "", "task whose held verification reservations to re-prove (required)")
	flags.BoolVar(&cfg.CommandGone, "command-gone", false,
		"confirm no container CLI of a crash-interrupted verification command still runs; "+
			"the daemon still proves every owned container absent")
	if err := flags.Parse(args); err != nil {
		return releaseVerificationMemoryConfig{}, err
	}
	if flags.NArg() != 0 {
		return releaseVerificationMemoryConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if cfg.DBPath == "" {
		return releaseVerificationMemoryConfig{}, errors.New("-db is required")
	}
	if task == "" {
		return releaseVerificationMemoryConfig{}, errors.New("-task is required")
	}
	cfg.TaskID = domain.TaskID(task)
	return cfg, nil
}
