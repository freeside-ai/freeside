package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// maxJudgmentPromptBytes bounds a judgment-role prompt file. The composed
// system prompt reaches the CLI as a single --system-prompt argument, so the
// operating system's argument-length limit caps it; 32 KiB stays well under it.
const maxJudgmentPromptBytes = 32 << 10

type judgmentConfig struct {
	CLI          claudeinference.Config `json:"cli"`
	AuthSnapshot string                 `json:"auth_snapshot"`
	// PublicationAuthorPrompt is the path to the publication-author role prompt
	// file. The daemon reads it at startup and folds the file's content digest
	// into the judgment configuration digest preflight records. The path stays
	// on the comparable judgmentConfig; the bytes do not.
	PublicationAuthorPrompt string `json:"publication_author_prompt"`
	ExpectedDigest          string `json:"-"`
}

func judgmentFlags(flags *flag.FlagSet, cfg *judgmentConfig) {
	flags.StringVar(&cfg.CLI.Binary, "judgment-claude-bin", "", "absolute native Claude CLI path for subscription judgments (optional)")
	flags.StringVar(&cfg.CLI.SHA256, "judgment-claude-sha256", "", "exact Claude CLI executable digest")
	flags.StringVar(&cfg.CLI.Model, "judgment-model", "", "explicit Claude model for classifier and adjudicator")
	flags.StringVar(&cfg.AuthSnapshot, "judgment-auth-snapshot", "", "existing Claude setup-token snapshot relative to review-input-root")
	flags.StringVar(&cfg.PublicationAuthorPrompt, "judgment-publication-author-prompt", "", "publication-author role prompt file (optional; both sites fail safe when unset)")
}

// readJudgmentPrompt reads a configured judgment-role prompt file, rejecting an
// empty or oversized one so half a prompt never becomes a live system prompt.
func readJudgmentPrompt(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: deployment-owned operator config path, read once at startup.
	if err != nil {
		return nil, errors.New("judgment publication-author prompt file is unavailable")
	}
	if len(data) == 0 {
		return nil, errors.New("judgment publication-author prompt file is empty")
	}
	if len(data) > maxJudgmentPromptBytes {
		return nil, errors.New("judgment publication-author prompt file exceeds the size limit")
	}
	return data, nil
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
	var authorPrompt []byte
	authorPromptDigest := ""
	if cfg.PublicationAuthorPrompt != "" {
		prompt, err := readJudgmentPrompt(cfg.PublicationAuthorPrompt)
		if err != nil {
			return inference.Binding{}, "", err
		}
		authorPrompt = prompt
		authorPromptDigest = contentaddr.Sum(authorPrompt)
	}
	driver, err := claudeinference.New(cfg.CLI, claudeinference.WithPublicationAuthorPrompt(authorPrompt))
	if err != nil {
		return inference.Binding{}, "", err
	}
	token, err := readSetupTokenSnapshot(inputRoot, filepath.Join(inputRoot, cfg.AuthSnapshot))
	if err != nil {
		return inference.Binding{}, "", errors.New("judgment setup-token snapshot is unavailable or unsafe")
	}
	// The outer digest commits to configuration and credential content without
	// storing either the credential or its separately usable content digest. The
	// prompt's content digest pins the exact role prompt bytes, so a different
	// prompt file yields a different judgment configuration digest even though
	// only its path rides on the comparable config.
	receipt, _ := json.Marshal(struct {
		Protocol                      string
		Config                        judgmentConfig
		TokenDigest                   string
		PublicationAuthorPromptDigest string
	}{claudeinference.Protocol, cfg, contentaddr.Sum(token), authorPromptDigest})
	digest := contentaddr.Sum(receipt)
	return inference.Binding{Provider: claudeinference.Protocol + ":" + digest, Model: cfg.CLI.Model, Credential: inference.Secret(token), Driver: driver}, digest, nil
}
