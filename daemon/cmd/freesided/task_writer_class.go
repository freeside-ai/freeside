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

func runTaskWriterClassMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runTaskWriterClassCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided task-writer-class:", err)
		os.Exit(1)
	}
}

type taskWriterClassConfig struct {
	DBPath string
	TaskID domain.TaskID
	Class  domain.RunnerBackendClass
	Clear  bool
}

// taskWriterClassResult reports the task's requirement after the command:
// WriterClass is null once it is cleared. Removed is true when -clear
// removed a requirement the task had.
type taskWriterClassResult struct {
	TaskID      domain.TaskID           `json:"task_id"`
	WriterClass *domain.TaskWriterClass `json:"writer_class"`
	Removed     bool                    `json:"removed"`
}

// runTaskWriterClassCommand sets or clears a task's requirement that its
// writers run warded (#3471). The store accepts only the warded class and
// refuses a task that doesn't exist. A writer admission reads the
// requirement when it admits, so a running daemon needs no restart; what a
// change after the task's first writer admission does is #3472's rule.
func runTaskWriterClassCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	cfg, err := parseTaskWriterClassConfig(args, stderr)
	if err != nil {
		return err
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, taskWriterClassRoute, args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	result := taskWriterClassResult{TaskID: cfg.TaskID}
	if cfg.Clear {
		err = handle.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			var err error
			result.Removed, err = tx.ClearTaskWriterClass(ctx, cfg.TaskID)
			return err
		})
	} else {
		setBy, setErr := settingsSetBy()
		if setErr != nil {
			return setErr
		}
		requirement := domain.TaskWriterClass{TaskID: cfg.TaskID, Class: cfg.Class, SetBy: setBy, SetAt: time.Now().UTC()}
		err = handle.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.SetTaskWriterClass(ctx, requirement)
		})
		result.WriterClass = &requirement
	}
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write task writer class result: %w", err)
	}
	return nil
}

const taskWriterClassRoute = "/tasks/writer-class"

// parseTaskWriterClassConfig checks only that the flags form one request.
// The class is refused by the store, which accepts only the warded class.
func parseTaskWriterClassConfig(args []string, output io.Writer) (taskWriterClassConfig, error) {
	var (
		cfg         taskWriterClassConfig
		task, class string
	)
	flags := flag.NewFlagSet("freesided task-writer-class", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DBPath, "db", "", "existing SQLite database path (required)")
	flags.StringVar(&task, "task", "", "task id (required)")
	flags.StringVar(&class, "class", "",
		"the class the task's writers must run on: only "+string(domain.BackendFreshVMReadOnlyVolumeHandoff)+"; or -clear")
	flags.BoolVar(&cfg.Clear, "clear", false, "remove the requirement, so the task's writers take the project's class")
	if err := flags.Parse(args); err != nil {
		return taskWriterClassConfig{}, err
	}
	if flags.NArg() != 0 {
		return taskWriterClassConfig{}, errors.New("unexpected positional arguments")
	}
	switch {
	case cfg.DBPath == "":
		return taskWriterClassConfig{}, errors.New("-db is required")
	case task == "":
		return taskWriterClassConfig{}, errors.New("-task is required")
	case (class != "") == cfg.Clear:
		return taskWriterClassConfig{}, errors.New("exactly one of -class and -clear is required")
	}
	cfg.TaskID, cfg.Class = domain.TaskID(task), domain.RunnerBackendClass(class)
	return cfg, nil
}
