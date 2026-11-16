package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// executionPauseRoute is the control route behind freesided execution.
const executionPauseRoute = "/execution-pause"

// executionPausePollInterval is how often freesided execution pause reads
// the daemon's status while it waits for the release.
const executionPausePollInterval = 2 * time.Second

// executionPauseAction is what one freesided execution asks of prod.
type executionPauseAction string

const (
	// executionPauseRequest puts an execution pause in force, or keeps the
	// one in force.
	executionPauseRequest executionPauseAction = "pause"
	// executionPauseResume clears the pause, and restarts prod when it runs
	// without execution.
	executionPauseResume executionPauseAction = "resume"
	// executionPauseStatus changes nothing.
	executionPauseStatus executionPauseAction = "status"
)

// allExecutionPauseActions registers every executionPauseAction.
var allExecutionPauseActions = []executionPauseAction{
	executionPauseRequest, executionPauseResume, executionPauseStatus,
}

func (a executionPauseAction) valid() bool {
	switch a {
	case executionPauseRequest, executionPauseResume, executionPauseStatus:
		return true
	default:
		return false
	}
}

// executionPauseCommandConfig is also the /execution-pause control payload.
type executionPauseCommandConfig struct {
	DBPath string               `json:"db_path"`
	Action executionPauseAction `json:"action"`
}

// runExecutionMain implements `freesided execution pause|resume|status -db
// <database>` (issue #3204), which hands the App authority between an
// executing prod and a deputy run.
//
// pause asks the running prod to start no new work, wait for its running
// work to finish, and restart without execution; it prints prod's status as
// it changes and returns once a prod without execution reports the pause
// released, so a deputy started next finds the writer lock free.
// Interrupting it leaves prod finishing the pause. resume clears the pause,
// and restarts a prod running without execution, which retries its gated
// order; an executing prod keeps its running work. With no daemon running it
// records the resume for the next start. status prints the pause and restart
// readiness.
func runExecutionMain(args []string) {
	cfg, err := parseExecutionCommand(args, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided execution:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runExecutionCommand(ctx, cfg, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

func parseExecutionCommand(args []string, stderr io.Writer) (executionPauseCommandConfig, error) {
	if len(args) == 0 {
		return executionPauseCommandConfig{}, errors.New("want pause, resume, or status")
	}
	action := executionPauseAction(args[0])
	if !action.valid() {
		return executionPauseCommandConfig{}, fmt.Errorf("unknown subcommand %q; want pause, resume, or status", args[0])
	}
	flags := flag.NewFlagSet("freesided execution "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path of prod (required)")
	if err := flags.Parse(args[1:]); err != nil {
		return executionPauseCommandConfig{}, err
	}
	if flags.NArg() != 0 {
		return executionPauseCommandConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if *dbPath == "" {
		return executionPauseCommandConfig{}, errors.New("-db is required")
	}
	return executionPauseCommandConfig{DBPath: *dbPath, Action: action}, nil
}

func runExecutionCommand(ctx context.Context, cfg executionPauseCommandConfig, stdout, stderr io.Writer) error {
	if cfg.Action == executionPauseResume {
		defer noteRigLease(stderr)
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("execution %s: %w", cfg.Action, err)
	}
	if client == nil {
		defer func() { _ = handle.Close() }()
		if cfg.Action != executionPauseResume {
			return fmt.Errorf("execution %s: no daemon is running on %s", cfg.Action, cfg.DBPath)
		}
		return resumeExecutionOffline(ctx, handle.store, stdout)
	}
	defer client.Close()
	cfg.DBPath = client.dbPath
	var status daemoncompose.ExecutionPauseStatus
	if err := client.call(ctx, executionPauseRoute, cfg, &status); err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(status); err != nil {
		return err
	}
	if cfg.Action != executionPauseRequest {
		return nil
	}
	return awaitExecutionPauseRelease(ctx, cfg, status, stdout, stderr)
}

// awaitExecutionPauseRelease polls prod until a prod without execution
// reports the pause released, printing each status that differs from the
// last. The prod that released the pause still answers, and may still hold
// the writer lock, while it tears down for its restart, so its own report of
// the release is not enough: the wait ends only once the restarted prod,
// which never takes the lock, answers. While no daemon answers, it waits for
// launchd to start prod again.
func awaitExecutionPauseRelease(
	ctx context.Context, cfg executionPauseCommandConfig, last daemoncompose.ExecutionPauseStatus,
	stdout, stderr io.Writer,
) error {
	ticker := time.NewTicker(executionPausePollInterval)
	defer ticker.Stop()
	waitingForRestart := false
	for {
		if last.Pause == nil {
			return errors.New("execution pause: the pause was cleared before prod released it")
		}
		if executionPauseHandedOver(last) {
			_, err := fmt.Fprintln(stderr, "execution pause: released; prod runs without execution")
			return err
		}
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintln(stderr, "execution pause: stopped waiting; prod still finishes the pause")
			return ctx.Err()
		case <-ticker.C:
		}
		status, err := readExecutionPauseStatus(ctx, cfg.DBPath)
		if err != nil {
			if !waitingForRestart {
				_, _ = fmt.Fprintln(stderr, "execution pause: prod is not answering; waiting for it to restart:", err)
				waitingForRestart = true
			}
			continue
		}
		waitingForRestart = false
		if !reflect.DeepEqual(status.Pause, last.Pause) || status.Executes != last.Executes ||
			!reflect.DeepEqual(status.Readiness.Blockers, last.Readiness.Blockers) {
			if err := json.NewEncoder(stdout).Encode(status); err != nil {
				return err
			}
		}
		last = status
	}
}

// executionPauseHandedOver reports that a prod that does not execute, and so
// holds no writer lock, reports the pause released.
func executionPauseHandedOver(status daemoncompose.ExecutionPauseStatus) bool {
	return status.Pause != nil && status.Pause.State == domain.ExecutionPauseReleased && !status.Executes
}

// readExecutionPauseStatus asks the daemon on dbPath for its status. It
// opens a fresh connection each time, since prod restarts during a pause.
func readExecutionPauseStatus(ctx context.Context, dbPath string) (daemoncompose.ExecutionPauseStatus, error) {
	access, err := openStoreOrControl(dbPath)
	if err != nil {
		return daemoncompose.ExecutionPauseStatus{}, err
	}
	defer func() { _ = access.Close() }()
	if access.client == nil {
		return daemoncompose.ExecutionPauseStatus{}, errors.New("no daemon is running")
	}
	var status daemoncompose.ExecutionPauseStatus
	err = access.client.call(ctx, executionPauseRoute,
		executionPauseCommandConfig{DBPath: access.client.dbPath, Action: executionPauseStatus}, &status)
	return status, err
}

// resumeExecutionOffline clears the pause in a store no daemon holds, so the
// next start runs prod's gated order.
func resumeExecutionOffline(ctx context.Context, st *store.Store, stdout io.Writer) error {
	setBy, err := settingsSetBy()
	if err != nil {
		return err
	}
	cleared, err := daemoncompose.ResumeExecution(ctx, st, setBy, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("execution resume: %w", err)
	}
	if cleared {
		_, err = fmt.Fprintln(stdout, "execution resume: cleared the pause; prod runs its gated order at its next start")
	} else {
		_, err = fmt.Fprintln(stdout, "execution resume: no pause was in force")
	}
	return err
}

// noteRigLease warns that a resumed prod comes back driverless while a
// production rig lease exists, live or stale.
func noteRigLease(stderr io.Writer) {
	root, err := daemonlock.DefaultRigLeaseRoot()
	if err != nil {
		return
	}
	if present, err := daemonlock.RigManifestPresent(root); err == nil && present {
		_, _ = fmt.Fprintln(stderr, "execution resume: a production rig lease exists, so prod comes back without "+
			"execution; once the deputy run ends, or after freesided rig recover for a stale lease, resume again")
	}
}

// registerExecutionPauseRoute serves freesided execution. control is nil in a
// prod started without a driver, which has no execution to pause.
func (p *pairingControl) registerExecutionPauseRoute(mux *http.ServeMux, control *daemoncompose.ExecutionPauseControl) {
	p.handle(mux, executionPauseRoute, func(ctx context.Context, body json.RawMessage) (any, error) {
		cfg, err := decodeControlPayload[executionPauseCommandConfig](body)
		if err != nil {
			return nil, err
		}
		return executionPauseInDaemon(ctx, control, cfg.Action)
	})
}

// executionPauseInDaemon applies action inside prod. The control socket's
// peer check limits callers to the daemon's account, so the account this
// process runs as is the operator who asked.
func executionPauseInDaemon(
	ctx context.Context, control *daemoncompose.ExecutionPauseControl, action executionPauseAction,
) (daemoncompose.ExecutionPauseStatus, error) {
	if !action.valid() {
		return daemoncompose.ExecutionPauseStatus{}, fmt.Errorf("execution: unknown action %q", action)
	}
	if control == nil {
		return daemoncompose.ExecutionPauseStatus{}, errors.New(
			"execution: this prod was started without -driver claude, so it has no execution to pause")
	}
	setBy, err := settingsSetBy()
	if err != nil {
		return daemoncompose.ExecutionPauseStatus{}, err
	}
	switch action {
	case executionPauseRequest:
		return control.Pause(ctx, setBy)
	case executionPauseResume:
		return control.Resume(ctx, setBy)
	case executionPauseStatus:
		return control.Status(ctx)
	}
	return daemoncompose.ExecutionPauseStatus{}, fmt.Errorf("execution: unknown action %q", action)
}
