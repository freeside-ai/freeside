package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// maxJudgmentPromptBytes bounds a judgment-role prompt file. The composed
// system prompt reaches the CLI as a single --system-prompt argument, so the
// operating system's argument-length limit caps it; 32 KiB stays well under it.
const maxJudgmentPromptBytes = 32 << 10

type judgmentConfig struct {
	CLI claudeinference.Config `json:"cli"`
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
func composeRuntimeJudgments(cfg judgmentConfig) (judgmentRuntime, error) {
	expected := cfg.ExpectedDigest
	cfg.ExpectedDigest = ""
	if cfg != (judgmentConfig{}) && !contentaddr.Valid(expected) {
		return judgmentRuntime{}, errors.New("judgment startup requires a preflight configuration digest")
	}
	runtime, actual, err := composeJudgments(cfg)
	if err != nil {
		return judgmentRuntime{}, err
	}
	if actual != expected {
		return judgmentRuntime{}, errors.New("judgment configuration differs from preflight")
	}
	return runtime, nil
}

// composeProductionJudgments composes the judgment runtime the daemon starts
// with: what the flags name, held to the preflight digest, with the driver
// wrapped in the task-judgment ownership journal under stateDir, which is
// also returned so a task stop can confirm the run's judgments quiescent.
// With the flags absent there is no driver, and the runtime stays the zero
// value: judgments are off, the daemon starts, and judgmentRoles answers
// every role off (#1945). The journal is still opened, around no driver,
// because its records outlive the process: a task stop confirms what an
// earlier start with judgments on left there.
func composeProductionJudgments(stateDir string, cfg judgmentConfig) (judgmentRuntime, *ward.TaskJudgments, error) {
	runtime, err := composeRuntimeJudgments(cfg)
	if err != nil {
		return judgmentRuntime{}, nil, fmt.Errorf("compose subscription judgments: %w", err)
	}
	owned, err := ward.NewTaskJudgments(filepath.Join(stateDir, "task-judgments"), runtime.Driver)
	if err != nil {
		return judgmentRuntime{}, nil, err
	}
	if runtime.Driver != nil {
		runtime.Driver = owned
	}
	return runtime, owned, nil
}

// composeJudgments composes what the judgment flags name: the pinned CLI
// driver and the publication author's prompt. It reads no credential: each
// call takes its token from the enrollment its lineup line names
// (judgmentCredentials). An empty configuration composes the zero runtime:
// judgments switched off.
func composeJudgments(cfg judgmentConfig) (judgmentRuntime, string, error) {
	if cfg == (judgmentConfig{}) {
		return judgmentRuntime{}, "", nil
	}
	var authorPrompt []byte
	authorPromptDigest := ""
	if cfg.PublicationAuthorPrompt != "" {
		prompt, err := readJudgmentPrompt(cfg.PublicationAuthorPrompt)
		if err != nil {
			return judgmentRuntime{}, "", err
		}
		authorPrompt = prompt
		authorPromptDigest = contentaddr.Sum(authorPrompt)
	}
	driver, err := claudeinference.New(cfg.CLI)
	if err != nil {
		return judgmentRuntime{}, "", err
	}
	// The digest commits to the configuration and to no credential: the
	// token a call carries is the enrollment generation's, which the call
	// record names. The prompt's content digest pins the exact role prompt
	// bytes, so a different prompt file yields a different judgment
	// configuration digest even though only its path rides on the comparable
	// config.
	receipt, _ := json.Marshal(struct {
		Protocol                      string
		Config                        judgmentConfig
		PublicationAuthorPromptDigest string
	}{claudeinference.Protocol, cfg, authorPromptDigest})
	return judgmentRuntime{Driver: driver, AuthorPrompt: authorPrompt}, contentaddr.Sum(receipt), nil
}
