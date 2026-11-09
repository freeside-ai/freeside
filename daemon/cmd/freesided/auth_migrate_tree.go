package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// authMigrateTreeReport is the command's result. Patch is where the patch
// went, and Changed is false when the revision was already in the current
// layout, so the patch is empty and there is nothing to commit.
type authMigrateTreeReport struct {
	Patch          string        `json:"patch"`
	Changed        bool          `json:"changed"`
	LineupRevision domain.Digest `json:"lineup_revision"`
}

// runAuthMigrateTreeCommand prints the patch that moves a committed tree in
// the layout before #2599 (agent and offer documents) to the current one
// (agenttree.Migrate). Like adoption, it changes no tree: a human reviews and
// commits the patch, then hands the daemon the new commit. It reads only the
// checkout's object store, so it runs beside a running daemon.
func runAuthMigrateTreeCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("freesided auth migrate-tree", flag.ContinueOnError)
	flags.SetOutput(stderr)
	checkout := flags.String("agent-tree", "", "checkout holding policy/ (required)")
	commit := flags.String("agent-tree-commit", "", "exact commit of -agent-tree whose tree is migrated (required)")
	patchPath := flags.String("patch", "-", "file to write the tree patch to, outside any checkout; - is stdout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if *checkout == "" || *commit == "" {
		return errors.New("-agent-tree and -agent-tree-commit are required")
	}
	if *patchPath != "-" {
		if err := refusePatchInsideCheckout(*patchPath); err != nil {
			return err
		}
	}

	scratch, err := os.MkdirTemp("", "freesided-agent-tree-")
	if err != nil {
		return fmt.Errorf("agent tree scratch: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	old, err := agenttree.ReadCommit(ctx, *checkout, scratch, *commit)
	if err != nil {
		return fmt.Errorf("read agent tree: %w", err)
	}
	next, err := agenttree.Migrate(old)
	if err != nil {
		return fmt.Errorf("migrate agent tree at %s: %w", *commit, err)
	}
	patch, err := agenttree.Patch(old, next)
	if err != nil {
		return err
	}
	report := authMigrateTreeReport{Changed: len(patch) != 0}
	if report.LineupRevision, err = agenttree.Revision(next); err != nil {
		return err
	}

	// The patch owns stdout when it goes there, so it pipes into git apply;
	// the report then goes to stderr.
	reportOut := stdout
	if *patchPath == "-" {
		reportOut = stderr
		report.Patch = "stdout"
		if _, err := stdout.Write(patch); err != nil {
			return fmt.Errorf("write tree patch: %w", err)
		}
	} else {
		if err := os.WriteFile(*patchPath, patch, 0o600); err != nil {
			return fmt.Errorf("write tree patch: %w", err)
		}
		report.Patch = *patchPath
	}
	encoder := json.NewEncoder(reportOut)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write migration report: %w", err)
	}
	return nil
}
