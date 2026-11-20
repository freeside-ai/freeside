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
	"github.com/freeside-ai/freeside/daemon/internal/observe/causecount"
	"github.com/freeside-ai/freeside/daemon/internal/observe/readyshare"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// runReadyShareMain is the operator entry point that prints how many tasks
// reached a ready pull request over a period without raising an attention
// item, and which items the others raised first (issue #3406). It is a read:
// no model runs and nothing is written.
func runReadyShareMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runReadyShareCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided ready-share:", err)
		os.Exit(1)
	}
}

func runReadyShareCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("freesided ready-share", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "existing SQLite database path (required)")
	period := flags.Duration("period", 168*time.Hour, "length of the period counted")
	endFlag := flags.String("end", "", "end of the period, RFC 3339, exclusive (default now)")
	project := flags.String("project", "", "count only this project's items (default every project)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if *dbPath == "" {
		return errors.New("-db is required")
	}
	if *period <= 0 {
		return errors.New("-period must be positive")
	}
	end := time.Now().UTC()
	if *endFlag != "" {
		parsed, err := time.Parse(time.RFC3339, *endFlag)
		if err != nil {
			return fmt.Errorf("-end: %w", err)
		}
		end = parsed.UTC()
	}

	handle, client, err := openCommandStore(ctx, *dbPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		// The daemon reruns these arguments, so a default -end is its now.
		return callCommand(ctx, client, "/ready-share", args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	st := handle.store

	projectID := domain.ProjectID(*project)
	var reportProject *domain.ProjectID
	if projectID != "" {
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetProject(ctx, projectID)
			return err
		}); errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("unknown project %q: %w", projectID, store.ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("read project: %w", err)
		}
		reportProject = &projectID
	}
	rows, err := st.ReadReadyShareRows(ctx, projectID)
	if err != nil {
		return err
	}
	report := readyshare.Compute(toReadyShareItems(rows.Items), causecount.Window{Start: end.Add(-*period), End: end})
	report.ProjectID = reportProject
	report.Skipped = toCauseSkipped(rows.Skipped)
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		return fmt.Errorf("write ready share: %w", err)
	}
	return nil
}

func toReadyShareItems(rows []store.ReadyShareRow) []readyshare.Item {
	out := make([]readyshare.Item, len(rows))
	for i, row := range rows {
		out[i] = readyshare.Item{
			ItemID: row.ItemID, TaskID: row.TaskID, Type: row.Type, Class: row.Class, CreatedAt: row.CreatedAt,
		}
	}
	return out
}
