package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Result is one verification's account: the exact binding (head and
// recipe digest), the outcome, the per-step record, the accumulated
// risk-flag findings, and the emitted evidence. Integrity violations
// (unreadable trusted recipe, head mismatch, room failure) fail closed
// with a typed error and no Result; findings are risk flags for the
// publication gate, never errors.
type Result struct {
	HeadSHA      string        `json:"head_sha"`
	RecipeDigest domain.Digest `json:"recipe_digest"`
	Outcome      Outcome       `json:"outcome"`
	Steps        []Step        `json:"steps"`
	Findings     []Finding     `json:"findings"`
	// Evidence carries the emitted artifacts with their content; the
	// caller persists them. Excluded from the JSON account: the report
	// artifact is the serialized account.
	Evidence []Evidence `json:"-"`
}

// Verify runs the trusted recipe against the exact candidate head in a
// fresh workspace materialized from the daemon-owned checkout at
// checkoutDir, and emits the verifier's evidence (§5.6, §5.15). The
// trusted recipe governs execution unconditionally; candidate content
// can be flagged but can never steer what runs.
func Verify(ctx context.Context, checkoutDir string, opts Options) (Result, error) {
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return Result{}, err
	}
	scratch, err := os.MkdirTemp("", "freeside-verify-")
	if err != nil {
		return Result{}, fmt.Errorf("create verify scratch: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	g, err := newGitRunner(ctx, opts.GitPath, checkoutDir, scratch)
	if err != nil {
		return Result{}, err
	}
	trusted, err := loadTrustedRecipeBytes(ctx, g, opts.RecipeSource, opts.BaseSHA, opts.RecipePath, opts.Policy.MaxRecipeBytes)
	if err != nil {
		return Result{}, err
	}
	recipe, err := ParseRecipe(trusted)
	if err != nil {
		return Result{}, err
	}
	recipeDigest := RecipeDigest(trusted)
	findings := flagControlPaths(opts.Changes, opts.Policy.ExtraVerificationControlPatterns, recipe.CommandPaths(), opts.RecipePath)
	divergence, err := recipeDivergence(ctx, g, opts.RecipeSource, opts.HeadSHA, opts.RecipePath, trusted, opts.Policy.MaxRecipeBytes)
	if err != nil {
		return Result{}, err
	}
	findings = append(findings, divergence...)
	if findings == nil {
		findings = []Finding{}
	}
	workspace := filepath.Join(scratch, "workspace")
	if err := g.materialize(ctx, opts.HeadSHA, workspace); err != nil {
		return Result{}, err
	}
	steps, transcript, outcome, err := runRecipe(ctx, opts, recipe, workspace)
	if err != nil {
		return Result{}, err
	}
	rep := report{
		HeadSHA:      opts.HeadSHA,
		BaseSHA:      opts.BaseSHA,
		RecipePath:   opts.RecipePath,
		RecipeDigest: recipeDigest,
		Outcome:      outcome,
		Steps:        steps,
		Findings:     findings,
	}
	evidence, err := buildEvidence(opts, recipeDigest, rep, transcript)
	if err != nil {
		return Result{}, err
	}
	return Result{
		HeadSHA:      opts.HeadSHA,
		RecipeDigest: recipeDigest,
		Outcome:      outcome,
		Steps:        steps,
		Findings:     findings,
		Evidence:     evidence,
	}, nil
}

// runRecipe executes the trusted recipe's commands in order, fail-fast:
// a non-zero exit (including a timeout kill) fails the verification and
// later commands do not run, matching what the recipe's own toolchain
// would do. The named §5.6 residual applies here: candidate test code
// executes inside the room.
func runRecipe(ctx context.Context, opts Options, recipe Recipe, workspace string) ([]Step, []byte, Outcome, error) {
	steps := make([]Step, 0, len(recipe.Commands))
	transcript := &boundedBuffer{max: opts.Policy.MaxTranscriptBytes}
	outcome := OutcomePassed
	for _, argv := range recipe.Commands {
		res, err := runStep(ctx, opts, workspace, argv)
		if err != nil {
			return nil, nil, "", fmt.Errorf("recipe command %q: %w", argv[0], err)
		}
		steps = append(steps, Step{Argv: argv, ExitCode: res.ExitCode, OutputTruncated: res.Truncated})
		writeTranscriptStep(transcript, argv, res)
		if res.ExitCode != 0 {
			outcome = OutcomeFailed
			break
		}
	}
	return steps, transcript.buf.Bytes(), outcome, nil
}

// runStep runs one command under the per-command timeout.
func runStep(ctx context.Context, opts Options, workspace string, argv []string) (StepResult, error) {
	stepCtx := ctx
	if opts.Policy.CommandTimeout > 0 {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(ctx, opts.Policy.CommandTimeout)
		defer cancel()
	}
	return opts.Room.Run(stepCtx, workspace, argv)
}

// writeTranscriptStep appends one step's record to the transcript.
// Recipe parsing rejected every shell metacharacter, so the
// space-joined argv rendering is unambiguous.
func writeTranscriptStep(w *boundedBuffer, argv []string, res StepResult) {
	_, _ = w.Write([]byte("$ " + strings.Join(argv, " ") + "\n"))
	_, _ = w.Write(res.Output)
	suffix := ""
	if res.Truncated {
		suffix = " (output truncated)"
	}
	_, _ = fmt.Fprintf(w, "exit %d%s\n\n", res.ExitCode, suffix)
}
