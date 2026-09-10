package main

import (
	"encoding/json"
	"errors"
	"flag"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

type judgmentConfig struct {
	CLI            claudeinference.Config `json:"cli"`
	AuthSnapshot   string                 `json:"auth_snapshot"`
	ExpectedDigest string                 `json:"-"`
}

func judgmentFlags(flags *flag.FlagSet, cfg *judgmentConfig) {
	flags.StringVar(&cfg.CLI.Binary, "judgment-claude-bin", "", "absolute native Claude CLI path for subscription judgments (optional)")
	flags.StringVar(&cfg.CLI.SHA256, "judgment-claude-sha256", "", "exact Claude CLI executable digest")
	flags.StringVar(&cfg.CLI.Model, "judgment-model", "", "explicit Claude model for classifier and adjudicator")
	flags.StringVar(&cfg.AuthSnapshot, "judgment-auth-snapshot", "", "existing Claude setup-token snapshot relative to review-input-root")
}

// composeRuntimeJudgments binds startup to the successful preflight evidence.
func composeRuntimeJudgments(cfg judgmentConfig, inputRoot string) (inference.Binding, error) {
	expected := cfg.ExpectedDigest
	cfg.ExpectedDigest = ""
	if cfg != (judgmentConfig{}) && !contentaddr.Valid(expected) {
		return inference.Binding{}, errors.New("judgment startup requires a preflight configuration digest")
	}
	binding, actual, err := composeJudgments(cfg, inputRoot)
	if err != nil {
		return inference.Binding{}, err
	}
	if actual != expected {
		return inference.Binding{}, errors.New("judgment configuration differs from preflight")
	}
	return binding, nil
}

// composeJudgments reuses the private setup-token reader used by shadow review.
// No new credential enrollment, refresh mechanism, or provider API is involved.
func composeJudgments(cfg judgmentConfig, inputRoot string) (inference.Binding, string, error) {
	if cfg == (judgmentConfig{}) {
		return inference.Binding{Provider: "unavailable", Model: "unbound"}, "", nil
	}
	if cfg.AuthSnapshot == "" || filepath.IsAbs(cfg.AuthSnapshot) || filepath.Clean(cfg.AuthSnapshot) != cfg.AuthSnapshot {
		return inference.Binding{}, "", errors.New("judgment auth snapshot must be relative to the private review input root")
	}
	driver, err := claudeinference.New(cfg.CLI)
	if err != nil {
		return inference.Binding{}, "", err
	}
	token, err := readSetupTokenSnapshot(inputRoot, filepath.Join(inputRoot, cfg.AuthSnapshot))
	if err != nil {
		return inference.Binding{}, "", errors.New("judgment setup-token snapshot is unavailable or unsafe")
	}
	// The outer digest commits to configuration and credential content without
	// storing either the credential or its separately usable content digest.
	receipt, _ := json.Marshal(struct {
		Protocol    string
		Config      judgmentConfig
		TokenDigest string
	}{claudeinference.Protocol, cfg, contentaddr.Sum(token)})
	digest := contentaddr.Sum(receipt)
	return inference.Binding{Provider: claudeinference.Protocol + ":" + digest, Model: cfg.CLI.Model, Credential: inference.Secret(token), Driver: driver}, digest, nil
}
