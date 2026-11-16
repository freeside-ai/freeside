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
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// runCauseCountsMain is the operator entry point that prints how often each
// typed cause occurred over a period, and in the period before it (issue
// #3238). It is a read: no model runs and nothing is written.
func runCauseCountsMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runCauseCountsCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided cause-counts:", err)
		os.Exit(1)
	}
}

func runCauseCountsCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("freesided cause-counts", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "existing SQLite database path (required)")
	period := flags.Duration("period", 168*time.Hour, "length of the period counted")
	endFlag := flags.String("end", "", "end of the period, RFC 3339, exclusive (default now)")
	project := flags.String("project", "", "count only this project's causes (default every project)")
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
		return callCommand(ctx, client, "/cause-counts", args, stdout)
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
	rows, err := st.ReadCauseRows(ctx, projectID)
	if err != nil {
		return err
	}
	window := causecount.Window{Start: end.Add(-*period), End: end}
	report := causecount.Report{
		ProjectID:      reportProject,
		Period:         window,
		PreviousPeriod: window.Previous(),
		Causes:         causecount.Compute(toCauseOccurrences(rows), window),
		Skipped:        toCauseSkipped(rows.Skipped),
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		return fmt.Errorf("write cause counts: %w", err)
	}
	return nil
}

// toCauseOccurrences restates the store's per-table rows as the pure counting
// package's occurrences, naming each slice's source.
func toCauseOccurrences(rows store.CauseRows) []causecount.Occurrence {
	var out []causecount.Occurrence
	for _, source := range []struct {
		source causecount.Source
		rows   []store.CauseRow
	}{
		{causecount.SourceFailureCause, rows.FailureCauses},
		{causecount.SourceTerminal, rows.Terminals},
		{causecount.SourcePublicationBlocked, rows.PublicationBlocks},
		{causecount.SourceReviewFailure, rows.ReviewFailures},
		{causecount.SourceAttentionConclusion, rows.AttentionConclusions},
	} {
		for _, row := range source.rows {
			out = append(out, causecount.Occurrence{Source: source.source, Value: row.Value, RunID: row.RunID, At: row.At})
		}
	}
	return out
}

func toCauseSkipped(rows []store.SkippedCauseRow) []causecount.Skipped {
	out := make([]causecount.Skipped, len(rows))
	for i, row := range rows {
		out[i] = causecount.Skipped{Table: row.Table, Row: row.Key, Error: row.Error}
	}
	return out
}
