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
	"github.com/freeside-ai/freeside/daemon/internal/topicstore"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// runRenewCodexMain renews an existing subscription without enrolling it.
func runRenewCodexMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runRenewCodexCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided renew-codex:", err)
		os.Exit(1)
	}
}

func runRenewCodexCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer,
) (err error) {
	deps := productionAuthAddDeps()
	return runRenewCodexCommandWithDeps(ctx, args, stdout, stderr, ward.NewCodexAuthHTTPRefresher(),
		deps.runtime, deps.codexModelDiscovery)
}

func runRenewCodexCommandWithRefresher(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	refresher ward.CodexAuthRefresher,
) (err error) {
	deps := productionAuthAddDeps()
	return runRenewCodexCommandWithDeps(ctx, args, stdout, stderr, refresher, deps.runtime, deps.codexModelDiscovery)
}

// runRenewCodexCommandWithDeps is renew-codex with its token endpoint,
// container runtime, and model discovery replaceable by tests.
func runRenewCodexCommandWithDeps(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	refresher ward.CodexAuthRefresher,
	runtime func(containerBin string) ward.Runtime,
	discovery func(ward.Runtime) codexModelDiscoverer,
) (err error) {
	flags := flag.NewFlagSet("freesided renew-codex", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	authIdentityID := flags.String("auth-identity", "", "Codex auth identity id (required)")
	authStoreRoot := flags.String("auth-store-root", "", "separate private root containing live Codex auth stores (required)")
	authStorePath := flags.String("auth-store", "", "live auth.json path under auth-store-root (required; immutable after initial enrollment)")
	agentImage := flags.String("agent-image", "", "optional digest-pinned Codex image model discovery runs in")
	containerBin := flags.String("container-bin", "container", "Apple container CLI path")
	approvedRecipes := digestSetFlag{}
	flags.Var(&approvedRecipes, "approved-recipe",
		"approved verification-recipe digest (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	switch {
	case *dbPath == "":
		return errors.New("-db is required")
	case *authIdentityID == "":
		return errors.New("-auth-identity is required")
	case *authStoreRoot == "":
		return errors.New("-auth-store-root is required")
	case *authStorePath == "":
		return errors.New("-auth-store is required")
	case *agentImage != "" && domain.ImageRef(*agentImage).Validate() != nil:
		return errors.New("-agent-image must be digest-pinned")
	}

	lock, err := requireNoDaemon("renew-codex", *dbPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := topicstore.InspectKey(*dbPath); err != nil {
		return fmt.Errorf("inspect store topic key: %w", err)
	}
	st, err := store.OpenExisting(
		ctx, *dbPath, store.Options{ApprovedRecipes: approvedRecipes},
	)
	if err != nil {
		return fmt.Errorf("open existing store without migration (use a schema-compatible daemon or the supported runtime upgrade): %w", err)
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	adapters, err := wardstore.New(st)
	if err != nil {
		return err
	}
	result, err := ward.RenewCodexAuth(ctx, ward.CodexAuthRenewalConfig{
		AuthStoreRoot: *authStoreRoot, AuthStorePath: *authStorePath,
		AuthIdentityID: domain.AuthIdentityID(*authIdentityID),
		AuthState:      adapters.AuthState, AuthStoreLeaser: adapters.Leaser,
		AuthRefresher:               refresher,
		AccessTokenLifetimeFloor:    ward.CodexAuthProductionLifetimeFloor,
		AccessTokenRefreshThreshold: ward.CodexAuthProductionRefreshThreshold,
	})
	if err != nil {
		if errors.Is(err, domain.ErrUnapprovedRecipe) {
			return fmt.Errorf(
				"the store contains recipe-gated evidence; pass each approved "+
					"verification-recipe digest with -approved-recipe: %w", err)
		}
		return err
	}
	enrollments, err := codexOAuthEnrollments(ctx, st, result.AuthIdentityID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: model discovery skipped: enrollments not read: %v\n", err)
	} else {
		discoverCodexAfterAuthChange(ctx, st, runtime, discovery, *containerBin, *agentImage,
			*authStoreRoot, result.AuthStorePath, enrollments, stderr)
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write renewal result: %w", err)
	}
	return nil
}

// codexOAuthEnrollments lists an identity's Codex CLI OAuth enrollments,
// whose catalogs a renewal refreshes.
func codexOAuthEnrollments(
	ctx context.Context, st *store.Store, identity domain.AuthIdentityID,
) ([]domain.ClientEnrollmentID, error) {
	var ids []domain.ClientEnrollmentID
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		enrollments, err := tx.ListClientEnrollments(ctx, identity)
		for _, enrollment := range enrollments {
			if enrollment.HarnessClient == domain.HarnessClientCodexCLI && enrollment.AuthMethod == domain.AuthMethodOAuth {
				ids = append(ids, enrollment.ID)
			}
		}
		return err
	})
	return ids, err
}
