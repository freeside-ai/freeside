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
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func runTaskLineMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runTaskLineCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided task-line:", err)
		os.Exit(1)
	}
}

type taskLineConfig struct {
	DBPath  string
	Request engine.ChangeTaskLineRequest
}

// taskLineResult reports the change. Recorded is false when the role already
// had the requested line, so nothing was written. Line is the role's newest
// version after the request, null when the role has never had a line.
type taskLineResult struct {
	TaskID   domain.TaskID    `json:"task_id"`
	Role     domain.RoleName  `json:"role"`
	Recorded bool             `json:"recorded"`
	Line     *domain.TaskLine `json:"line"`
}

// runTaskLineCommand changes or clears a task's line for a role while no
// attempt of that role runs. The change applies to attempts admitted after it
// commits, so a running daemon needs no restart; an admission that read the
// old line before the commit may still run on it.
func runTaskLineCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	cfg, err := parseTaskLineConfig(args, stderr)
	if err != nil {
		return err
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, "/tasks/lines", args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	changed, err := engine.ChangeTaskLine(ctx, handle.store, cfg.Request, time.Now().UTC())
	if err != nil {
		return err
	}
	result := taskLineResult{
		TaskID: cfg.Request.TaskID, Role: cfg.Request.Role, Recorded: changed.Recorded, Line: changed.Line,
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write task line result: %w", err)
	}
	return nil
}

// parseTaskLineConfig checks only that the flags form one request. The role
// and agent are refused by the engine, whose errors never repeat them.
func parseTaskLineConfig(args []string, output io.Writer) (taskLineConfig, error) {
	var (
		cfg        taskLineConfig
		task, role string
	)
	flags := flag.NewFlagSet("freesided task-line", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DBPath, "db", "", "existing SQLite database path (required)")
	flags.StringVar(&task, "task", "", "task id (required)")
	flags.StringVar(&role, "role", "", "specifier, implementer, remediator, or reviewer (required)")
	flags.StringVar(&cfg.Request.Agent, "agent", "", "agent to move the role to; exactly one of -agent and -clear")
	flags.BoolVar(&cfg.Request.Clear, "clear", false, "return the role to the lineup; exactly one of -agent and -clear")
	if err := flags.Parse(args); err != nil {
		return taskLineConfig{}, err
	}
	if flags.NArg() != 0 {
		return taskLineConfig{}, errors.New("unexpected positional arguments")
	}
	switch {
	case cfg.DBPath == "":
		return taskLineConfig{}, errors.New("-db is required")
	case task == "":
		return taskLineConfig{}, errors.New("-task is required")
	case role == "":
		return taskLineConfig{}, errors.New("-role is required")
	case (cfg.Request.Agent != "") == cfg.Request.Clear:
		return taskLineConfig{}, errors.New("exactly one of -agent and -clear is required")
	}
	cfg.Request.TaskID, cfg.Request.Role = domain.TaskID(task), domain.RoleName(role)
	return cfg, nil
}
