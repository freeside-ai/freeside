package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// replaceFixture is a submitted task on one store, reachable offline or
// through a running daemon, plus the inputs to submit its source again.
type replaceFixture struct {
	root  string
	cfg   submitCommandConfig
	first submitResult
	// withStore runs fn against the store: the daemon's when one runs,
	// otherwise a store opened for the call.
	withStore func(t *testing.T, fn func(*store.Store))
}

func newReplaceFixture(t *testing.T, live bool) replaceFixture {
	t.Helper()
	ctx := t.Context()
	root := t.TempDir()
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		SubmissionID: "replaced", DBPath: filepath.Join(root, "freeside.db"),
		TaskPath: taskPath, PolicyPath: policyPath, WorkUnitPath: declaredWorkUnit(t), PublicationPath: publicationPath,
		ProjectID: "proj-replace",
	}
	seedTemplateStore(t, cfg.DBPath)
	f := replaceFixture{root: root, cfg: cfg}
	f.withStore = func(t *testing.T, fn func(*store.Store)) {
		t.Helper()
		st, err := store.OpenExisting(ctx, cfg.DBPath, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		fn(st)
	}
	if live {
		h, err := run(ctx, nil, config{Environment: environmentEphemeral, DBPath: cfg.DBPath, ListenAddr: "127.0.0.1:0"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		f.withStore = func(t *testing.T, fn func(*store.Store)) { t.Helper(); fn(h.Store) }
	}
	first, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.first = first
	return f
}

// seedItem opens an item of the given type on the first task's
// specification run.
func (f replaceFixture) seedItem(t *testing.T, id domain.ItemID, kind domain.AttentionType) {
	t.Helper()
	runID := f.first.SpecificationRunID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: f.cfg.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    kind, Priority: domain.PriorityHigh, Reason: "Operator decision needed.",
		RequestedDecision: []domain.Action{domain.ActionStop}, ItemVersion: 1, Status: domain.StatusOpen,
		InterruptionClass: domain.InterruptionExceptional,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.withStore(t, func(st *store.Store) {
		if err := st.Write(t.Context(), func(tx *store.WriteTx) error { return tx.PutAttentionItem(t.Context(), item) }); err != nil {
			t.Fatal(err)
		}
	})
}

// lifecycle reads a task's display lifecycle, nil while it is unfinished.
func (f replaceFixture) lifecycle(t *testing.T, id domain.TaskID) *domain.TaskLifecycle {
	t.Helper()
	var lifecycle *domain.TaskLifecycle
	f.withStore(t, func(st *store.Store) {
		if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
			task, err := tx.GetTask(t.Context(), id)
			lifecycle = task.DisplayLifecycle(nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	return lifecycle
}

// replacement is the fixture's submission again under a new identity,
// naming the first task for --replace.
func (f replaceFixture) replacement(id string) submitCommandConfig {
	cfg := f.cfg
	cfg.SubmissionID = id
	cfg.ReplaceTaskID = f.first.TaskID
	return cfg
}

func TestSubmitReplaceAbandonsTheFailedTaskInTheSameWrite(t *testing.T) {
	t.Parallel()
	for _, live := range []bool{false, true} {
		name := "offline"
		if live {
			name = "control"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newReplaceFixture(t, live)
			f.seedItem(t, "replace-failure", domain.AttentionExecutionFailure)

			result, err := runSubmitCommand(ctx, f.replacement("replacement"))
			if err != nil {
				t.Fatal(err)
			}
			replaced := result.ReplacedTask
			if result.TaskID == "" || result.TaskID == f.first.TaskID || replaced == nil ||
				replaced.TaskID != f.first.TaskID || !replaced.Released || replaced.Episode != 1 ||
				!replaced.WorkflowClosed || !replaced.CleanupComplete || replaced.Error != "" ||
				!slices.Contains(replaced.RetiredItems, "replace-failure") {
				t.Fatalf("result %+v, replaced %+v; want a new task and the old one abandoned and cleaned up", result, replaced)
			}
			if got := f.lifecycle(t, f.first.TaskID); got == nil || *got != domain.TaskAbandoned {
				t.Fatalf("replaced task lifecycle = %v, want abandoned", got)
			}
			if got := f.lifecycle(t, result.TaskID); got != nil {
				t.Fatalf("new task lifecycle = %v, want unfinished", *got)
			}

			// The journal keeps the choice, and a manual retry replays it
			// without a second abandon.
			journal, err := os.ReadFile(submissionJournalPath(f.cfg.DBPath, "replacement"))
			if err != nil || !strings.Contains(string(journal), `"ReplaceTaskID":"`+string(f.first.TaskID)+`"`) {
				t.Fatalf("journal %s, %v; want the replaced task", journal, err)
			}
			retried, err := runSubmitCommand(ctx, submitCommandConfig{DBPath: f.cfg.DBPath, RetrySubmissionID: "replacement"})
			if err != nil {
				t.Fatal(err)
			}
			if retried.TaskID != result.TaskID || retried.ReplacedTask == nil || retried.ReplacedTask.Released ||
				retried.ReplacedTask.Episode != 1 || !retried.ReplacedTask.CleanupComplete {
				t.Fatalf("retry %+v, replaced %+v; want the same task and a converged cleanup", retried, retried.ReplacedTask)
			}
		})
	}
}

func TestSubmitReplaceRetryFinishesAFailedCleanup(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newReplaceFixture(t, false)
	f.seedItem(t, "replace-failure", domain.AttentionExecutionFailure)
	db, err := sql.Open("sqlite", f.cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_closure BEFORE INSERT ON task_workflow_closures
		BEGIN SELECT RAISE(ABORT, 'injected closure failure'); END`); err != nil {
		t.Fatal(err)
	}

	result, err := runSubmitCommand(ctx, f.replacement("replacement"))
	if err == nil || !strings.Contains(err.Error(), "repair with abandon -task "+string(f.first.TaskID)+" -episode 1") ||
		result.TaskID == "" || result.ReplacedTask == nil || result.ReplacedTask.WorkflowClosed ||
		result.ReplacedTask.CleanupComplete {
		t.Fatalf("result %+v, %v; want the new task and a cleanup failure naming the repair", result, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_closure`); err != nil {
		t.Fatal(err)
	}
	retried, err := runSubmitCommand(ctx, submitCommandConfig{DBPath: f.cfg.DBPath, RetrySubmissionID: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	if retried.TaskID != result.TaskID || !retried.ReplacedTask.WorkflowClosed || !retried.ReplacedTask.CleanupComplete ||
		!slices.Contains(retried.ReplacedTask.RetiredItems, "replace-failure") {
		t.Fatalf("retry replaced %+v; want the closure and retirement finished", retried.ReplacedTask)
	}
}

func TestSubmitReplaceRefusesBeforeChangingAnything(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// open seeds the first task's open items.
		open []domain.AttentionType
		// seed changes the store before the refused submission.
		seed func(t *testing.T, f replaceFixture)
		edit func(t *testing.T, f replaceFixture, cfg *submitCommandConfig)
		want string
	}{
		{
			name: "a proposal the episode owns",
			open: []domain.AttentionType{domain.AttentionExecutionFailure},
			seed: seedReplacedTaskProposal,
			want: "(task_proposal)",
		},
		{
			name: "already abandoned",
			open: []domain.AttentionType{domain.AttentionExecutionFailure},
			seed: func(t *testing.T, f replaceFixture) {
				if _, err := runAbandonCommand(t.Context(), abandonCommandConfig{DBPath: f.cfg.DBPath, TaskID: f.first.TaskID}); err != nil {
					t.Fatal(err)
				}
			},
			want: "is abandoned",
		},
		{
			name: "another item open",
			open: []domain.AttentionType{domain.AttentionExecutionFailure, domain.AttentionSpecApproval},
			want: "items open other than execution failures: replace-item-0 (execution_failure), replace-item-1 (spec_approval)",
		},
		{name: "no item open", want: "has no execution failure open"},
		{
			name: "another project",
			open: []domain.AttentionType{domain.AttentionExecutionFailure},
			edit: func(_ *testing.T, _ replaceFixture, cfg *submitCommandConfig) { cfg.ProjectID = "proj-other" },
			want: "is in project proj-replace, not proj-other",
		},
		{
			name: "another source",
			open: []domain.AttentionType{domain.AttentionExecutionFailure},
			edit: func(t *testing.T, f replaceFixture, cfg *submitCommandConfig) {
				cfg.TaskPath = filepath.Join(f.root, "other-spec.md")
				if err := os.WriteFile(cfg.TaskPath, []byte("# Other task\n\nDo something else.\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "does not hold this submission's source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newReplaceFixture(t, false)
			for i, kind := range tc.open {
				f.seedItem(t, domain.ItemID("replace-item-"+string(rune('0'+i))), kind)
			}
			if tc.seed != nil {
				tc.seed(t, f)
			}
			before := f.lifecycle(t, f.first.TaskID)
			cfg := f.replacement("refused")
			if tc.edit != nil {
				tc.edit(t, f, &cfg)
			}
			result, err := runSubmitCommand(t.Context(), cfg)
			if !errors.Is(err, errReplaceRefused) || !strings.Contains(err.Error(), tc.want) || result.TaskID != "" {
				t.Fatalf("result %+v, %v; want a refusal containing %q", result, err, tc.want)
			}
			if got := f.lifecycle(t, f.first.TaskID); !reflect.DeepEqual(got, before) {
				t.Fatalf("named task lifecycle = %v, want it unchanged from %v", got, before)
			}
			f.withStore(t, func(st *store.Store) {
				if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
					tasks, err := tx.ListTasks(t.Context())
					if err == nil && len(tasks) != 1 {
						t.Fatalf("tasks = %d, want only the named one", len(tasks))
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

// seedReplacedTaskProposal opens a task proposal declared by the first
// task's specification run. Its subject names no task, yet closing the
// task's episode would retire it.
func seedReplacedTaskProposal(t *testing.T, f replaceFixture) {
	t.Helper()
	ctx := t.Context()
	f.withStore(t, func(st *store.Store) {
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			policy, err := tx.GetResolvedPolicy(ctx, f.first.SpecificationRunID)
			if err != nil {
				return err
			}
			declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
				CompletionCriterion: domain.CompletionBoundPRMerged, DeclaredPaths: domain.CanonicalDeclaredPaths(policy),
			}, policy.RunID, f.cfg.ProjectID, proposalSeedAt.Add(-time.Hour))
			if err != nil {
				return err
			}
			if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
				return err
			}
			proposal, err := domain.NewEffectProposal(domain.EffectTaskProposal, domain.TaskProposalParameters{
				SubjectHandle: domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID)),
				Intent:        domain.TaskProposalIntentImplement, ExpectedCostUnits: 10,
				Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
			}, policy)
			if err != nil {
				return err
			}
			_, err = putTaskProposalCard(ctx, tx, f.cfg.ProjectID, proposal, "replace")
			return err
		}); err != nil {
			t.Fatalf("seed task proposal: %v", err)
		}
	})
}

func TestSubmitJournalWithoutReplaceKeepsItsBytes(t *testing.T) {
	t.Parallel()
	// A journal from before the field: the field must not appear in one
	// saved without --replace, so the two decode and encode alike.
	before := `{"SubmissionID":"s","RetrySubmissionID":"","SavedInputs":null,"DBPath":"/db","TaskPath":"t",` +
		`"PolicyPath":"p","PublicationPath":"pub","WorkUnitPath":"w","CompositionPath":"","RequireComposition":false,"ProjectID":"proj"}`
	var saved submitCommandConfig
	if err := json.Unmarshal([]byte(before), &saved); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Fatalf("journal re-encodes as\n%s\nwant\n%s", after, before)
	}
}

func TestSubmitReplaceIsPartOfTheSubmissionIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplaceFixture(t, false)
	f.seedItem(t, "replace-failure", domain.AttentionExecutionFailure)
	// A retry takes only the saved inputs; --replace with it is refused.
	if _, err := runSubmitCommand(ctx, submitCommandConfig{
		DBPath: f.cfg.DBPath, RetrySubmissionID: "replaced", ReplaceTaskID: f.first.TaskID,
	}); err == nil || !strings.Contains(err.Error(), "manual retry takes only") {
		t.Fatalf("retry with --replace: %v, want a refusal", err)
	}
	// The first submission's identity saved no replacement, so the same
	// identity with one is a different request.
	cfg := f.cfg
	cfg.ReplaceTaskID = f.first.TaskID
	if _, err := runSubmitCommand(ctx, cfg); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("same identity with --replace: %v, want an immutable conflict", err)
	}
}
