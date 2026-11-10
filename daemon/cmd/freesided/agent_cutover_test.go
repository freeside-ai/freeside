package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// flagEraReviewConfig is a daemon configuration as the removed flags set it
// for the adoption fixture's two identities, with the shadow arm on, and the
// Codex provider the fixture's reviewer line selects.
func flagEraReviewConfig() ClaudeDriverConfig {
	return ClaudeDriverConfig{
		ReviewProvider: mustRoutedReviewProvider(domain.HarnessClientCodexCLI),
		ExporterImage:  "ghcr.io/x/exporter@sha256:" + strings.Repeat("b", 64),
		OperatingMode:  domain.ModeUnattended,
		ReviewImage:    "ghcr.io/x/codex@sha256:" + strings.Repeat("c", 64),
		ReviewAuthMode: ward.CodexAuthSubscription, ReviewAuthIdentityID: "codex-review",
		ReviewModel: "gpt-fixture-1", ReviewReasoningEffort: "high",
		ReviewCostOwner: "review-operator", ReviewWorkspaceSizeMB: 8192,
		ShadowReviewImage:          "ghcr.io/x/claude@sha256:" + strings.Repeat("e", 64),
		ShadowReviewAuthIdentityID: "claude-main",
		ShadowReviewModel:          "claude-opus", ShadowReviewReasoningEffort: "high",
		ShadowReviewCostOwner: "operator", ShadowReviewWorkspaceSizeMB: 4096,
		ShadowReviewRate: 0.2,
	}
}

// TestLineupResolvedReviewDigestsEqualTheFlagEraDigests pins both review
// configuration digests across the cutover: the identity, cost owner, model,
// and effort the reviewer and shadow reviewer lines resolve produce the
// digests the removed flags produced, so no operator approval moves.
func TestLineupResolvedReviewDigestsEqualTheFlagEraDigests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.shadowArgs())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := loadAdoptedPatch(t, patch)
	flags := flagEraReviewConfig()
	wantReview, err := ClaudeReviewConfigurationDigest(flags)
	if err != nil {
		t.Fatal(err)
	}
	wantShadow, err := ShadowReviewConfigurationDigests(flags)
	if err != nil {
		t.Fatal(err)
	}

	cfg := flags
	cfg.ReviewProvider = ward.RoutedReviewProvider{}
	cfg.ReviewAuthIdentityID, cfg.ReviewCostOwner = "", ""
	cfg.ReviewModel, cfg.ReviewReasoningEffort = "", ""
	cfg.ShadowReviewAuthIdentityID, cfg.ShadowReviewCostOwner = "", ""
	cfg.ShadowReviewModel, cfg.ShadowReviewReasoningEffort = "", ""
	f.withStore(t, func(st *store.Store) {
		cfg, err = ResolveReviewSelection(ctx, st, tree, cfg, time.Now().UTC())
	})
	if err != nil {
		t.Fatalf("resolve review selection: %v", err)
	}
	if !reflect.DeepEqual(cfg, flags) {
		t.Fatalf("lineup-resolved configuration = %+v, want the flag-era %+v", cfg, flags)
	}
	gotReview, err := ClaudeReviewConfigurationDigest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gotShadow, err := ShadowReviewConfigurationDigests(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if gotReview != wantReview || gotShadow != wantShadow {
		t.Fatalf("digests moved: review %s want %s; shadow %+v want %+v", gotReview, wantReview, gotShadow, wantShadow)
	}
	golden.Assert(t, "cutover-review-configuration-digests",
		[]byte(string(gotReview)+"\n"+string(gotShadow.Runtime)+"\n"+string(gotShadow.Approval)+"\n"))
}

// TestPreflightResolvesTheAdoptedLineup covers what preflight probes: the
// writer roles' one credential volume and the review identities, read from
// the lineup the daemon will start with.
func TestPreflightResolvesTheAdoptedLineup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := loadAdoptedPatch(t, patch)
	revision := domain.Digest("sha256:" + strings.Repeat("4", 64))
	f.withStore(t, func(st *store.Store) {
		now := time.Now().UTC()
		got, err := resolvePreflightAgents(ctx, st, tree, revision, ward.CodexAuthSubscription, false, now)
		want := preflightAgentSelection{
			LineupRevision: revision, AuthIdentityID: "claude-main", AuthVolume: adoptClaudeVolume,
			ReviewProvider:       mustRoutedReviewProvider(domain.HarnessClientCodexCLI),
			ReviewAuthIdentityID: "codex-review", ReviewCostOwner: "review-operator",
			ReviewModel: "gpt-fixture-1", ReviewReasoningEffort: "high",
		}
		if err != nil || got != want {
			t.Fatalf("preflight agents = %+v, %v; want %+v", got, err, want)
		}
		// A setup token is not a mode the Codex reviewer accepts (#397).
		_, err = resolvePreflightAgents(ctx, st, tree, revision, ward.CodexAuthSetupToken, false, now)
		var mismatch *ReviewProviderMismatch
		if !errors.As(err, &mismatch) || !errors.Is(err, engine.ErrAgentNotAdmissible) {
			t.Fatalf("Codex reviewer under setup_token = %v", err)
		}
		// The shadow arm is on but adoption emitted no shadow reviewer line.
		_, err = resolvePreflightAgents(ctx, st, tree, revision, ward.CodexAuthSubscription, true, now)
		var failure *RoleAdmissionError
		if !errors.As(err, &failure) || failure.Role != domain.RoleShadowReviewer {
			t.Fatalf("preflight agents with the shadow arm on = %v", err)
		}
	})
}

// TestReviewLaunchSendsTheHarnessNativeEffort adopts both review agents at
// max: the reviewer's launch gets xhigh, Codex's highest level, and the
// shadow reviewer's gets max, which Claude Code has. Preflight resolves the
// same pair, so it approves the digest the daemon will run.
func TestReviewLaunchSendsTheHarnessNativeEffort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.shadowArgs("-review-effort", "max", "-shadow-review-effort", "max"))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := loadAdoptedPatch(t, patch)
	want := flagEraReviewConfig()
	want.ReviewReasoningEffort, want.ShadowReviewReasoningEffort = "xhigh", "max"
	cfg := want
	cfg.ReviewProvider = ward.RoutedReviewProvider{}
	cfg.ReviewModel, cfg.ReviewReasoningEffort = "", ""
	cfg.ShadowReviewModel, cfg.ShadowReviewReasoningEffort = "", ""
	f.withStore(t, func(st *store.Store) {
		now := time.Now().UTC()
		got, err := ResolveReviewSelection(ctx, st, tree, cfg, now)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("lineup-resolved configuration = %+v, %v; want %+v", got, err, want)
		}
		agents, err := resolvePreflightAgents(ctx, st, tree, domain.Digest("sha256:"+strings.Repeat("4", 64)), ward.CodexAuthSubscription, true, now)
		if err != nil || agents.ReviewModel != want.ReviewModel || agents.ReviewReasoningEffort != "xhigh" ||
			agents.ShadowReviewModel != want.ShadowReviewModel || agents.ShadowReviewReasoningEffort != "max" {
			t.Fatalf("preflight agents = %+v, %v", agents, err)
		}
	})
}

// treeWithReviewLinesNamingNoLaunch rewrites an adopted tree's review lines
// to name what their launch would not pass: the reviewer line asking
// harness_default, which sends no effort, and the shadow reviewer line on
// the native-default model id, which names no model. Adoption no longer
// writes either, but a hand-edited lineup can, so the only thing wrong with
// the tree is what its lines lack.
func treeWithReviewLinesNamingNoLaunch(t *testing.T, tree agenttree.Tree) agenttree.Tree {
	t.Helper()
	old := tree
	old.Lineup = slices.Clone(tree.Lineup)
	for i := range old.Lineup {
		names := &old.Lineup[i].Selection.Names
		if role := domain.RoleName(old.Lineup[i].Key); role == domain.RoleReviewer {
			names.Effort = string(domain.EffortHarnessDefault)
		} else if role == domain.RoleShadowReviewer {
			names.Model = domain.ClaudeCodeNativeDefaultRouteModelID
		}
	}
	return old
}

// TestReviewRoleNeedsAModelAndAnEffort runs this build on a tree whose
// review lines name no model or no effort for the launch to pass. Each
// review role is refused by name with what its line lacks, at startup, in preflight,
// and before a review, instead of launching on the harness's own defaults.
func TestReviewRoleNeedsAModelAndAnEffort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.shadowArgs())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	old := treeWithReviewLinesNamingNoLaunch(t, loadAdoptedPatch(t, patch))
	f.withStore(t, func(st *store.Store) {
		for role, lacks := range map[domain.RoleName]string{
			domain.RoleReviewer:       "names the harness-default effort, which sends none",
			domain.RoleShadowReviewer: "names the native-default model",
		} {
			_, err := ReviewRoleAgent(ctx, st, old, role, now)
			var failure *RoleAdmissionError
			if !errors.As(err, &failure) || failure.Role != role || !errors.Is(err, engine.ErrAgentNotAdmissible) ||
				!strings.Contains(err.Error(), "role "+string(role)) || !strings.Contains(err.Error(), lacks) {
				t.Fatalf("%s on the edited tree = %v, want it refused because its line %s", role, err, lacks)
			}
			// The identity is never compared: the line is refused first, so
			// the refusal has to name what the line lacks.
			admit := ReviewAdmission(st, old, testReviewRole(role, nil), time.Now)
			if err := admit(ctx, exec.ReviewRequest{}); !errors.Is(err, engine.ErrAgentNotAdmissible) ||
				!strings.Contains(err.Error(), lacks) {
				t.Fatalf("%s review admission on the edited tree = %v, want it refused because its line %s", role, err, lacks)
			}
		}
		var failure *RoleAdmissionError
		if _, err := ResolveReviewSelection(ctx, st, old, flagEraReviewConfig(), now); !errors.As(err, &failure) ||
			failure.Role != domain.RoleReviewer {
			t.Fatalf("startup selection on the edited tree = %v", err)
		}
		_, err := resolvePreflightAgents(ctx, st, old, domain.Digest("sha256:"+strings.Repeat("4", 64)), ward.CodexAuthSubscription, true, now)
		if !errors.As(err, &failure) || failure.Role != domain.RoleReviewer {
			t.Fatalf("preflight agents on the edited tree = %v", err)
		}
	})
}

// TestPreflightResolvesAWriterLinesSelection edits a writer line to an effort
// no Claude ward adapter sends: preflight refuses the role by name, rather
// than passing a line the daemon's startup check would hold.
func TestPreflightResolvesAWriterLinesSelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := loadAdoptedPatch(t, patch)
	tree.Lineup = slices.Clone(tree.Lineup)
	for i := range tree.Lineup {
		if domain.RoleName(tree.Lineup[i].Key) == domain.RoleRemediator {
			tree.Lineup[i].Selection.Names.Effort = "no-such-effort"
		}
	}
	f.withStore(t, func(st *store.Store) {
		_, err := resolvePreflightAgents(ctx, st, tree, domain.Digest("sha256:"+strings.Repeat("4", 64)),
			ward.CodexAuthSubscription, false, time.Now().UTC())
		var failure *RoleAdmissionError
		if !errors.As(err, &failure) || failure.Role != domain.RoleRemediator || !strings.Contains(err.Error(), "no-such-effort") {
			t.Fatalf("preflight agents with an unsendable remediator effort = %v", err)
		}
	})
}

// TestReviewRoleRefusesAModelTheLaunchCannotPass hand-authors each review
// line with a model that is no single argument: a comma, which the review
// launch's own rule refuses, and a leading dash, which a CLI would read as
// another option and the lineup's launch-option rule refuses when the role
// reads its line.
// Adoption refuses both as flags; a committed line is held to the same rule.
func TestReviewRoleRefusesAModelTheLaunchCannotPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.shadowArgs())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	adopted := loadAdoptedPatch(t, patch)
	for _, tc := range []struct {
		role  domain.RoleName
		model string
		want  string
	}{
		{domain.RoleReviewer, "gpt,fixture", "cannot pass as one argument"},
		{domain.RoleShadowReviewer, "--dangerously-skip-permissions", "each one launch option value"},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			tree := adopted
			tree.Lineup = slices.Clone(adopted.Lineup)
			for i := range tree.Lineup {
				if domain.RoleName(tree.Lineup[i].Key) == tc.role {
					tree.Lineup[i].Selection.Names.Model = tc.model
				}
			}
			f.withStore(t, func(st *store.Store) {
				_, err := ReviewRoleAgent(ctx, st, tree, tc.role, now)
				var failure *RoleAdmissionError
				if !errors.As(err, &failure) || failure.Role != tc.role || !errors.Is(err, engine.ErrAgentNotAdmissible) ||
					!strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%s on a line naming %q = %v, want it refused for the model's shape", tc.role, tc.model, err)
				}
			})
		})
	}
}

// TestReviewSelectionFailsClosed covers the review roles' startup and
// per-review checks: a lineup with no reviewer line refuses and names the
// role, an attended daemon resolves no review role, an offer past its
// not_after no longer refuses (a selection names no offer), and a source
// composed under an identity refuses its next review once the identity is
// disabled.
func TestReviewSelectionFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("no reviewer line", func(t *testing.T) {
		f := newAuthAdoptFixture(t)
		// One identity under both flags adopts no review agent.
		_, patch, err := f.run(t, f.args("-review-auth-identity", "claude-main", "-review-cost-owner", "operator"))
		if err != nil {
			t.Fatalf("auth adopt: %v", err)
		}
		tree := loadAdoptedPatch(t, patch)
		f.withStore(t, func(st *store.Store) {
			cfg := flagEraReviewConfig()
			_, err := ResolveReviewSelection(ctx, st, tree, cfg, now)
			var failure *RoleAdmissionError
			if !errors.As(err, &failure) || failure.Role != domain.RoleReviewer ||
				!errors.Is(err, engine.ErrAgentNotAdmissible) {
				t.Fatalf("resolve without a reviewer line = %v", err)
			}
			cfg.OperatingMode = domain.ModeAttendedDev
			if got, err := ResolveReviewSelection(ctx, st, tree, cfg, now); err != nil || !reflect.DeepEqual(got, cfg) {
				t.Fatalf("attended daemon resolved a review role: %+v, %v", got, err)
			}
		})
	})

	t.Run("no shadow reviewer line", func(t *testing.T) {
		f := newAuthAdoptFixture(t)
		_, patch, err := f.run(t, f.args())
		if err != nil {
			t.Fatalf("auth adopt: %v", err)
		}
		tree := loadAdoptedPatch(t, patch)
		f.withStore(t, func(st *store.Store) {
			_, err := ResolveReviewSelection(ctx, st, tree, flagEraReviewConfig(), now)
			var failure *RoleAdmissionError
			if !errors.As(err, &failure) || failure.Role != domain.RoleShadowReviewer {
				t.Fatalf("resolve without a shadow reviewer line = %v", err)
			}
		})
	})

	t.Run("identity disabled after composition", func(t *testing.T) {
		f := newAuthAdoptFixture(t)
		_, patch, err := f.run(t, f.args())
		if err != nil {
			t.Fatalf("auth adopt: %v", err)
		}
		tree := loadAdoptedPatch(t, patch)
		f.withStore(t, func(st *store.Store) {
			req := admittedReviewRequest(t, st, tree, domain.RoleReviewer, seedReviewedRun(t, st, "run-reviewed").ID)
			admit := ReviewAdmission(st, tree, testReviewRole(domain.RoleReviewer, nil), time.Now)
			if err := admit(ctx, req); err != nil {
				t.Fatalf("review admission of the adopted reviewer: %v", err)
			}
			setIdentityEnabled(t, st, "codex-review", false)
			var failure *RoleAdmissionError
			if err := admit(ctx, req); !errors.As(err, &failure) || failure.Role != domain.RoleReviewer ||
				!errors.Is(err, engine.ErrAgentNotAdmissible) {
				t.Fatalf("review admission after the identity was disabled = %v", err)
			}
		})
	})
}

// seedReviewedRun persists a run, which binds it to a task, and records the
// submit_task command that created that task: the record a task line's
// set_by names.
func seedReviewedRun(t *testing.T, st *store.Store, id domain.RunID) domain.Run {
	t.Helper()
	ctx := context.Background()
	run := domain.Run{ID: id, ProjectID: "project-review", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		var err error
		if run, err = tx.GetRun(ctx, id); err != nil {
			return err
		}
		return recordReviewedRunCommand(ctx, tx, run, "cmd-"+string(id))
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

// recordReviewedRunCommand records a submit_task command that created the
// run's task and carried the lines.
func recordReviewedRunCommand(
	ctx context.Context, tx *store.WriteTx, run domain.Run, commandID string, lines ...domain.TaskLineChoice,
) error {
	return tx.PutTaskSubmissionRequest(ctx, domain.TaskSubmission{
		CommandID: commandID, Origin: domain.TaskSubmissionOriginDevice, DeviceID: new(domain.DeviceID("device-1")), ProjectID: run.ProjectID,
		SourceDigest: domain.Digest("sha256:" + strings.Repeat("4", 64)), TaskID: run.TaskID,
		SpecificationRunID: run.ID,
		Name:               domain.DisplayName{Text: "Reviewed task", Source: domain.DisplayNameSourceOperator},
		TaskLines:          lines,
	}, domain.Digest("sha256:"+strings.Repeat("c", 64)))
}

// submitReviewedRunLine records a line on the run's task as submit_task does:
// set by a command that carried it, at the task's creation instant.
func submitReviewedRunLine(
	t *testing.T, st *store.Store, run domain.Run, role domain.RoleName, selection domain.TaskLineSelection,
) domain.TaskLine {
	t.Helper()
	ctx := context.Background()
	commandID := "cmd-" + string(run.ID) + "-" + string(role)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return recordReviewedRunCommand(ctx, tx, run, commandID, domain.TaskLineChoice{Role: role, Selection: &selection})
	}); err != nil {
		t.Fatal(err)
	}
	return setReviewedRunLine(t, st, run, role, selection, commandID, 0)
}

// setReviewedRunLine records a line on the run's task, set by setBy at the
// task's creation instant plus skew.
func setReviewedRunLine(
	t *testing.T, st *store.Store, run domain.Run, role domain.RoleName, selection domain.TaskLineSelection, setBy string,
	skew time.Duration,
) domain.TaskLine {
	t.Helper()
	ctx := context.Background()
	var line domain.TaskLine
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetTask(ctx, run.TaskID)
		if err != nil {
			return err
		}
		line, err = tx.AppendTaskLine(ctx, domain.TaskLineInput{
			TaskID: task.ID, Role: role, Selection: &selection, Source: domain.TaskLineSourceSubmitTask, SetBy: setBy,
		}, task.CreatedAt.Add(skew))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return line
}

// TestReviewAdmissionHoldsTheReviewerTaskLine: a task's reviewer line selects
// the agent its review runs, within the harness the source was composed for
// (#1981). The recheck accepts the snapshot of the agent the line selects and
// refuses one naming another identity, model, or effort. A line naming an
// agent on another harness is refused, and so is one that does not hold
// against the command that set it. The shadow reviewer takes no task line.
func TestReviewAdmissionHoldsTheReviewerTaskLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.shadowArgs())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := loadAdoptedPatch(t, patch)
	// A second selection on the reviewer's enrollment, at another effort,
	// which a line can select.
	second := lineupSelection(t, tree, domain.RoleReviewer)
	for _, effort := range domain.CodexCLISendableEfforts() {
		if string(effort) != second.Effort && effort != domain.EffortHarnessDefault {
			second.Effort = string(effort)
			break
		}
	}
	now := time.Now().UTC()
	f.withStore(t, func(st *store.Store) {
		reviewer, err := ReviewRoleAgent(ctx, st, tree, domain.RoleReviewer, now)
		if err != nil {
			t.Fatal(err)
		}
		shadow, err := ReviewRoleAgent(ctx, st, tree, domain.RoleShadowReviewer, now)
		if err != nil {
			t.Fatal(err)
		}
		admit := ReviewAdmission(st, tree, testReviewRole(domain.RoleReviewer, nil), time.Now)
		admitShadow := ReviewAdmission(st, tree, testReviewRole(domain.RoleShadowReviewer, nil), time.Now)
		refused := func(t *testing.T, req exec.ReviewRequest, want string) {
			t.Helper()
			err := admit(ctx, req)
			var failure *RoleAdmissionError
			if !errors.As(err, &failure) || failure.Role != domain.RoleReviewer ||
				!errors.Is(err, engine.ErrAgentNotAdmissible) || !strings.Contains(err.Error(), want) {
				t.Fatalf("review admission = %v, want a reviewer refusal naming %q", err, want)
			}
		}

		t.Run("no reviewer line", func(t *testing.T) {
			run := seedReviewedRun(t, st, "run-lineless")
			// A line for another role is not the reviewer's.
			submitReviewedRunLine(t, st, run, domain.RoleImplementer, domain.TaskLineSelection{
				EnrollmentID: "enroll-other", ModelID: "model-other", Effort: "high",
			})
			req := admittedReviewRequest(t, st, tree, domain.RoleReviewer, run.ID)
			if err := admit(ctx, req); err != nil {
				t.Fatalf("review admission with no reviewer line: %v", err)
			}
			// The source launches the identity, model, and effort the snapshot
			// names, so a snapshot naming another than the line selects, or the
			// other review role, is refused.
			for name, change := range map[string]func(*domain.ReviewAdmission){
				"another role":     func(a *domain.ReviewAdmission) { a.Role = domain.RoleShadowReviewer },
				"another identity": func(a *domain.ReviewAdmission) { a.AuthIdentityID = shadow.Identity.ID },
				"another model":    func(a *domain.ReviewAdmission) { a.AgentBinding.RouteModelID += "-next" },
				"another effort":   func(a *domain.ReviewAdmission) { a.AgentBinding.NativeEffort += "er" },
			} {
				changed := req
				change(&changed.Admission)
				refused(t, changed, "the invocation was admitted as")
				if err := admit(ctx, changed); err == nil || !strings.Contains(err.Error(), "the line now selects") {
					t.Fatalf("review admission of a snapshot naming %s = %v", name, err)
				}
			}
		})

		t.Run("the engine's admission is the one the source accepts", func(t *testing.T) {
			run := seedReviewedRun(t, st, "run-admitted")
			revision, err := treeRevision(tree)
			if err != nil {
				t.Fatal(err)
			}
			egress := map[domain.RoleName][]string{
				domain.RoleReviewer:       {"chatgpt.com:443"},
				domain.RoleShadowReviewer: {"api.anthropic.com:443"},
			}
			admitter := ReviewAdmitter(st, engine.AgentSelection{
				Tree: tree, LineupRevision: revision, Launch: agentbaseline.RoleLaunch,
			}, testReviewRoles(egress))
			for role, arm := range map[domain.RoleName]struct {
				agent    engine.ReviewRoleAgent
				source   func(context.Context, exec.ReviewRequest) error
				provider string
			}{
				domain.RoleReviewer:       {reviewer, admit, "openai"},
				domain.RoleShadowReviewer: {shadow, admitShadow, "anthropic"},
			} {
				id := domain.InvocationID("review-admitted-" + string(role))
				admitted, err := admitter(ctx, role, run.ID, id, now)
				if err != nil {
					t.Fatalf("admit the %s invocation: %v", role, err)
				}
				selection := arm.agent.Selection
				lineage := domain.LineageGroupOf(arm.agent.Adapter.ClientKind, selection.ModelID)
				if admitted.InvocationID != id || admitted.RunID != run.ID || admitted.Role != role ||
					admitted.AuthIdentityID != arm.agent.Identity.ID || admitted.LineageGroup != lineage || lineage == "" ||
					admitted.AgentBinding.AgentDigest != selection.Digest ||
					admitted.AgentBinding.SelectionDigest != selection.Digest ||
					admitted.AgentBinding.LineupRevision != revision ||
					admitted.ProviderLabel() != arm.provider ||
					admitted.ModelConfiguration() != arm.agent.Model+"/"+arm.agent.NativeEffort ||
					!slices.Equal(admitted.AgentBinding.EffectiveEgress, egress[role]) ||
					!admitted.AdmittedAt.Equal(now) {
					t.Fatalf("%s admission = %+v", role, admitted)
				}
				if err := arm.source(ctx, exec.ReviewRequest{RunID: run.ID, Admission: admitted}); err != nil {
					t.Fatalf("the %s source refused the engine's admission: %v", role, err)
				}
			}
			// The reviewer line of this task names an agent on another
			// harness: the engine refuses the invocation before any snapshot
			// exists. The shadow reviewer reads no line.
			submitReviewedRunLine(t, st, run, domain.RoleReviewer, lineupSelection(t, tree, domain.RoleShadowReviewer))
			var failure *RoleAdmissionError
			if _, err := admitter(ctx, domain.RoleReviewer, run.ID, "review-admitted-refused", now); !errors.As(err, &failure) ||
				failure.Role != domain.RoleReviewer || !errors.Is(err, engine.ErrAgentNotAdmissible) {
				t.Fatalf("admit a reviewer invocation whose task line names an agent on another harness = %v", err)
			}
			if _, err := admitter(ctx, domain.RoleShadowReviewer, run.ID, "review-admitted-shadow-2", now); err != nil {
				t.Fatalf("admit a shadow invocation beside a refused reviewer line: %v", err)
			}
		})

		t.Run("the line names the agent the source runs", func(t *testing.T) {
			run := seedReviewedRun(t, st, "run-same-agent")
			submitReviewedRunLine(t, st, run, domain.RoleReviewer, lineupSelection(t, tree, domain.RoleReviewer))
			if err := admit(ctx, admittedReviewRequest(t, st, tree, domain.RoleReviewer, run.ID)); err != nil {
				t.Fatalf("review admission with a line naming the composed agent: %v", err)
			}
		})

		t.Run("the line selects another agent", func(t *testing.T) {
			run := seedReviewedRun(t, st, "run-second-agent")
			line := submitReviewedRunLine(t, st, run, domain.RoleReviewer, second)
			revision, err := treeRevision(tree)
			if err != nil {
				t.Fatal(err)
			}
			admission, err := engine.AgentSelection{
				Tree: tree, LineupRevision: revision, Launch: agentbaseline.RoleLaunch,
			}.AdmitReview(ctx, st, testReviewRole(domain.RoleReviewer, []string{"chatgpt.com:443"}),
				run.ID, "review-second-agent", time.Now().UTC())
			if err != nil {
				t.Fatalf("admit a review whose line selects another agent: %v", err)
			}
			// The selection record is what shows the line chose it.
			if admission.AgentBinding.SelectionSource != domain.AgentSelectionSourceTaskLine ||
				admission.AgentBinding.SelectionRecordID != line.ID {
				t.Fatalf("admission = %+v, want the line's agent cited by the line", admission.AgentBinding)
			}
			if err := admit(ctx, exec.ReviewRequest{RunID: run.ID, Admission: admission}); err != nil {
				t.Fatalf("recheck of the line's agent: %v", err)
			}
		})

		t.Run("the line names an agent on another harness", func(t *testing.T) {
			run := seedReviewedRun(t, st, "run-other-agent")
			// The other agent is a real one that passes every admission step
			// for its own role: the refusal is the source's harness, not the
			// agent.
			if lineupSelection(t, tree, domain.RoleShadowReviewer) == lineupSelection(t, tree, domain.RoleReviewer) {
				t.Fatal("the fixture's two review roles share an agent")
			}
			line := submitReviewedRunLine(t, st, run, domain.RoleReviewer, lineupSelection(t, tree, domain.RoleShadowReviewer))
			req := exec.ReviewRequest{RunID: run.ID}
			refused(t, req, string(line.ID))
			refused(t, req, "harness client")
			// The shadow reviewer reads no line, this task's included.
			if err := admitShadow(ctx, admittedReviewRequest(t, st, tree, domain.RoleShadowReviewer, run.ID)); err != nil {
				t.Fatalf("shadow review admission beside a refused reviewer line: %v", err)
			}
		})

		t.Run("the refusal never repeats the line's agent", func(t *testing.T) {
			run := seedReviewedRun(t, st, "run-pasted-agent")
			// A credential pasted as the model can pass the selection
			// grammar. The refusal becomes the review failure's recorded
			// reason, so it names the line and not the text.
			const pasted = "0123456789abcdef0123456789abcdef"
			selection := lineupSelection(t, tree, domain.RoleReviewer)
			selection.ModelID = pasted
			line := submitReviewedRunLine(t, st, run, domain.RoleReviewer, selection)
			req := exec.ReviewRequest{RunID: run.ID}
			refused(t, req, string(line.ID))
			if err := admit(ctx, req); strings.Contains(err.Error(), pasted) {
				t.Fatalf("review admission = %v, which repeats the line's agent", err)
			}
		})

		t.Run("the line does not hold against its command", func(t *testing.T) {
			// Each line names the composed agent, so only the anchor refuses.
			unrecorded := seedReviewedRun(t, st, "run-unrecorded")
			setReviewedRunLine(t, st, unrecorded, domain.RoleReviewer, lineupSelection(t, tree, domain.RoleReviewer), "cmd-never-recorded", 0)
			refused(t, exec.ReviewRequest{RunID: unrecorded.ID}, "cmd-never-recorded")

			borrowed := seedReviewedRun(t, st, "run-borrowed")
			setReviewedRunLine(t, st, borrowed, domain.RoleReviewer, lineupSelection(t, tree, domain.RoleReviewer), "cmd-run-same-agent", 0)
			refused(t, exec.ReviewRequest{RunID: borrowed.ID}, "that command created task")

			late := seedReviewedRun(t, st, "run-late")
			setReviewedRunLine(t, st, late, domain.RoleReviewer, lineupSelection(t, tree, domain.RoleReviewer), "cmd-run-late", time.Second)
			refused(t, exec.ReviewRequest{RunID: late.ID}, "its task was created at")
		})

		t.Run("the request names no stored run", func(t *testing.T) {
			// Whether the task has a line cannot be read, so the review is
			// refused instead of run on the assumption that it has none.
			refused(t, exec.ReviewRequest{RunID: "run-never-stored"}, "run-never-stored")
		})
	})
}

// TestReviewInvocationIsRefusedBeforeItIsAdmitted admits a review invocation
// for each review role and then breaks, one at a time, what review admission
// rests on. Each refusal names the role, and none returns a snapshot the
// engine could store: a refused invocation is never requested.
func TestReviewInvocationIsRefusedBeforeItIsAdmitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()
	other := map[domain.RoleName]domain.RoleName{
		domain.RoleReviewer:       domain.RoleShadowReviewer,
		domain.RoleShadowReviewer: domain.RoleReviewer,
	}
	lineOf := func(tree agenttree.Tree, role domain.RoleName) *domain.LineupSelection {
		for i := range tree.Lineup {
			if domain.RoleName(tree.Lineup[i].Key) == role {
				return &tree.Lineup[i].Selection
			}
		}
		t.Fatalf("the adopted lineup has no %s line", role)
		return nil
	}
	type fixture struct {
		tree agenttree.Tree
		st   *store.Store
		run  domain.Run
		at   time.Time
	}
	for name, tc := range map[string]struct {
		roles   []domain.RoleName
		breakIt func(*testing.T, *fixture, domain.RoleName, engine.ReviewRoleAgent)
		want    string
	}{
		"the line selects another prompt": {
			breakIt: func(_ *testing.T, f *fixture, role domain.RoleName, _ engine.ReviewRoleAgent) {
				lineOf(f.tree, role).PromptDigest = domain.Digest("sha256:" + strings.Repeat("7", 64))
			},
			want: "this daemon reviews with",
		},
		"the line selects a selection on the other harness": {
			breakIt: func(_ *testing.T, f *fixture, role domain.RoleName, _ engine.ReviewRoleAgent) {
				lineOf(f.tree, role).Names = lineOf(f.tree, other[role]).Names
			},
			want: "runs harness client",
		},
		"the line names an enrollment the store lacks": {
			breakIt: func(_ *testing.T, f *fixture, role domain.RoleName, _ engine.ReviewRoleAgent) {
				lineOf(f.tree, role).Names.Enrollment = "absent-identity/claude_code"
			},
			want: "its enrollment cannot be read",
		},
		"the identity holds no current credential": {
			breakIt: func(t *testing.T, f *fixture, _ domain.RoleName, agent engine.ReviewRoleAgent) {
				setIdentityEnabled(t, f.st, agent.Identity.ID, false)
			},
			want: "role ",
		},
		// Only the routed reviewer reads a task line.
		"the task's line names an agent on another harness": {
			roles: []domain.RoleName{domain.RoleReviewer},
			breakIt: func(t *testing.T, f *fixture, role domain.RoleName, _ engine.ReviewRoleAgent) {
				submitReviewedRunLine(t, f.st, f.run, role, lineupSelection(t, f.tree, other[role]))
			},
			want: "harness client",
		},
	} {
		roles := tc.roles
		if roles == nil {
			roles = []domain.RoleName{domain.RoleReviewer, domain.RoleShadowReviewer}
		}
		for _, role := range roles {
			t.Run(name+"/"+string(role), func(t *testing.T) {
				adopt := newAuthAdoptFixture(t)
				_, patch, err := adopt.run(t, adopt.shadowArgs())
				if err != nil {
					t.Fatalf("auth adopt: %v", err)
				}
				adopted := loadAdoptedPatch(t, patch)
				adopted.Lineup = slices.Clone(adopted.Lineup)
				revision, err := treeRevision(adopted)
				if err != nil {
					t.Fatal(err)
				}
				adopt.withStore(t, func(st *store.Store) {
					f := &fixture{tree: adopted, st: st, run: seedReviewedRun(t, st, "run-refused"), at: now}
					admit := func(id domain.InvocationID) (domain.ReviewAdmission, error) {
						return ReviewAdmitter(st, engine.AgentSelection{
							Tree: f.tree, LineupRevision: revision, Launch: agentbaseline.RoleLaunch,
						}, testReviewRoles(map[domain.RoleName][]string{role: {"review.example:443"}}))(ctx, role, f.run.ID, id, f.at)
					}
					if _, err := admit("review-before"); err != nil {
						t.Fatalf("admit the %s invocation on the adopted tree: %v", role, err)
					}
					agent, err := ReviewRoleAgent(ctx, st, f.tree, role, now)
					if err != nil {
						t.Fatal(err)
					}
					tc.breakIt(t, f, role, agent)
					admitted, err := admit("review-after")
					var failure *RoleAdmissionError
					if !errors.As(err, &failure) || failure.Role != role ||
						!errors.Is(err, engine.ErrAgentNotAdmissible) || !strings.Contains(err.Error(), tc.want) {
						t.Fatalf("admit the %s invocation = %v, want a refusal naming %q", role, err, tc.want)
					}
					if admitted.InvocationID != "" || admitted.AuthIdentityID != "" {
						t.Fatalf("a refused admission returned a snapshot: %+v", admitted)
					}
				})
			})
		}
	}
}

// TestWriterRoleFailureHoldsAdmissionAndRaisesOneItem covers the startup
// check for the writer roles: a role whose line names another prompt closes
// the gate, raises one system_health item naming the role, raises no second
// item on the next start, and the item resolves once the role admits.
func TestWriterRoleFailureHoldsAdmissionAndRaisesOneItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	checkout, commit := commitAdoptedPatch(t, patch)
	selection, err := LoadAgentSelection(ctx, ClaudeDriverConfig{
		AgentTreeCheckout: checkout, AgentTreeCommit: commit,
		ProviderEndpoints: []string{"api.anthropic.com:443"},
	})
	if err != nil {
		t.Fatal(err)
	}
	selection.AttemptBudget = time.Hour
	adopted, err := readAdoptPrompts(authAdoptConfig{
		PromptPackage:              filepath.Join(f.promptDir, "implementer"),
		SpecificationPromptPackage: filepath.Join(f.promptDir, "specifier"),
		RemediationPromptPackage:   filepath.Join(f.promptDir, "remediator"),
	})
	if err != nil {
		t.Fatal(err)
	}
	prompts := map[domain.RoleName]domain.Digest{}
	for role, prompt := range adopted {
		prompts[role] = prompt.Digest
	}
	f.withStore(t, func(st *store.Store) {
		now := time.Now().UTC()
		if err := recordTestClaudeAdapterConformance(ctx, st, now); err != nil {
			t.Fatal(err)
		}
		if failure := CheckWriterRoles(ctx, st, *selection, prompts, now); failure != nil {
			t.Fatalf("adopted writer roles: %v", failure)
		}
		if err := AgentSelectionGate(st, nil)(ctx); err != nil {
			t.Fatalf("gate with every role admitted: %v", err)
		}

		// The daemon now runs another remediation prompt than the line names.
		prompts[domain.RoleRemediator] = prompts[domain.RoleImplementer]
		failure := CheckWriterRoles(ctx, st, *selection, prompts, now)
		if failure == nil || failure.Role != domain.RoleRemediator || !errors.Is(failure, engine.ErrAgentNotAdmissible) {
			t.Fatalf("mismatched remediation prompt = %v", failure)
		}
		if err := AgentSelectionGate(st, failure)(ctx); !errors.Is(err, engine.ErrAgentNotAdmissible) {
			t.Fatalf("gate with a failed role = %v", err)
		}
		for range 2 {
			if err := ActivateAgentSelection(ctx, st, failure, now); err != nil {
				t.Fatal(err)
			}
		}
		items := agentSelectionItems(t, st)
		if len(items) != 1 || !strings.Contains(items[0].Reason, "role remediator") ||
			!strings.HasPrefix(string(items[0].ID), AgentSelectionItemPrefix+RoleCausePrefix+"remediator-") {
			t.Fatalf("items after a role failure = %+v", items)
		}
		// A start that fails before the engine exists reports the same item.
		if err := ReportRoleFailure(ctx, st, failure, now); err != nil {
			t.Fatal(err)
		}
		if got := agentSelectionItems(t, st); len(got) != 1 || got[0].ID != items[0].ID {
			t.Fatalf("items after reporting the same failure = %+v", got)
		}
		if err := ActivateAgentSelection(ctx, st, nil, now); err != nil {
			t.Fatal(err)
		}
		if got := agentSelectionItems(t, st); len(got) != 0 {
			t.Fatalf("items after the role admits = %+v", got)
		}
	})
}

func markStartupGeneration(t *testing.T, st *store.Store, id domain.ClientEnrollmentID) domain.GenerationIntegrityMark {
	t.Helper()
	ctx := t.Context()
	var mark domain.GenerationIntegrityMark
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		generation, err := tx.CurrentEnrollmentGeneration(ctx, id)
		if err != nil {
			return err
		}
		mark = domain.GenerationIntegrityMark{
			EnrollmentID: id, Ordinal: generation.Ordinal,
			Finding: domain.CredentialIntegrityCorruption, ObservedAt: time.Now().UTC(),
		}
		_, err = tx.RecordGenerationIntegrityMark(ctx, mark)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return mark
}

func reenrollStartupRole(t *testing.T, st *store.Store, enrollment domain.ClientEnrollment) {
	t.Helper()
	ctx, at := t.Context(), time.Now().UTC()
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		generation, err := tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
		if err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, enrollment.AuthIdentityID, "reenroll-startup",
			&domain.LeaseGenerationBinding{
				EnrollmentID: enrollment.ID, Generation: generation.Ordinal,
				AuthStoreVolume: generation.AuthStoreVolume, StoreManifestDigest: generation.StoreManifestDigest,
			}, at, at.Add(time.Minute))
		if err != nil {
			return err
		}
		generation.Ordinal, generation.LeaseFence, generation.RecordedAt = 0, lease.Fence, at
		if _, err := tx.AppendEnrollmentGeneration(ctx, generation, at); err != nil {
			return err
		}
		return tx.ReleaseAuthStoreMutationLease(ctx, enrollment.AuthIdentityID, "reenroll-startup", lease.Fence, at)
	}); err != nil {
		t.Fatal(err)
	}
}

// enrollSecondSubscription enrolls a second Claude identity whose one
// generation copies source's, on its own auth-store volume, and returns the
// new enrollment. A test moves lineup lines onto it to put roles on a
// healthy subscription beside a marked one.
func enrollSecondSubscription(t *testing.T, st *store.Store, source engine.RoleAgent, at time.Time) domain.ClientEnrollment {
	t.Helper()
	ctx := t.Context()
	identity, enrollment, generation := source.Identity, source.Enrollment, source.Generation
	identity.ID, identity.AccountBinding = "claude-second", "second@example.test"
	enrollment.ID, enrollment.AuthIdentityID = "claude-second/claude_code", identity.ID
	enrollment.AccountBinding = identity.AccountBinding
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordAuthIdentity(ctx, identity, at); err != nil {
			return err
		}
		if err := tx.RecordClientEnrollment(ctx, enrollment, at); err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, identity.ID, "second-enroll",
			&domain.LeaseGenerationBinding{
				EnrollmentID: enrollment.ID, AuthStoreVolume: "second-auth", StoreManifestDigest: generation.StoreManifestDigest,
			}, at, at.Add(time.Minute))
		if err != nil {
			return err
		}
		generation.EnrollmentID, generation.AuthStoreVolume = enrollment.ID, "second-auth"
		generation.Ordinal = 0
		generation.AccountBinding, generation.LeaseFence, generation.RecordedAt = identity.AccountBinding, lease.Fence, at
		if _, err := tx.AppendEnrollmentGeneration(ctx, generation, at); err != nil {
			return err
		}
		return tx.ReleaseAuthStoreMutationLease(ctx, identity.ID, "second-enroll", lease.Fence, at)
	}); err != nil {
		t.Fatal(err)
	}
	return enrollment
}

func TestMarkedWriterRoleDoesNotHoldTheLineup(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatal(err)
	}
	tree := loadAdoptedPatch(t, patch)
	prompts := map[domain.RoleName]domain.Digest{}
	for _, line := range tree.Lineup {
		prompts[domain.RoleName(line.Key)] = line.Selection.PromptDigest
	}
	f.withStore(t, func(st *store.Store) {
		at := time.Now().UTC()
		if err := recordTestClaudeAdapterConformance(ctx, st, at); err != nil {
			t.Fatal(err)
		}
		source, err := engine.ResolveRole(ctx, st, tree, domain.RoleSpecifier)
		if err != nil {
			t.Fatal(err)
		}
		// A second subscription runs the implementer and remediator. The
		// specifier stays on the generation that will be marked.
		enrollment := enrollSecondSubscription(t, st, source, at)
		moved := lineupSelection(t, tree, domain.RoleImplementer)
		for i := range tree.Lineup {
			if tree.Lineup[i].Selection.Names == moved.Names() {
				tree.Lineup[i].Selection.Names.Enrollment = string(enrollment.ID)
			}
		}
		revision, err := treeRevision(tree)
		if err != nil {
			t.Fatal(err)
		}
		selection := engine.AgentSelection{
			Tree: tree, LineupRevision: revision, Launch: agentbaseline.RoleLaunch,
			EffectiveEgress: []string{"api.anthropic.com:443"}, AttemptBudget: time.Hour,
		}
		markStartupGeneration(t, st, source.Enrollment.ID)
		_, markedErr := selection.CheckRole(ctx, st, domain.RoleSpecifier, prompts[domain.RoleSpecifier], at)
		if !errors.Is(markedErr, domain.ErrGenerationIntegrityMarked) {
			t.Fatalf("marked role = %v", markedErr)
		}
		if err := ReportRoleFailure(ctx, st, &RoleAdmissionError{Role: domain.RoleSpecifier, Err: markedErr}, at); err != nil {
			t.Fatal(err)
		}
		if len(agentSelectionItems(t, st)) != 1 {
			t.Fatal("the earlier startup did not leave its role item")
		}
		failure := CheckWriterRoles(ctx, st, selection, prompts, at)
		if failure != nil {
			t.Fatal(failure)
		}
		selection.Gate = AgentSelectionGate(st, failure)
		if err := selection.Gate(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := selection.CheckRole(ctx, st, domain.RoleImplementer, prompts[domain.RoleImplementer], at); err != nil {
			t.Fatalf("healthy second subscription: %v", err)
		}
		if err := ActivateAgentSelection(ctx, st, failure, at); err != nil {
			t.Fatal(err)
		}
		if items := agentSelectionItems(t, st); len(items) != 0 {
			t.Fatalf("items after marked-only startup = %+v", items)
		}
		prompts[domain.RoleRemediator] = prompts[domain.RoleSpecifier]
		if failure := CheckWriterRoles(ctx, st, selection, prompts, at); failure == nil || failure.Role != domain.RoleRemediator {
			t.Fatalf("another role's real failure = %v", failure)
		}
		reenrollStartupRole(t, st, source.Enrollment)
		if _, err := selection.CheckRole(ctx, st, domain.RoleSpecifier, prompts[domain.RoleSpecifier], at); err != nil {
			t.Fatalf("same selection after re-enrollment: %v", err)
		}
	})
}

func TestReviewSelectionOnAMarkedGeneration(t *testing.T) {
	t.Parallel()
	for _, role := range []domain.RoleName{domain.RoleReviewer, domain.RoleShadowReviewer} {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			ctx, at := t.Context(), time.Now().UTC()
			f := newAuthAdoptFixture(t)
			_, patch, err := f.run(t, f.shadowArgs())
			if err != nil {
				t.Fatal(err)
			}
			tree := loadAdoptedPatch(t, patch)
			f.withStore(t, func(st *store.Store) {
				cfg, err := ResolveReviewSelection(ctx, st, tree, flagEraReviewConfig(), at)
				if err != nil {
					t.Fatal(err)
				}
				agent, err := ReviewRoleAgent(ctx, st, tree, role, at)
				if err != nil {
					t.Fatal(err)
				}
				run := seedReviewedRun(t, st, "run-marked-review")
				req := admittedReviewRequest(t, st, tree, role, run.ID)
				admit := ReviewAdmission(st, tree, testReviewRole(role, nil), func() time.Time { return at })
				mark := markStartupGeneration(t, st, agent.Enrollment.ID)
				got, err := ResolveReviewSelection(ctx, st, tree, flagEraReviewConfig(), at)
				if err != nil || !reflect.DeepEqual(got, cfg) {
					t.Fatalf("marked startup = %+v, %v; want %+v", got, err, cfg)
				}
				checked, err := engine.CheckReviewRole(ctx, st, tree, agentbaseline.RoleLaunch, testReviewRole(role, nil), "", at)
				var marked *domain.GenerationIntegrityMarkedError
				if !errors.Is(err, engine.ErrAgentNotAdmissible) || !errors.As(err, &marked) || marked.Mark != mark ||
					!reflect.DeepEqual(checked, agent) {
					t.Fatalf("marked review check = %+v, %v", checked, err)
				}
				if err := admit(ctx, req); !errors.Is(err, domain.ErrGenerationIntegrityMarked) {
					t.Fatalf("marked review launch = %v", err)
				}
				selection := engine.AgentSelection{Tree: tree, LineupRevision: req.Admission.AgentBinding.LineupRevision, Launch: agentbaseline.RoleLaunch}
				spec := testReviewRole(role, []string{"review.example:443"})
				refused, err := selection.AdmitReview(ctx, st, spec, run.ID, "review-marked", at)
				if !errors.Is(err, domain.ErrGenerationIntegrityMarked) || !reflect.DeepEqual(refused, domain.ReviewAdmission{}) {
					t.Fatalf("marked review admission = %+v, %v", refused, err)
				}
				_, err = resolvePreflightAgents(ctx, st, tree, req.Admission.AgentBinding.LineupRevision, ward.CodexAuthSubscription, true, at)
				if !errors.As(err, &marked) || marked.Mark != mark {
					t.Fatalf("marked preflight resolution = %v", err)
				}
				broken := tree
				broken.Lineup = slices.Clone(tree.Lineup)
				for i := range broken.Lineup {
					if broken.Lineup[i].Key == string(role) {
						broken.Lineup[i].Selection.PromptDigest = domain.Digest("sha256:" + strings.Repeat("7", 64))
					}
				}
				if _, err := ResolveReviewSelection(ctx, st, broken, flagEraReviewConfig(), at); err == nil ||
					errors.Is(err, domain.ErrGenerationIntegrityMarked) || !strings.Contains(err.Error(), "this daemon reviews with") {
					t.Fatalf("mark plus prompt mismatch = %v", err)
				}
				reenrollStartupRole(t, st, agent.Enrollment)
				if err := admit(ctx, req); err != nil {
					t.Fatalf("same review source after re-enrollment: %v", err)
				}
				admission, err := selection.AdmitReview(ctx, st, spec, run.ID, "review-reenrolled", at)
				if err != nil || admission.AgentBinding.EnrollmentGeneration != mark.Ordinal+1 {
					t.Fatalf("review admission after re-enrollment = %+v, %v", admission, err)
				}
			})
		})
	}
}

// TestRetiredIdentityWorkHoldsAdmissionUntilItIsStopped covers retirement and
// cancellation. An open task admitted under an identity that is then disabled
// with no enrollment holds the gate and raises one item naming the identity
// and the task, and the daemon stops nothing: that state is also an identity
// nobody adopted yet. The retirement act records one Stop, a rerun records
// nothing more, and the gate opens and the item resolves once the
// cancellation is confirmed.
func TestRetiredIdentityWorkHoldsAdmissionUntilItIsStopped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, taskID := legacyAdmittedTask(t, "flag-era-auth")
	now := time.Now().UTC()
	gate := AgentSelectionGate(st, nil)

	// An enabled identity is not retired, whatever it owns.
	if err := gate(ctx); err != nil {
		t.Fatalf("gate before retirement: %v", err)
	}
	if err := ActivateAgentSelection(ctx, st, nil, now); err != nil {
		t.Fatal(err)
	}
	if len(agentSelectionItems(t, st)) != 0 {
		t.Fatal("activation raised an item for an enabled identity's task")
	}

	setIdentityEnabled(t, st, "flag-era-auth", false)
	if err := gate(ctx); !errors.Is(err, ErrRetiredIdentityOwnsWork) {
		t.Fatalf("gate with a retired identity owning open work = %v", err)
	}
	if err := ActivateAgentSelection(ctx, st, nil, now); err != nil {
		t.Fatal(err)
	}
	if readTask(t, st, taskID).Cancellation != nil {
		t.Fatal("the daemon stopped a task on its own")
	}
	items := agentSelectionItems(t, st)
	if len(items) != 1 || !strings.Contains(items[0].Reason, "flag-era-auth") ||
		!strings.Contains(items[0].Reason, string(taskID)) ||
		items[0].HealthDiagnostic == nil || items[0].HealthDiagnostic.Code != "agent_selection_inactive" {
		t.Fatalf("items after retirement = %+v", items)
	}

	// The operator retires the identity: one Stop for the task it owns.
	stopped, err := stopRetiredTasks(ctx, st, "flag-era-auth", now)
	if err != nil || len(stopped) != 1 || stopped[0] != taskID {
		t.Fatalf("stopRetiredTasks = %v, %v", stopped, err)
	}
	cancellation := readTask(t, st, taskID).Cancellation
	if cancellation == nil || cancellation.State != domain.TaskCancellationRequested {
		t.Fatalf("cancellation after retirement = %+v", cancellation)
	}
	// Requested is not confirmed: the gate holds until teardown is proved.
	if err := gate(ctx); !errors.Is(err, ErrRetiredIdentityOwnsWork) {
		t.Fatalf("gate with an unconfirmed cancellation = %v", err)
	}

	// A rerun and a restart find the Stop recorded and the item open.
	if stopped, err := stopRetiredTasks(ctx, st, "flag-era-auth", now.Add(time.Minute)); err != nil || len(stopped) != 1 {
		t.Fatalf("second stopRetiredTasks = %v, %v", stopped, err)
	}
	if again := readTask(t, st, taskID).Cancellation; again.RequestID != cancellation.RequestID {
		t.Fatalf("a rerun recorded another stop: %+v", again)
	}
	if err := ActivateAgentSelection(ctx, st, nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := agentSelectionItems(t, st); len(got) != 1 || got[0].ID != items[0].ID {
		t.Fatalf("items after a restart = %+v", got)
	}

	// The cancellation loop confirms teardown.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.AcknowledgeTaskCancellation(ctx, domain.TaskCancellationAcknowledgement{
			ID: "ack-retired", RequestID: cancellation.RequestID, TargetDigest: cancellation.TargetDigest,
			State:          domain.TaskCancellationConfirmed,
			EvidenceDigest: domain.Digest("sha256:" + strings.Repeat("9", 64)),
			RecordedAt:     now.Add(2 * time.Minute),
		})
		return err
	}); err != nil {
		t.Fatalf("confirm the cancellation: %v", err)
	}
	if err := gate(ctx); err != nil {
		t.Fatalf("gate after the cancellation was confirmed: %v", err)
	}
	if err := ActivateAgentSelection(ctx, st, nil, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := agentSelectionItems(t, st); len(got) != 0 {
		t.Fatalf("items after confirmation = %+v", got)
	}
}

func agentSelectionItems(t *testing.T, st *store.Store) []domain.AttentionItem {
	t.Helper()
	var items []domain.AttentionItem
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		open, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
		for _, item := range open {
			if strings.HasPrefix(string(item.ID), AgentSelectionItemPrefix) {
				items = append(items, item)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return items
}

// removedReviewFlags are the four flags whose values the review agents now
// name. A command line that still passes one is refused, not silently
// ignored: an operator who kept the flag would otherwise believe it chose
// the model the review ran on.
var removedReviewFlags = []string{
	"-review-model", "-review-reasoning-effort", "-shadow-review-model", "-shadow-review-reasoning-effort",
}

// The daemon's review flags bind as before and name no model and no effort.
func TestReviewFlags(t *testing.T) {
	t.Parallel()
	args := []string{
		"-review-image", "image", "-review-input-root", "/root", "-review-auth-mode", "subscription",
		"-review-auth-snapshot", "auth.json", "-review-instructions", "AGENTS.md",
		"-review-workspace-size-mb", "1024",
		"-shadow-review-image", "shadow-image", "-shadow-review-auth-snapshot", "token",
		"-shadow-review-workspace-size-mb", "2048", "-shadow-review-rate", "0.5",
	}
	parse := func(args []string) (reviewFlagValues, error) {
		var parsed reviewFlagValues
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		reviewFlags(flags, &parsed)
		return parsed, flags.Parse(args)
	}
	parsed, err := parse(args)
	want := reviewFlagValues{
		Image: "image", InputRoot: "/root", AuthMode: "subscription", AuthSnapshot: "auth.json",
		Instructions: "AGENTS.md", WorkspaceSizeMB: 1024,
		ShadowImage: "shadow-image", ShadowAuthSnapshot: "token", ShadowWorkspaceSizeMB: 2048, ShadowRate: 0.5,
	}
	if err != nil || parsed != want {
		t.Fatalf("review flags = %+v, %v; want %+v", parsed, err, want)
	}
	if defaults, err := parse(nil); err != nil || defaults != (reviewFlagValues{
		WorkspaceSizeMB: 8192, ShadowWorkspaceSizeMB: 8192, ShadowRate: 0.2,
	}) {
		t.Fatalf("review flag defaults = %+v, %v", defaults, err)
	}
	for _, removed := range removedReviewFlags {
		if _, err := parse(append(slices.Clone(args), removed, "value")); err == nil ||
			!strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("removed flag %s: err = %v, want it unknown", removed, err)
		}
	}
}

// Preflight parses its own registration of the review flags, so it is held
// to the same four removals: the fixture's command line parses, and adding
// any one of them does not.
func TestPreflightRefusesTheRemovedReviewFlags(t *testing.T) {
	t.Parallel()
	args, _ := preflightFixture(t)
	if _, err := parsePreflightConfig(args, &bytes.Buffer{}); err != nil {
		t.Fatalf("fixture command line: %v", err)
	}
	for _, removed := range removedReviewFlags {
		var stderr bytes.Buffer
		if _, err := parsePreflightConfig(append(slices.Clone(args), removed, "value"), &stderr); err == nil ||
			!strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("removed flag %s: err = %v, want it unknown", removed, err)
		}
	}
}

// admittedReviewRequest is a review request for the run as the engine would
// send it: it carries the admission snapshot of the agent the role's line
// resolves, which is the agent a source composed from the same tree runs.
func admittedReviewRequest(
	t *testing.T, st *store.Store, tree agenttree.Tree, role domain.RoleName, runID domain.RunID,
) exec.ReviewRequest {
	t.Helper()
	agent, err := ReviewRoleAgent(context.Background(), st, tree, role, time.Now().UTC())
	if err != nil {
		t.Fatalf("resolve the %s line: %v", role, err)
	}
	admission := fake.ReviewAdmission(role, runID, "review-fixture")
	admission.AuthIdentityID = agent.Identity.ID
	admission.AgentBinding.RouteModelID = agent.Model
	admission.AgentBinding.NativeEffort = agent.NativeEffort
	return exec.ReviewRequest{RunID: runID, Admission: admission}
}

// testReviewRole is the review spec the daemon composes for a role whose line
// runs the fixture harness: Codex for the reviewer, Claude for the shadow.
func testReviewRole(role domain.RoleName, egress []string) engine.ReviewRole {
	provider := ShadowReviewProvider()
	if role == domain.RoleReviewer {
		provider, _ = ward.LookupRoutedReviewProvider(domain.HarnessClientCodexCLI)
	}
	return ReviewRole(role, provider, egress)
}

// testReviewRoles composes testReviewRole for each role in egress.
func testReviewRoles(egress map[domain.RoleName][]string) map[domain.RoleName]engine.ReviewRole {
	roles := make(map[domain.RoleName]engine.ReviewRole, len(egress))
	for role, endpoints := range egress {
		roles[role] = testReviewRole(role, endpoints)
	}
	return roles
}

// TestReviewAdmissionRefusesALineOnAnotherHarness covers the per-launch
// recheck across a harness switch (#397): a review source composed for one
// harness refuses its next review once the reviewer line names an agent on
// the other, and a source whose vendor is not the agent adapter's refuses
// even on the right harness. The restart that recomposes the source for the
// new line admits it.
func TestReviewAdmissionRefusesALineOnAnotherHarness(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, codexPatch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("adopt the Codex reviewer: %v", err)
	}
	f.recordClaudeReviewIdentity(t)
	f.enrollClaudeReviewIdentity(t)
	_, claudePatch, err := f.run(t, f.claudeReviewArgs())
	if err != nil {
		t.Fatalf("adopt the Claude reviewer: %v", err)
	}
	codexTree, claudeTree := loadAdoptedPatch(t, codexPatch), loadAdoptedPatch(t, claudePatch)
	codexSpec := testReviewRole(domain.RoleReviewer, nil)
	claudeSpec := ReviewRole(domain.RoleReviewer, mustRoutedReviewProvider(domain.HarnessClientClaudeCode), nil)
	wrongVendor := claudeSpec
	wrongVendor.Vendor = domain.AgentVendorCodex
	f.withStore(t, func(st *store.Store) {
		run := seedReviewedRun(t, st, "run-switched").ID
		codexReq := admittedReviewRequest(t, st, codexTree, domain.RoleReviewer, run)
		claudeReq := admittedReviewRequest(t, st, claudeTree, domain.RoleReviewer, run)
		for name, tc := range map[string]struct {
			tree    agenttree.Tree
			spec    engine.ReviewRole
			req     exec.ReviewRequest
			refusal string
		}{
			"Codex source, Claude line":    {claudeTree, codexSpec, claudeReq, "harness"},
			"Claude source, Codex line":    {codexTree, claudeSpec, codexReq, "harness"},
			"Claude harness, Codex vendor": {claudeTree, wrongVendor, claudeReq, "adapter"},
			"recomposed Claude source":     {claudeTree, claudeSpec, claudeReq, ""},
		} {
			err := ReviewAdmission(st, tc.tree, tc.spec, time.Now)(ctx, tc.req)
			if tc.refusal == "" {
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				continue
			}
			var failure *RoleAdmissionError
			if !errors.As(err, &failure) || failure.Role != domain.RoleReviewer ||
				!errors.Is(err, engine.ErrAgentNotAdmissible) || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("%s = %v, want a refusal naming the %s", name, err, tc.refusal)
			}
		}
	})
}

// lineupSelection is the selection the tree's lineup line for role names.
func lineupSelection(t *testing.T, tree agenttree.Tree, role domain.RoleName) domain.TaskLineSelection {
	t.Helper()
	for _, line := range tree.Lineup {
		if domain.RoleName(line.Key) == role {
			return line.Selection.TaskLineSelection()
		}
	}
	t.Fatalf("the tree has no %s line", role)
	return domain.TaskLineSelection{}
}

// TestReviewCredentialReadsTheAdmittedGeneration: the routed Claude source
// reads another identity's token from the enrollment generation the request
// was admitted under, refuses a read the store refuses, and never prints the
// token. A Codex source reads no other identity's credential.
func TestReviewCredentialReadsTheAdmittedGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	admission := fake.ReviewAdmission(domain.RoleReviewer, "run-1", "review-1")
	admission.AgentBinding.EnrollmentID, admission.AgentBinding.EnrollmentGeneration = "claude-second/claude_code", 3
	token := judgmentTestToken(t, judgmentSecondToken)
	var reads []string
	read := func(_ context.Context, id domain.ClientEnrollmentID, ordinal int) (ward.SetupToken, error) {
		reads = append(reads, fmt.Sprintf("%s@%d", id, ordinal))
		return token, nil
	}
	claude := mustRoutedReviewProvider(domain.HarnessClientClaudeCode)

	credential, err := reviewCredential(claude, read)(ctx, admission)
	if err != nil {
		t.Fatalf("read the admitted generation: %v", err)
	}
	if !slices.Equal(reads, []string{"claude-second/claude_code@3"}) {
		t.Fatalf("reads = %v, want the admitted generation", reads)
	}
	if printed := fmt.Sprintf("%v %+v %#v %s", credential, credential, credential, credential); strings.Contains(printed, judgmentSecondToken) {
		t.Fatalf("a formatted credential prints the token: %s", printed)
	}

	// A store that is not the admitted one refuses the review; a store that
	// is changing, or a read that did not finish, is retried.
	for cause, want := range map[error]domain.ReviewFailureClass{
		wardstore.ErrCredentialReadDigestMismatch:    domain.ReviewFailureConfiguration,
		wardstore.ErrCredentialReadGenerationChanged: domain.ReviewFailureConfiguration,
		wardstore.ErrCredentialReadUnsupported:       domain.ReviewFailureConfiguration,
		wardstore.ErrCredentialReadMutationLeaseLive: domain.ReviewFailureTransient,
		wardstore.ErrCredentialReadHoldEnded:         domain.ReviewFailureTransient,
		errors.New("exporter container exited"):      domain.ReviewFailureTransient,
	} {
		refusing := func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
			return ward.SetupToken{}, cause
		}
		_, err := reviewCredential(claude, refusing)(ctx, admission)
		if !errors.Is(err, cause) {
			t.Fatalf("a failed read = %v, want it to wrap %v", err, cause)
		}
		var failure *exec.ReviewSourceFailure
		switch want {
		case domain.ReviewFailureConfiguration:
			if !errors.Is(err, engine.ErrAgentNotAdmissible) || errors.As(err, &failure) {
				t.Errorf("read failing with %v = %v, want a refusal", cause, err)
			}
		default:
			if exec.ClassifyReviewSourceFailure(err) != want || errors.Is(err, engine.ErrAgentNotAdmissible) {
				t.Errorf("read failing with %v = %v, want a %s failure", cause, err, want)
			}
		}
	}

	reads = nil
	codex := mustRoutedReviewProvider(domain.HarnessClientCodexCLI)
	if _, err := reviewCredential(codex, read)(ctx, admission); !errors.Is(err, engine.ErrAgentNotAdmissible) || reads != nil {
		t.Fatalf("a Codex source = %v after reads %v, want a refusal that reads nothing", err, reads)
	}
}
