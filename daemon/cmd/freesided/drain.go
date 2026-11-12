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

	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// updateDrainRoute is the control route behind freesided drain.
const updateDrainRoute = "/update-drain"

// drainAction is what one freesided drain asks of the daemon.
type drainAction string

const (
	// drainRequest puts an update drain in force, or keeps the one in force.
	drainRequest drainAction = "request"
	// drainCancel lifts the drain in force, if any.
	drainCancel drainAction = "cancel"
	// drainStatus changes nothing.
	drainStatus drainAction = "status"
)

// allDrainActions registers every drainAction.
var allDrainActions = []drainAction{drainRequest, drainCancel, drainStatus}

func (a drainAction) valid() bool {
	switch a {
	case drainRequest, drainCancel, drainStatus:
		return true
	default:
		return false
	}
}

// runDrainMain implements `freesided drain -db <database> [-cancel | -status]`
// (issue #3104). With no flag it asks the running daemon to drain for an
// update: it starts no new work and lets what runs finish. -cancel lifts the
// drain and -status changes nothing. Each form prints the daemon's restart
// readiness as JSON, so the installer can poll -status until it reads ready.
//
// It needs a running daemon and never opens the store itself: the drain
// holds what a running daemon launches, and a daemon that isn't running has
// nothing to drain.
func runDrainMain(args []string) {
	cfg, err := parseDrainCommand(args, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided drain:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	readiness, err := runDrainCommand(ctx, cfg)
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(readiness)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

func parseDrainCommand(args []string, stderr io.Writer) (drainCommandConfig, error) {
	flags := flag.NewFlagSet("freesided drain", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path of the running daemon (required)")
	cancel := flags.Bool("cancel", false, "lift the update drain in force")
	status := flags.Bool("status", false, "print restart readiness without changing anything")
	if err := flags.Parse(args); err != nil {
		return drainCommandConfig{}, err
	}
	if flags.NArg() != 0 {
		return drainCommandConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if *cancel && *status {
		return drainCommandConfig{}, errors.New("-cancel and -status are exclusive")
	}
	cfg := drainCommandConfig{DBPath: *dbPath, Action: drainRequest}
	if *cancel {
		cfg.Action = drainCancel
	}
	if *status {
		cfg.Action = drainStatus
	}
	if err := validateDrainConfig(cfg); err != nil {
		return drainCommandConfig{}, err
	}
	return cfg, nil
}

// drainCommandConfig is also the /update-drain control payload.
type drainCommandConfig struct {
	DBPath string      `json:"db_path"`
	Action drainAction `json:"action"`
}

func validateDrainConfig(cfg drainCommandConfig) error {
	switch {
	case cfg.DBPath == "":
		return errors.New("-db is required")
	case !cfg.Action.valid():
		return fmt.Errorf("unknown drain action %q", cfg.Action)
	default:
		return nil
	}
}

// runDrainCommand forwards the action to the daemon that holds cfg.DBPath.
func runDrainCommand(ctx context.Context, cfg drainCommandConfig) (signet.RestartReadinessResponse, error) {
	if err := validateDrainConfig(cfg); err != nil {
		return signet.RestartReadinessResponse{}, fmt.Errorf("drain: %w", err)
	}
	access, err := openStoreOrControl(cfg.DBPath)
	if err != nil {
		return signet.RestartReadinessResponse{}, fmt.Errorf("drain: %w", err)
	}
	defer func() { _ = access.Close() }()
	if access.client == nil {
		// The lock was free, so no daemon runs; release it unopened.
		return signet.RestartReadinessResponse{}, fmt.Errorf(
			"drain: no daemon is running on %s; there is nothing to drain", cfg.DBPath)
	}
	cfg.DBPath = access.client.dbPath
	var readiness signet.RestartReadinessResponse
	if err := access.client.call(ctx, updateDrainRoute, cfg, &readiness); err != nil {
		return signet.RestartReadinessResponse{}, err
	}
	return readiness, nil
}

// updateDrainInDaemon applies action inside the running daemon, then reads
// restart readiness after it. The control socket's peer check already limits
// callers to the daemon's account, so the drain needs no other binding: it
// holds work and grants nothing.
func updateDrainInDaemon(
	ctx context.Context, drain engine.UpdateDrain, attention *signet.Service, action drainAction,
) (signet.RestartReadinessResponse, error) {
	if !action.valid() {
		return signet.RestartReadinessResponse{}, fmt.Errorf("drain: unknown action %q", action)
	}
	switch action {
	case drainRequest:
		if _, err := drain.Request(ctx); err != nil {
			return signet.RestartReadinessResponse{}, fmt.Errorf("drain: %w", err)
		}
	case drainCancel:
		if _, err := drain.Cancel(ctx); err != nil {
			return signet.RestartReadinessResponse{}, fmt.Errorf("drain: cancel: %w", err)
		}
	case drainStatus:
	}
	return attention.RestartReadiness(ctx)
}
