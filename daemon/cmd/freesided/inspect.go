package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// inspectIndexVersion names the index's JSON shape; a shape change changes it.
const inspectIndexVersion = "freeside-inspect-index-v1"

// inspectIndex is what `freesided inspect` prints: the machine-readable
// reads freesided offers and the tasks the store holds, so an agent can reach
// a task's record without reading source.
type inspectIndex struct {
	Version      string        `json:"version"`
	AsOfRevision int64         `json:"as_of_revision"`
	Reads        []inspectRead `json:"reads"`
	Tasks        []inspectTask `json:"tasks"`
}

type inspectRead struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	IDs     []string `json:"ids"`
	// Version is the read's shape version, nil when its JSON carries none.
	Version *string `json:"version"`
}

type inspectTask struct {
	TaskID      domain.TaskID    `json:"task_id"`
	ProjectID   domain.ProjectID `json:"project_id"`
	CreatedAt   time.Time        `json:"created_at"`
	LatestRunID *domain.RunID    `json:"latest_run_id"`
}

// inspectReads is the one list of freesided's machine-readable reads: the
// commands that print what the store holds as JSON and write nothing. A new
// read is registered here, or the index does not know it exists.
func inspectReads() []inspectRead {
	index, record := inspectIndexVersion, taskRecordVersion
	return []inspectRead{
		{Name: "inspect", Command: "freesided inspect -db PATH", IDs: []string{}, Version: &index},
		{
			Name: "inspect task", Command: "freesided inspect task -db PATH -task TASK_ID",
			IDs: []string{"task_id"}, Version: &record,
		},
		{
			Name: "follow -snapshot", Command: "freesided follow -db PATH -run RUN_ID -snapshot",
			IDs: []string{"run_id"},
		},
		{Name: "comprehension", Command: "freesided comprehension -db PATH", IDs: []string{}},
		{Name: "auth list", Command: "freesided auth list -db PATH", IDs: []string{}},
	}
}

func runInspectMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err := runInspectCommand(ctx, args, os.Stdout, os.Stderr)
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "freesided inspect:", err)
	stop()
	if errors.Is(err, observe.ErrUsage) {
		os.Exit(2)
	}
	os.Exit(1)
}

// runInspectCommand prints the index, or with the `task` verb one task's
// record, as one JSON line. Both are reads: they write nothing and open an
// unheld database without migrating it.
func runInspectCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	verb := "freesided inspect"
	wantTask := len(args) > 0 && args[0] == "task"
	if wantTask {
		verb, args = "freesided inspect task", args[1:]
	}
	flags := flag.NewFlagSet(verb, flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	approvedRecipes := digestSetFlag{}
	flags.Var(&approvedRecipes, "approved-recipe",
		"approved verification-recipe digest, used only when no daemon holds the database (repeatable)")
	taskID := new(string)
	if wantTask {
		taskID = flags.String("task", "", "task id (required)")
	}
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", observe.ErrUsage, err)
	}
	switch {
	case flags.NArg() != 0:
		return fmt.Errorf("%w: unexpected positional arguments: %v", observe.ErrUsage, flags.Args())
	case *dbPath == "":
		return fmt.Errorf("%w: -db is required", observe.ErrUsage)
	case wantTask && *taskID == "":
		return fmt.Errorf("%w: -task is required", observe.ErrUsage)
	}
	route, read := "/inspect", func(ctx context.Context, st *store.Store) (any, error) {
		return readInspectIndexFrom(ctx, st)
	}
	if wantTask {
		route, read = "/tasks/"+url.PathEscape(*taskID)+"/record", func(ctx context.Context, st *store.Store) (any, error) {
			return readTaskRecordFrom(ctx, st, domain.TaskID(*taskID))
		}
	}
	// storeExisting: a read never migrates a database.
	handle, client, err := openCommandStore(ctx, *dbPath, store.Options{ApprovedRecipes: approvedRecipes}, storeExisting)
	if err != nil {
		return err
	}
	if client != nil {
		defer client.Close()
		// Print the daemon's bytes instead of decoding and re-encoding them,
		// so both paths print what one encoder wrote.
		var body json.RawMessage
		if err := client.get(ctx, route, nil, &body); err != nil {
			return err
		}
		_, err := stdout.Write(append(body, '\n'))
		return err
	}
	value, readErr := read(ctx, handle.store)
	if err := errors.Join(readErr, handle.Close()); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(value)
}

func readTaskRecordFrom(ctx context.Context, st *store.Store, id domain.TaskID) (taskRecord, error) {
	var record taskRecord
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = readTaskRecord(ctx, tx, id, time.Now().UTC())
		return err
	})
	return record, err
}

func readInspectIndexFrom(ctx context.Context, st *store.Store) (inspectIndex, error) {
	var index inspectIndex
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		index, err = readInspectIndex(ctx, tx)
		return err
	})
	return index, err
}

func readInspectIndex(ctx context.Context, tx *store.ReadTx) (inspectIndex, error) {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return inspectIndex{}, err
	}
	tasks, err := tx.ListTasks(ctx)
	if err != nil {
		return inspectIndex{}, fmt.Errorf("list tasks: %w", err)
	}
	index := inspectIndex{
		Version: inspectIndexVersion, AsOfRevision: state.Revision,
		Reads: inspectReads(), Tasks: make([]inspectTask, 0, len(tasks)),
	}
	for _, snapshot := range tasks {
		task := snapshot.Value
		runIDs, err := tx.TaskRunIDs(ctx, task.ID)
		if err != nil {
			return inspectIndex{}, fmt.Errorf("task %q runs: %w", task.ID, err)
		}
		entry := inspectTask{TaskID: task.ID, ProjectID: task.ProjectID, CreatedAt: task.CreatedAt}
		if len(runIDs) > 0 {
			entry.LatestRunID = &runIDs[len(runIDs)-1]
		}
		index.Tasks = append(index.Tasks, entry)
	}
	return index, nil
}
