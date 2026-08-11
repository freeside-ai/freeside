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

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// runEnrollCodexMain is the operator-owned bootstrap and revoked-chain
// recovery entry point.
func runEnrollCodexMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runEnrollCodexCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided enroll-codex:", err)
		os.Exit(1)
	}
}

func runEnrollCodexCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer,
) (err error) {
	return runEnrollCodexCommandWithRefresher(
		ctx, args, stdout, stderr, ward.NewCodexAuthHTTPRefresher(),
	)
}

func runEnrollCodexCommandWithRefresher(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	refresher ward.CodexAuthRefresher,
) (err error) {
	flags := flag.NewFlagSet("freesided enroll-codex", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	projectID := flags.String("project", "", "project id for the recovery attention item (required)")
	authIdentityID := flags.String("auth-identity", "", "Codex auth identity id (required)")
	inputRoot := flags.String("input-root", "", "private root containing the operator login (required)")
	inputFile := flags.String("input-file", "", "operator-generated auth.json under input-root (required)")
	authStoreRoot := flags.String("auth-store-root", "", "separate private root containing live Codex auth stores (required)")
	authStorePath := flags.String("auth-store", "", "live auth.json path under auth-store-root (required; immutable after initial enrollment)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	switch {
	case *dbPath == "":
		return errors.New("-db is required")
	case *projectID == "":
		return errors.New("-project is required")
	case *authIdentityID == "":
		return errors.New("-auth-identity is required")
	case *inputRoot == "":
		return errors.New("-input-root is required")
	case *inputFile == "":
		return errors.New("-input-file is required")
	case *authStoreRoot == "":
		return errors.New("-auth-store-root is required")
	case *authStorePath == "":
		return errors.New("-auth-store is required")
	}

	st, err := store.Open(ctx, *dbPath, store.Options{})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	adapters, err := wardstore.New(st)
	if err != nil {
		return err
	}
	result, err := ward.EnrollCodexAuth(ctx, ward.CodexAuthEnrollmentConfig{
		InputRoot: *inputRoot, InputFile: *inputFile,
		AuthStoreRoot: *authStoreRoot, AuthStorePath: *authStorePath,
		AuthIdentityID: domain.AuthIdentityID(*authIdentityID),
		ProjectID:      domain.ProjectID(*projectID),
		Journal:        adapters.Enrollment, AuthStoreLeaser: adapters.Leaser,
		AuthRefresher: refresher,
	})
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write enrollment result: %w", err)
	}
	return nil
}
