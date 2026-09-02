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
	"strings"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func runReattemptMain(args []string) {
	cfg, err := parseReattemptCommand(args, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided reattempt:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	result, err := runReattemptCommand(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

func parseReattemptCommand(args []string, stderr io.Writer) (reattemptCommandConfig, error) {
	flags := flag.NewFlagSet("freesided reattempt", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	parentRun := flags.String("parent-run", "", "terminal campaign run to retry")
	campaign := flags.String("campaign", "", "campaign whose latest terminal run to retry")
	reason := flags.String("reason", "", "operator reason for the deliberate new attempt (required)")
	if err := flags.Parse(args); err != nil {
		return reattemptCommandConfig{}, err
	}
	if flags.NArg() != 0 {
		return reattemptCommandConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	cfg := reattemptCommandConfig{
		DBPath: *dbPath, ParentRunID: domain.RunID(*parentRun),
		CampaignID: domain.CampaignID(*campaign), Reason: *reason,
	}
	if err := validateReattemptConfig(cfg); err != nil {
		return reattemptCommandConfig{}, err
	}
	return cfg, nil
}

type reattemptCommandConfig struct {
	DBPath      string
	ParentRunID domain.RunID
	CampaignID  domain.CampaignID
	Reason      string
}

func runReattemptCommand(ctx context.Context, cfg reattemptCommandConfig) (submitResult, error) {
	if err := validateReattemptConfig(cfg); err != nil {
		return submitResult{}, fmt.Errorf("reattempt: %w", err)
	}
	st, _, err := openStoreWithTopicKey(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		return submitResult{}, fmt.Errorf("reattempt: open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	created, err := engine.ReattemptProductionRun(ctx, st, engine.ProductionReattemptSpec{
		ParentRunID: cfg.ParentRunID, CampaignID: cfg.CampaignID, Reason: cfg.Reason,
	})
	if err != nil {
		return submitResult{}, fmt.Errorf("reattempt: %w", err)
	}
	result := submitResult{
		RunID: created.Run.Run.ID, ProjectID: created.Run.Run.ProjectID,
		InvocationID: created.Run.InvocationID, StageID: created.Run.StageID,
		ImplementationRunID:           created.Run.Run.ID,
		ImplementationInvocationID:    created.Run.InvocationID,
		ImplementationStageID:         created.Run.StageID,
		SpecificationRunID:            created.Attempt.SpecificationRunID,
		SourceDigest:                  created.Attempt.SourceDigest,
		ApprovedSpecDigest:            created.Attempt.ApprovedSpecDigest,
		SourceArtifactID:              created.RootSourceArtifactID,
		SpecificationPolicyDigest:     created.PolicyDigest,
		SpecificationPolicyArtifactID: created.PolicyArtifactID,
		PublicationDigest:             created.Attempt.PublicationDigest,
		CampaignID:                    created.Attempt.CampaignID,
		AttemptNumber:                 created.Attempt.AttemptNumber,
		AttemptReason:                 created.Attempt.Reason,
		ParentRunID:                   created.Attempt.ParentRunID,
	}
	result.SpecificationInvocationID = created.SpecificationInvocationID
	result.SpecificationStageID = created.SpecificationStageID
	if created.HasWorkUnit {
		result.WorkUnitID = domain.WorkUnitIDForRun(created.Run.Run.ID)
	}
	return result, nil
}

func validateReattemptConfig(cfg reattemptCommandConfig) error {
	switch {
	case cfg.DBPath == "":
		return errors.New("-db is required")
	case (cfg.ParentRunID == "") == (cfg.CampaignID == ""):
		return errors.New("exactly one of -parent-run or -campaign is required")
	case cfg.Reason == "" || cfg.Reason != strings.TrimSpace(cfg.Reason):
		return errors.New("-reason must be non-empty and trimmed")
	default:
		return nil
	}
}
