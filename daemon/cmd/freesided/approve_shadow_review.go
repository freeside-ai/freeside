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
	"slices"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func runApproveShadowReviewMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runApproveShadowReviewCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided approve-shadow-review:", err)
		os.Exit(1)
	}
}

type approveShadowReviewConfig struct {
	Repository          string
	DBPath              string
	Source              domain.ShadowReviewSource
	ConfigurationDigest domain.Digest
	ApprovalDigest      domain.Digest
}

func runApproveShadowReviewCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer,
) (err error) {
	cfg, err := parseApproveShadowReviewConfig(args, stderr)
	if err != nil {
		return err
	}
	st, err := store.OpenExisting(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	result, err := (operations.ShadowReviewConfigurationApprover{
		Store: st, Now: time.Now,
	}).Run(ctx, operations.ShadowReviewConfigurationApprovalRequest{
		Repository: cfg.Repository, Source: cfg.Source,
		ConfigurationDigest: cfg.ConfigurationDigest,
		ApprovalDigest:      cfg.ApprovalDigest,
	})
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write shadow review configuration approval result: %w", err)
	}
	return nil
}

func parseApproveShadowReviewConfig(
	args []string, output io.Writer,
) (approveShadowReviewConfig, error) {
	if len(args) == 0 {
		return approveShadowReviewConfig{}, errors.New("repository owner/name is required")
	}
	cfg := approveShadowReviewConfig{Repository: args[0]}
	flags := flag.NewFlagSet("freesided approve-shadow-review", flag.ContinueOnError)
	flags.SetOutput(output)
	var source, configurationDigest, approvalDigest string
	flags.StringVar(&cfg.DBPath, "db", "", "existing SQLite database path (required)")
	flags.StringVar(&source, "source", "", "registered shadow review source (required)")
	flags.StringVar(&configurationDigest, "configuration-digest", "",
		"exact effective shadow review configuration digest (required)")
	flags.StringVar(&approvalDigest, "approve", "",
		"exact proposed approval digest; omit for the review pass")
	if err := flags.Parse(args[1:]); err != nil {
		return approveShadowReviewConfig{}, err
	}
	if flags.NArg() != 0 {
		return approveShadowReviewConfig{},
			fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if cfg.DBPath == "" {
		return approveShadowReviewConfig{}, errors.New("-db is required")
	}
	cfg.Source = domain.ShadowReviewSource(source)
	if !slices.Contains(domain.AllShadowReviewSources, cfg.Source) {
		return approveShadowReviewConfig{}, fmt.Errorf(
			"-source %q is invalid; valid values: %v", source, domain.AllShadowReviewSources,
		)
	}
	if !contentaddr.Valid(configurationDigest) {
		return approveShadowReviewConfig{},
			errors.New("-configuration-digest must be a canonical sha256 digest")
	}
	cfg.ConfigurationDigest = domain.Digest(configurationDigest)
	if approvalDigest != "" && !contentaddr.Valid(approvalDigest) {
		return approveShadowReviewConfig{},
			errors.New("-approve must be a canonical sha256 digest")
	}
	cfg.ApprovalDigest = domain.Digest(approvalDigest)
	return cfg, nil
}
