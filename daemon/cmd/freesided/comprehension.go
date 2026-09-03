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

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe/comprehension"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// runComprehensionMain is the operator entry point for the §9 comprehension
// signals the wave-10 exit evaluation reads. With no subcommand it prints the
// measures as JSON; `record-defect` records one operator-found defect.
func runComprehensionMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runComprehensionCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided comprehension:", err)
		os.Exit(1)
	}
}

func runComprehensionCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "record-defect" {
		return runComprehensionRecordDefect(ctx, args[1:], stdout, stderr)
	}
	return runComprehensionMeasures(ctx, args, stdout, stderr)
}

func runComprehensionMeasures(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("freesided comprehension", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "existing SQLite database path (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if *dbPath == "" {
		return errors.New("-db is required")
	}
	st, err := store.OpenExisting(ctx, *dbPath, store.Options{})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { err = errors.Join(err, st.Close()) }()

	var (
		events   []domain.ComprehensionEvent
		surfaces []domain.DecisionActionSurface
		decided  []store.DecidedCommand
		defects  []domain.ComprehensionDefect
	)
	if err := st.ReadComprehension(ctx, func(tx *store.ComprehensionReadTx) error {
		var err error
		if events, err = tx.ListComprehensionEvents(ctx); err != nil {
			return err
		}
		if surfaces, err = tx.ListDecisionActionSurfaces(ctx); err != nil {
			return err
		}
		if decided, err = tx.ListDecidedCommands(ctx); err != nil {
			return err
		}
		defects, err = tx.ListComprehensionDefects(ctx)
		return err
	}); err != nil {
		return fmt.Errorf("read comprehension telemetry: %w", err)
	}
	measures := comprehension.Compute(events, surfaces, toMeasureCommands(decided), defects)
	if err := json.NewEncoder(stdout).Encode(measures); err != nil {
		return fmt.Errorf("write comprehension measures: %w", err)
	}
	return nil
}

// toMeasureCommands restates the store projection in the pure measures
// package's domain-only terms.
func toMeasureCommands(rows []store.DecidedCommand) []comprehension.DecidedCommand {
	out := make([]comprehension.DecidedCommand, len(rows))
	for i, row := range rows {
		out[i] = comprehension.DecidedCommand{
			Command: row.Command, ItemType: row.ItemType,
			DecidedAt: row.DecidedAt, SubjectRunID: row.SubjectRunID,
		}
	}
	return out
}

func runComprehensionRecordDefect(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("freesided comprehension record-defect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "existing SQLite database path (required)")
	item := flags.String("item", "", "attention item id (required)")
	claim := flags.String("claim", "", "claim content-address digest the defect concerns (required)")
	reason := flags.String("reason", "", "short defect reason (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	switch {
	case *dbPath == "":
		return errors.New("-db is required")
	case *item == "":
		return errors.New("-item is required")
	case !contentaddr.Valid(*claim):
		return errors.New("-claim must be a canonical sha256 digest")
	case *reason == "":
		return errors.New("-reason is required")
	}
	defect := domain.ComprehensionDefect{
		ItemID: domain.ItemID(*item), ClaimDigest: domain.Digest(*claim),
		RecordedAt: time.Now().UTC(), Reason: *reason,
	}
	st, err := store.OpenExisting(ctx, *dbPath, store.Options{})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordComprehensionDefect(ctx, defect)
	}); err != nil {
		return fmt.Errorf("record comprehension defect: %w", err)
	}
	if err := json.NewEncoder(stdout).Encode(defect); err != nil {
		return fmt.Errorf("write comprehension defect: %w", err)
	}
	return nil
}
