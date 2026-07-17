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
	// TranscriptTruncated mirrors the report's flag: the transcript
	// artifact was cut at the transcript cap.
	TranscriptTruncated bool `json:"transcript_truncated"`
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
	// Enforce both bindings first: everything after this reads the
	// candidate and the base by these exact SHAs, so an unheld head or
	// a base that is not the named commit fails closed before any
	// recipe or tree work (materialize re-checks the head).
	if err := g.verifyHead(ctx, opts.HeadSHA); err != nil {
		return Result{}, err
	}
	if err := verifyBase(ctx, g, opts.BaseSHA); err != nil {
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
	commandPaths := recipe.CommandPaths()
	// Fail closed on a symlink command entrypoint. exec follows a
	// symlink to its target, so the executed control surface would be
	// the target, not the recorded lexical path, and a candidate change
	// to that target would go unflagged. Resolving symlink chains in the
	// tree is disproportionate for a trusted recipe, so the recipe must
	// name a regular file directly.
	if err := g.rejectSymlinkEntrypoints(ctx, opts.HeadSHA, commandPaths); err != nil {
		return Result{}, err
	}
	findings := flagControlPaths(opts.Changes, opts.Policy.ExtraVerificationControlPatterns, commandPaths, opts.RecipePath)
	divergence, err := recipeDivergence(ctx, g, opts.RecipeSource, opts.HeadSHA, opts.RecipePath, trusted, opts.Policy.MaxRecipeBytes)
	if err != nil {
		return Result{}, err
	}
	findings = append(findings, divergence...)
	if findings == nil {
		findings = []Finding{}
	}
	steps, transcript, outcome, err := runRecipe(ctx, g, opts, recipe, scratch)
	if err != nil {
		return Result{}, err
	}
	rep := report{
		HeadSHA:             opts.HeadSHA,
		BaseSHA:             opts.BaseSHA,
		RecipePath:          opts.RecipePath,
		RecipeDigest:        recipeDigest,
		Outcome:             outcome,
		Steps:               steps,
		Findings:            findings,
		TranscriptTruncated: transcript.truncated,
	}
	evidence, err := buildEvidence(opts, recipeDigest, rep, transcript.buf.Bytes())
	if err != nil {
		return Result{}, err
	}
	return Result{
		HeadSHA:             opts.HeadSHA,
		RecipeDigest:        recipeDigest,
		Outcome:             outcome,
		Steps:               steps,
		Findings:            findings,
		TranscriptTruncated: transcript.truncated,
		Evidence:            evidence,
	}, nil
}

// rejectSymlinkEntrypoints fails closed if any command path, or any of
// its directory prefixes, is a symlink in the candidate head's tree
// (mode 120000). exec follows a symlink whether it is the final
// component (`./run-check` -> `scripts/verify.sh`) or a prefix
// directory (`./run-check/verify.sh` with `run-check` -> `scripts`), so
// in either case the executed control surface is a target the recorded
// lexical path does not name. commitSHA and the paths are daemon-derived
// (validated options and the trusted recipe's own argv), never
// candidate bytes, so composing the ls-tree pathspec is safe;
// GIT_LITERAL_PATHSPECS (the runner env) pins each path as a literal
// name.
func (g *gitRunner) rejectSymlinkEntrypoints(ctx context.Context, commitSHA string, commandPaths []string) error {
	checked := map[string]bool{}
	for _, p := range commandPaths {
		segs := strings.Split(p, "/")
		for i := range segs {
			prefix := strings.Join(segs[:i+1], "/")
			if checked[prefix] {
				continue
			}
			checked[prefix] = true
			symlink, err := g.isTreeSymlink(ctx, commitSHA, prefix)
			if err != nil {
				return err
			}
			if symlink {
				return fmt.Errorf("recipe command entrypoint %q traverses symlink %q; name the target file directly: %w", p, prefix, ErrSymlinkEntrypoint)
			}
		}
	}
	return nil
}

// isTreeSymlink reports whether path is a symlink entry (mode 120000)
// in commitSHA's tree. An absent path is not a symlink (a
// candidate-added path is flagged as a change elsewhere).
func (g *gitRunner) isTreeSymlink(ctx context.Context, commitSHA, path string) (bool, error) {
	out, err := g.run(ctx, nil, "ls-tree", "-z", "--full-tree", commitSHA, "--", path)
	if err != nil {
		return false, err
	}
	rec, _, _ := strings.Cut(string(out), "\x00")
	if rec == "" {
		return false, nil
	}
	meta, _, _ := strings.Cut(rec, "\t")
	fields := strings.Fields(meta)
	return len(fields) >= 1 && fields[0] == "120000", nil
}

// runRecipe executes the trusted recipe's commands in order, fail-fast:
// a non-zero exit (including a timeout kill) fails the verification and
// later commands do not run, matching what the recipe's own toolchain
// would do. The named §5.6 residual applies here: candidate test code
// executes inside the room.
//
// Every command runs in its own freshly materialized workspace, not a
// shared one: an earlier command's candidate code (a `go test` running
// the candidate's test functions) could otherwise rewrite files a later
// command reads, so the later command would verify bytes that are not
// the bound head while the evidence still claims that head. A fresh
// checkout per command makes each command provably run against the head
// (the clean-room model, §6); recipe commands are therefore independent
// checks and cannot pass workspace state between one another.
func runRecipe(ctx context.Context, g *gitRunner, opts Options, recipe Recipe, scratch string) ([]Step, *boundedBuffer, Outcome, error) {
	steps := make([]Step, 0, len(recipe.Commands))
	transcript := &boundedBuffer{max: opts.Policy.MaxTranscriptBytes}
	outcome := OutcomePassed
	for i, argv := range recipe.Commands {
		workspace := filepath.Join(scratch, fmt.Sprintf("workspace-%d", i))
		if err := g.materialize(ctx, opts.HeadSHA, workspace); err != nil {
			return nil, nil, "", err
		}
		res, err := runStep(ctx, opts, workspace, argv)
		if err != nil {
			return nil, nil, "", fmt.Errorf("recipe command %q: %w", argv[0], err)
		}
		steps = append(steps, Step{Argv: argv, ExitCode: res.ExitCode, OutputTruncated: res.Truncated})
		writeTranscriptStep(transcript, argv, res)
		// Remove the used workspace before the next materialization so
		// the scratch does not hold N full checkouts at once.
		_ = os.RemoveAll(workspace)
		if res.ExitCode != 0 {
			outcome = OutcomeFailed
			break
		}
	}
	return steps, transcript, outcome, nil
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
