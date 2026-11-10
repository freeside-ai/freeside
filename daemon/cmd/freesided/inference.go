package main

import (
	"flag"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
)

func judgmentFlags(flags *flag.FlagSet, cfg *daemoncompose.JudgmentConfig) {
	flags.StringVar(&cfg.CLI.Binary, "judgment-claude-bin", "", "absolute native Claude CLI path for subscription judgments (optional)")
	flags.StringVar(&cfg.CLI.SHA256, "judgment-claude-sha256", "", "exact Claude CLI executable digest")
	flags.StringVar(&cfg.PublicationAuthorPrompt, "judgment-publication-author-prompt", "", "publication-author role prompt file (optional; both sites fail safe when unset)")
}
