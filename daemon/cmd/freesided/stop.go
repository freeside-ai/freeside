package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// runStopMain implements `freesided stop -db <database> -task <id>` (issue
// #3063): it records the same Stop a client's stop_task command records,
// through the running daemon, so an operator or a session acting for one can
// stop a task without a client. The daemon's cancellation loop tears the
// work down and concludes the Stop as it does for a client's.
//
// It needs a running daemon and never opens the store itself: with no daemon,
// nothing would tear the work down until the next start, and `freesided
// abandon` already gives a task up offline.
func runStopMain(args []string) {
	cfg, err := parseStopCommand(args, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided stop:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	receipt, err := runStopCommand(ctx, cfg)
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(receipt)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

func parseStopCommand(args []string, stderr io.Writer) (stopCommandConfig, error) {
	flags := flag.NewFlagSet("freesided stop", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path of the running daemon (required)")
	task := flags.String("task", "", "task to stop (required)")
	if err := flags.Parse(args); err != nil {
		return stopCommandConfig{}, err
	}
	if flags.NArg() != 0 {
		return stopCommandConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	cfg := stopCommandConfig{DBPath: *dbPath, TaskID: domain.TaskID(*task)}
	if err := validateStopConfig(cfg); err != nil {
		return stopCommandConfig{}, err
	}
	return cfg, nil
}

// stopCommandConfig is also the /tasks/stop control payload.
type stopCommandConfig struct {
	DBPath string        `json:"db_path"`
	TaskID domain.TaskID `json:"task_id"`
}

func validateStopConfig(cfg stopCommandConfig) error {
	switch {
	case cfg.DBPath == "":
		return errors.New("-db is required")
	case cfg.TaskID == "":
		return errors.New("-task is required")
	default:
		return nil
	}
}

// runStopCommand forwards the Stop to the daemon that holds cfg.DBPath.
func runStopCommand(ctx context.Context, cfg stopCommandConfig) (domain.StopTaskReceipt, error) {
	if err := validateStopConfig(cfg); err != nil {
		return domain.StopTaskReceipt{}, fmt.Errorf("stop: %w", err)
	}
	access, err := openStoreOrControl(cfg.DBPath)
	if err != nil {
		return domain.StopTaskReceipt{}, fmt.Errorf("stop: %w", err)
	}
	defer func() { _ = access.Close() }()
	if access.client == nil {
		// The lock was free, so no daemon runs; release it unopened.
		return domain.StopTaskReceipt{}, fmt.Errorf(
			"stop: no daemon is running on %s; start the daemon, or give the task up with freesided abandon", cfg.DBPath)
	}
	cfg.DBPath = access.client.dbPath
	var receipt domain.StopTaskReceipt
	if err := access.client.call(ctx, "/tasks/stop", cfg, &receipt); err != nil {
		return domain.StopTaskReceipt{}, err
	}
	return receipt, nil
}

// stopTaskInDaemon records the Stop inside the running daemon under the
// engine's launch guard, so it is ordered against invocation launch exactly
// as a client's Stop is.
//
// It skips the client path's active-device gate: no paired device sent it,
// and the control socket's peer check already limits callers to the daemon's
// account, which the receipt names as cli:<account>. It refuses a task that
// is already final, which the client's Stop does not check, so an operator
// learns there was nothing to stop instead of getting a receipt.
func stopTaskInDaemon(ctx context.Context, daemon daemonStoreContext, taskID domain.TaskID) (domain.StopTaskReceipt, error) {
	account, err := user.Current()
	if err != nil {
		return domain.StopTaskReceipt{}, fmt.Errorf("stop: read account: %w", err)
	}
	device := domain.DeviceID("cli:" + account.Username)
	var receipt domain.StopTaskReceipt
	commit := func() error {
		return daemon.store.Write(ctx, func(tx *store.WriteTx) error {
			task, err := tx.GetTask(ctx, taskID)
			if err != nil {
				return fmt.Errorf("stop: task %q: %w", taskID, err)
			}
			if lifecycle := task.DisplayLifecycle(nil); lifecycle != nil {
				return fmt.Errorf("stop: task %q is already %s; nothing to stop", taskID, *lifecycle)
			}
			// Reading the binding in the same write keeps it from going stale.
			state, err := tx.ServerState(ctx)
			if err != nil {
				return err
			}
			receipt, _, err = tx.StopTask(ctx, domain.StopTaskRequest{
				CommandID: "cli-stop-" + rand.Text(), DeviceID: device,
				TaskID: task.ID, ProjectID: task.ProjectID,
				ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
			}, time.Now().UTC())
			if err != nil {
				return fmt.Errorf("stop: task %q: %w", taskID, err)
			}
			return nil
		})
	}
	if err := daemon.commitTaskStop(ctx, taskID, commit); err != nil {
		return domain.StopTaskReceipt{}, err
	}
	return receipt, nil
}
