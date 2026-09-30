package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type authListOutput struct {
	Identities []authListIdentity `json:"identities"`
}

type authListIdentity struct {
	ID       domain.AuthIdentityID `json:"id"`
	Provider string                `json:"provider"`
	// Label is the masked account binding. It is computed here, at render
	// time, because the account probe plan §5.4 names as its source (#868)
	// does not exist yet; it is never stored and never enters evidence.
	Label                 string               `json:"label"`
	Enabled               bool                 `json:"enabled"`
	CostOwner             string               `json:"cost_owner"`
	UsagePool             string               `json:"usage_pool"`
	MaxParallelExecutions int                  `json:"max_parallel_executions"`
	Interim               bool                 `json:"interim"`
	Enrollments           []authListEnrollment `json:"enrollments"`
}

type authListEnrollment struct {
	ID              domain.ClientEnrollmentID `json:"id"`
	HarnessClient   domain.HarnessClientKind  `json:"harness_client"`
	Route           string                    `json:"route"`
	AuthMethod      domain.AuthMethod         `json:"auth_method"`
	CredentialMode  domain.CredentialMode     `json:"credential_mode"`
	RefreshStrategy domain.RefreshStrategy    `json:"refresh_strategy"`
	// Generation, TokenExpiry, and RecordedAt describe the current store
	// generation; all are null for an enrollment whose bootstrap never
	// recorded one.
	Generation  *int       `json:"generation"`
	TokenExpiry *time.Time `json:"token_expiry"`
	RecordedAt  *time.Time `json:"recorded_at"`
}

// runAuthListCommand prints every identity with its enrollments. It reads
// through the running daemon's control socket when one holds the store.
func runAuthListCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("freesided auth list", flag.ContinueOnError)
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
	handle, client, err := openCommandStore(ctx, *dbPath, store.Options{}, storeExisting)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if client != nil {
		return callCommand(ctx, client, "/auth/list", args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	var output authListOutput
	if err := handle.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		output, err = readAuthList(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(output); err != nil {
		return fmt.Errorf("write auth list: %w", err)
	}
	return nil
}

func readAuthList(ctx context.Context, tx *store.ReadTx) (authListOutput, error) {
	identities, err := tx.ListAuthIdentities(ctx)
	if err != nil {
		return authListOutput{}, err
	}
	output := authListOutput{Identities: make([]authListIdentity, 0, len(identities))}
	for _, identity := range identities {
		enrollments, err := tx.ListClientEnrollments(ctx, identity.ID)
		if err != nil {
			return authListOutput{}, err
		}
		listed := authListIdentity{
			ID: identity.ID, Provider: identity.Provider,
			Label:   maskAccountBinding(identity.AccountBinding),
			Enabled: identity.Enabled, CostOwner: identity.CostOwner, UsagePool: identity.UsagePool,
			MaxParallelExecutions: identity.MaxParallelExecutions,
			Interim:               identity.Interim.Present(),
			Enrollments:           make([]authListEnrollment, 0, len(enrollments)),
		}
		for _, enrollment := range enrollments {
			entry := authListEnrollment{
				ID: enrollment.ID, HarnessClient: enrollment.HarnessClient, Route: enrollment.Route,
				AuthMethod: enrollment.AuthMethod, CredentialMode: enrollment.CredentialMode,
				RefreshStrategy: enrollment.RefreshStrategy,
			}
			generation, err := tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
			switch {
			case err == nil:
				entry.Generation = &generation.Ordinal
				entry.TokenExpiry = generation.TokenExpiry
				entry.RecordedAt = &generation.RecordedAt
			case !errors.Is(err, store.ErrNotFound):
				return authListOutput{}, err
			}
			listed.Enrollments = append(listed.Enrollments, entry)
		}
		output.Identities = append(output.Identities, listed)
	}
	return output, nil
}

// maskAccountBinding keeps the last four characters of a binding long enough
// that four characters are a small part of it, behind a fixed-width mask so
// the label does not reveal the binding's length. A shorter binding masks
// entirely, and an unbound identity has no label.
func maskAccountBinding(binding string) string {
	const mask = "****"
	runes := []rune(binding)
	switch {
	case len(runes) == 0:
		return ""
	case len(runes) <= 8:
		return mask
	}
	return mask + string(runes[len(runes)-4:])
}
