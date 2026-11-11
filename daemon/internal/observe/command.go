package observe

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe/observedb"
)

// The `freesided follow` verb lives here rather than in the command package
// so its containment boundary is a real one. A file in package main can call
// every unexported helper its siblings define without importing anything, so
// no assertion over that file proves what it can reach; a package can only
// reach what it imports, and containment_test.go holds this package's imports
// to a closed list that names no filesystem, process, or network capability.

// ErrUsage marks an invocation mistake (an unknown flag, a missing or
// malformed required value, a stray positional argument) as distinct from a
// failure to observe, so the command can exit 2 for the first and 1 for the
// second.
var ErrUsage = errors.New("invalid invocation")

// Source is the bounded observation surface supplied by the command shim.
type Source interface {
	ObserveRun(context.Context, domain.RunID) (domain.RunObservation, error)
	ObserveConclusion(context.Context, domain.RunID) (domain.RunObservation, domain.RunConclusion, error)
	ObserveSnapshot(context.Context, domain.RunID) (observedb.Snapshot, error)
	Close() error
}

type Opener func(context.Context, string, ...domain.Digest) (Source, error)

// Run parses the follow verb's flags, opens the selected observation source, and follows
// the selected run to a final outcome, an interrupt, (with -once) a single
// snapshot, or (with -until) a checkpoint or a timeout. Observing a run whose own outcome is blocked or failed succeeds:
// the command reports what the daemon saw, and the printed outcome line
// carries the run's verdict.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, open Opener) (err error) {
	flags := flag.NewFlagSet("freesided follow", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	runID := flags.String("run", "", "run id to follow (required)")
	interval := flags.Duration("interval", DefaultInterval, "observation read cadence")
	window := flags.Duration("freshness-window", domain.DefaultObservationFreshnessWindow,
		"age past which the last observation reads as an observation gap")
	once := flags.Bool("once", false, "print one snapshot instead of following")
	snapshot := flags.Bool("snapshot", false, "print one machine-readable supervision snapshot")
	approvedRecipe := flags.String("approved-recipe", "", "recipe digest approved for snapshot evidence")
	untilText := flags.String("until", "",
		"wait until COND holds: card=TYPE, stage-started=STAGE, stage-finished=STAGE, or park-ended (requires -timeout)")
	timeout := flags.Duration("timeout", 0, "how long -until waits before exiting 1")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	waiting := *untilText != ""
	switch {
	case flags.NArg() != 0:
		return fmt.Errorf("%w: unexpected positional arguments: %v", ErrUsage, flags.Args())
	case *dbPath == "":
		return fmt.Errorf("%w: -db is required", ErrUsage)
	case *runID == "":
		return fmt.Errorf("%w: -run is required", ErrUsage)
	case *interval <= 0:
		return fmt.Errorf("%w: -interval must be positive, got %s", ErrUsage, *interval)
	case *window <= 0:
		return fmt.Errorf("%w: -freshness-window must be positive, got %s", ErrUsage, *window)
	case *once && *snapshot:
		return fmt.Errorf("%w: -once and -snapshot are mutually exclusive", ErrUsage)
	case waiting && (*once || *snapshot):
		return fmt.Errorf("%w: -until cannot be combined with -once or -snapshot", ErrUsage)
	case waiting && *timeout <= 0:
		return fmt.Errorf("%w: -until requires a positive -timeout, got %s", ErrUsage, *timeout)
	case !waiting && *timeout != 0:
		return fmt.Errorf("%w: -timeout requires -until", ErrUsage)
	case *approvedRecipe != "" && !*snapshot && !waiting:
		return fmt.Errorf("%w: -approved-recipe requires -snapshot or -until", ErrUsage)
	}
	var until Until
	if waiting {
		parsed, err := ParseUntil(*untilText)
		if err != nil {
			return err
		}
		until = parsed
	}
	// Source is the only database access this package has: no write,
	// checkpoint, restore, or backup-file capability reaches the follow path.
	var approvedRecipes []domain.Digest
	if *approvedRecipe != "" {
		approvedRecipes = append(approvedRecipes, domain.Digest(*approvedRecipe))
	}
	db, err := open(ctx, *dbPath, approvedRecipes...)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if *snapshot {
		current, err := db.ObserveSnapshot(ctx, domain.RunID(*runID))
		if err != nil {
			return err
		}
		return writeSnapshot(stdout, current)
	}
	if waiting {
		return Wait(ctx, db, stdout, WaitConfig{
			RunID: domain.RunID(*runID), Until: until, Timeout: *timeout,
			Interval: *interval, Window: *window,
		})
	}
	return Follow(ctx, db, stdout, Config{
		RunID: domain.RunID(*runID), Interval: *interval, Window: *window, Once: *once,
	})
}

type supervisionSnapshot struct {
	RunID             domain.RunID                      `json:"run_id"`
	State             SupervisionState                  `json:"state"`
	Outcome           domain.RunOutcome                 `json:"outcome"`
	Reason            *domain.RunHoldReason             `json:"outcome_reason,omitempty"`
	HoldReason        *domain.RunHoldReason             `json:"hold_reason,omitempty"`
	Terminal          *domain.ObservedInvocationStatus  `json:"terminal,omitempty"`
	Lineage           *observedb.Lineage                `json:"lineage,omitempty"`
	Admission         *observedb.Admission              `json:"admission,omitempty"`
	LastStage         string                            `json:"last_stage,omitempty"`
	AttentionItems    []observedb.AttentionItem         `json:"attention_items"`
	LastAttentionItem *observedb.AttentionItem          `json:"last_attention_item,omitempty"`
	ShadowReviews     []domain.ShadowReviewRecord       `json:"shadow_reviews"`
	ClassifierSamples []domain.ClassifierAccuracySample `json:"classifier_samples"`
	ReviewYield       []observedb.ReviewYield           `json:"review_yield"`
	Adjudications     []observedb.AdjudicationDispatch  `json:"adjudications"`
}

func writeSnapshot(out io.Writer, current observedb.Snapshot) error {
	conclusion := current.AuthenticatedConclusion
	view := supervisionSnapshot{
		RunID: current.Observation.RunID, State: deriveSupervisionState(current, conclusion),
		Outcome: conclusion.Outcome, Reason: conclusion.Reason, Terminal: conclusion.Terminal,
		Lineage: current.Attempt, LastStage: current.LastStage,
		AttentionItems: current.AttentionItems,
		ShadowReviews:  current.ShadowReviews, ClassifierSamples: current.ClassifierSamples,
		ReviewYield: current.ReviewYield, Adjudications: current.Adjudications,
	}
	if current.Observation.Hold != nil {
		view.HoldReason = &current.Observation.Hold.Reason
	}
	if len(current.AttentionItems) > 0 {
		last := current.AttentionItems[len(current.AttentionItems)-1]
		view.LastAttentionItem = &last
	}
	if len(current.Admissions) > 0 {
		selected := current.Admissions[len(current.Admissions)-1]
		for _, admission := range current.Admissions {
			if admission.Stage == string(domain.StageNameImplementation) {
				selected = admission
			}
		}
		view.Admission = &selected
	}
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(view); err != nil {
		return fmt.Errorf("snapshot: write output: %w", err)
	}
	return nil
}

// ExitCode maps a Run result onto the command's exit contract: 0 observed
// (including an interrupted follow, and including a run whose own outcome is
// blocked or failed) or an -until condition met, 1 could not observe or an
// -until wait that timed out or was interrupted, 2 invocation mistake.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrUsage):
		return 2
	default:
		return 1
	}
}
