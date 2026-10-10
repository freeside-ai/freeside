package main

import (
	"context"
	"errors"
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
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// flagEraReviewConfig is a daemon configuration as the removed flags set it
// for the adoption fixture's two identities, with the shadow arm on.
func flagEraReviewConfig() claudeDriverConfig {
	return claudeDriverConfig{
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
// configuration digests across the cutover: the identity and cost owner the
// reviewer and shadow reviewer lines resolve produce the digests the removed
// flags produced, so no operator approval moves.
func TestLineupResolvedReviewDigestsEqualTheFlagEraDigests(t *testing.T) {
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args("-shadow-review-cost-owner", "operator"))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := loadAdoptedPatch(t, patch)
	flags := flagEraReviewConfig()
	wantReview, err := claudeReviewConfigurationDigest(flags)
	if err != nil {
		t.Fatal(err)
	}
	wantShadow, err := shadowReviewConfigurationDigests(flags)
	if err != nil {
		t.Fatal(err)
	}

	cfg := flags
	cfg.ReviewAuthIdentityID, cfg.ReviewCostOwner = "", ""
	cfg.ShadowReviewAuthIdentityID, cfg.ShadowReviewCostOwner = "", ""
	f.withStore(t, func(st *store.Store) {
		cfg, err = resolveReviewSelection(ctx, st, tree, cfg, time.Now().UTC())
	})
	if err != nil {
		t.Fatalf("resolve review selection: %v", err)
	}
	if !reflect.DeepEqual(cfg, flags) {
		t.Fatalf("lineup-resolved configuration = %+v, want the flag-era %+v", cfg, flags)
	}
	gotReview, err := claudeReviewConfigurationDigest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gotShadow, err := shadowReviewConfigurationDigests(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if gotReview != wantReview || gotShadow != wantShadow {
		t.Fatalf("digests moved: review %s want %s; shadow %+v want %+v", gotReview, wantReview, gotShadow, wantShadow)
	}
	golden.Assert(t, "cutover-review-configuration-digests",
		[]byte(string(gotReview)+"\n"+string(gotShadow.runtime)+"\n"+string(gotShadow.approval)+"\n"))
}

// TestPreflightResolvesTheAdoptedLineup covers what preflight probes: the
// writer roles' one credential volume and the review identities, read from
// the lineup the daemon will start with.
func TestPreflightResolvesTheAdoptedLineup(t *testing.T) {
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
		got, err := resolvePreflightAgents(ctx, st, tree, revision, false, now)
		want := preflightAgentSelection{
			LineupRevision: revision, AuthIdentityID: "claude-main", AuthVolume: adoptClaudeVolume,
			ReviewAuthIdentityID: "codex-review", ReviewCostOwner: "review-operator",
		}
		if err != nil || got != want {
			t.Fatalf("preflight agents = %+v, %v; want %+v", got, err, want)
		}
		// The shadow arm is on but adoption emitted no shadow reviewer line.
		_, err = resolvePreflightAgents(ctx, st, tree, revision, true, now)
		var failure *roleAdmissionError
		if !errors.As(err, &failure) || failure.Role != domain.RoleShadowReviewer {
			t.Fatalf("preflight agents with the shadow arm on = %v", err)
		}
	})
}

// TestReviewSelectionFailsClosed covers the review roles' startup and
// per-review checks: a lineup with no reviewer line refuses and names the
// role, an attended daemon resolves no review role, an offer past its
// not_after and an agent with no attended mark for the review launch refuse,
// and a source composed under an identity refuses its next review once the
// identity is disabled.
func TestReviewSelectionFailsClosed(t *testing.T) {
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
			_, err := resolveReviewSelection(ctx, st, tree, cfg, now)
			var failure *roleAdmissionError
			if !errors.As(err, &failure) || failure.Role != domain.RoleReviewer ||
				!errors.Is(err, engine.ErrAgentNotAdmissible) {
				t.Fatalf("resolve without a reviewer line = %v", err)
			}
			cfg.OperatingMode = domain.ModeAttendedDev
			if got, err := resolveReviewSelection(ctx, st, tree, cfg, now); err != nil || !reflect.DeepEqual(got, cfg) {
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
			_, err := resolveReviewSelection(ctx, st, tree, flagEraReviewConfig(), now)
			var failure *roleAdmissionError
			if !errors.As(err, &failure) || failure.Role != domain.RoleShadowReviewer {
				t.Fatalf("resolve without a shadow reviewer line = %v", err)
			}
		})
	})

	t.Run("offer past its not_after", func(t *testing.T) {
		f := newAuthAdoptFixture(t)
		_, patch, err := f.run(t, f.args("-shadow-review-cost-owner", "operator"))
		if err != nil {
			t.Fatalf("auth adopt: %v", err)
		}
		tree := loadAdoptedPatch(t, patch)
		for i := range tree.Offers {
			if tree.Offers[i].Fragment.NotAfter.Before(now) {
				t.Fatalf("adopted offer %s is already past its not_after", tree.Offers[i].Name)
			}
		}
		f.withStore(t, func(st *store.Store) {
			for _, role := range []domain.RoleName{domain.RoleReviewer, domain.RoleShadowReviewer} {
				agent, err := reviewRoleAgent(ctx, st, tree, role, now)
				if err != nil {
					t.Fatalf("%s before the offer's not_after: %v", role, err)
				}
				expired := agent.Resolved.Offer.NotAfter.Add(time.Second)
				_, err = reviewRoleAgent(ctx, st, tree, role, expired)
				var failure *roleAdmissionError
				if !errors.As(err, &failure) || failure.Role != role ||
					!errors.Is(err, engine.ErrAgentNotAdmissible) || !strings.Contains(err.Error(), "not_after") {
					t.Fatalf("%s after the offer's not_after = %v", role, err)
				}
				admit := reviewAdmission(st, tree, role, agent.Identity.ID, func() time.Time { return expired })
				if err := admit(ctx, exec.ReviewRequest{}); !errors.Is(err, engine.ErrAgentNotAdmissible) {
					t.Fatalf("%s review admission after the offer's not_after = %v", role, err)
				}
			}
		})
	})

	t.Run("no attended mark", func(t *testing.T) {
		f := newAuthAdoptFixture(t)
		_, patch, err := f.run(t, f.args("-shadow-review-cost-owner", "operator"))
		if err != nil {
			t.Fatalf("auth adopt: %v", err)
		}
		tree := loadAdoptedPatch(t, patch)
		review, err := agentbaseline.RoleLaunch(domain.RoleReviewer)
		if err != nil {
			t.Fatal(err)
		}
		f.withStore(t, func(st *store.Store) {
			for _, role := range []domain.RoleName{domain.RoleReviewer, domain.RoleShadowReviewer} {
				agent, err := reviewRoleAgent(ctx, st, tree, role, now)
				if err != nil {
					t.Fatalf("%s with its attended mark: %v", role, err)
				}
				// Drop only this agent's mark for the review launch: a mark for
				// another launch of the same agent does not cover a review.
				unmarked := tree
				unmarked.Marks = slices.DeleteFunc(slices.Clone(tree.Marks), func(mark agenttree.Mark) bool {
					return mark.Agent == agent.Line.AgentName && mark.LaunchDigest == review.Digest
				})
				if len(unmarked.Marks) != len(tree.Marks)-1 {
					t.Fatalf("%s: dropped %d marks, want 1", role, len(tree.Marks)-len(unmarked.Marks))
				}
				_, err = reviewRoleAgent(ctx, st, unmarked, role, now)
				var failure *roleAdmissionError
				if !errors.As(err, &failure) || failure.Role != role ||
					!errors.Is(err, engine.ErrAgentNotAdmissible) || !strings.Contains(err.Error(), "attended mark") {
					t.Fatalf("%s without its attended mark = %v", role, err)
				}
				admit := reviewAdmission(st, unmarked, role, agent.Identity.ID, time.Now)
				if err := admit(ctx, exec.ReviewRequest{}); !errors.Is(err, engine.ErrAgentNotAdmissible) {
					t.Fatalf("%s review admission without its attended mark = %v", role, err)
				}
			}
			cfg := flagEraReviewConfig()
			unmarked := tree
			unmarked.Marks = nil
			var failure *roleAdmissionError
			if _, err := resolveReviewSelection(ctx, st, unmarked, cfg, now); !errors.As(err, &failure) ||
				failure.Role != domain.RoleReviewer {
				t.Fatalf("startup selection with no marks = %v", err)
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
			req := exec.ReviewRequest{RunID: seedReviewedRun(t, st, "run-reviewed").ID}
			admit := reviewAdmission(st, tree, domain.RoleReviewer, "codex-review", time.Now)
			if err := admit(ctx, req); err != nil {
				t.Fatalf("review admission of the adopted reviewer: %v", err)
			}
			other := reviewAdmission(st, tree, domain.RoleReviewer, "codex-other", time.Now)
			if err := other(ctx, req); !errors.Is(err, engine.ErrAgentNotAdmissible) {
				t.Fatalf("review admission under another identity = %v", err)
			}
			setIdentityEnabled(t, st, "codex-review", false)
			var failure *roleAdmissionError
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
		return tx.PutTaskSubmissionRequest(ctx, domain.TaskSubmission{
			CommandID: "cmd-" + string(id), DeviceID: "device-1", ProjectID: run.ProjectID,
			SourceDigest: domain.Digest("sha256:" + strings.Repeat("4", 64)), TaskID: run.TaskID,
			SpecificationRunID: id,
			Name:               domain.DisplayName{Text: "Reviewed task", Source: domain.DisplayNameSourceOperator},
		}, domain.Digest("sha256:"+strings.Repeat("c", 64)))
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

// setReviewedRunLine records a line on the run's task, set by setBy at the
// task's creation instant plus skew.
func setReviewedRunLine(
	t *testing.T, st *store.Store, run domain.Run, role domain.RoleName, agent, setBy string, skew time.Duration,
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
			TaskID: task.ID, Role: role, Agent: agent, Source: domain.TaskLineSourceSubmitTask, SetBy: setBy,
		}, task.CreatedAt.Add(skew))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return line
}

// TestReviewAdmissionHoldsTheReviewerTaskLine: the review source runs one
// agent, fixed when the daemon composed it, so a task's reviewer line cannot
// move its review. The line is never passed over: a request for a task whose
// line names another agent is refused, and so is one whose line does not hold
// against the command that set it. The shadow reviewer takes no task line.
func TestReviewAdmissionHoldsTheReviewerTaskLine(t *testing.T) {
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args("-shadow-review-cost-owner", "operator"))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	tree := loadAdoptedPatch(t, patch)
	now := time.Now().UTC()
	f.withStore(t, func(st *store.Store) {
		reviewer, err := reviewRoleAgent(ctx, st, tree, domain.RoleReviewer, now)
		if err != nil {
			t.Fatal(err)
		}
		shadow, err := reviewRoleAgent(ctx, st, tree, domain.RoleShadowReviewer, now)
		if err != nil {
			t.Fatal(err)
		}
		admit := reviewAdmission(st, tree, domain.RoleReviewer, reviewer.Identity.ID, time.Now)
		admitShadow := reviewAdmission(st, tree, domain.RoleShadowReviewer, shadow.Identity.ID, time.Now)
		refused := func(t *testing.T, req exec.ReviewRequest, want string) {
			t.Helper()
			err := admit(ctx, req)
			var failure *roleAdmissionError
			if !errors.As(err, &failure) || failure.Role != domain.RoleReviewer ||
				!errors.Is(err, engine.ErrAgentNotAdmissible) || !strings.Contains(err.Error(), want) {
				t.Fatalf("review admission = %v, want a reviewer refusal naming %q", err, want)
			}
		}

		t.Run("no reviewer line", func(t *testing.T) {
			run := seedReviewedRun(t, st, "run-lineless")
			// A line for another role is not the reviewer's.
			setReviewedRunLine(t, st, run, domain.RoleImplementer, "some-other-agent", "cmd-run-lineless", 0)
			if err := admit(ctx, exec.ReviewRequest{RunID: run.ID}); err != nil {
				t.Fatalf("review admission with no reviewer line: %v", err)
			}
		})

		t.Run("the line names the agent the source runs", func(t *testing.T) {
			run := seedReviewedRun(t, st, "run-same-agent")
			setReviewedRunLine(t, st, run, domain.RoleReviewer, reviewer.Line.AgentName, "cmd-run-same-agent", 0)
			if err := admit(ctx, exec.ReviewRequest{RunID: run.ID}); err != nil {
				t.Fatalf("review admission with a line naming the composed agent: %v", err)
			}
		})

		t.Run("the line names another agent", func(t *testing.T) {
			run := seedReviewedRun(t, st, "run-other-agent")
			// The other agent is a real one that passes every admission step
			// for its own role: the refusal is the source's limit, not the
			// agent's.
			if shadow.Line.AgentName == reviewer.Line.AgentName {
				t.Fatal("the fixture's two review roles share an agent")
			}
			line := setReviewedRunLine(t, st, run, domain.RoleReviewer, shadow.Line.AgentName, "cmd-run-other-agent", 0)
			req := exec.ReviewRequest{RunID: run.ID}
			refused(t, req, string(line.ID))
			refused(t, req, "cannot run another agent for one task")
			// The shadow reviewer reads no line, this task's included.
			if err := admitShadow(ctx, req); err != nil {
				t.Fatalf("shadow review admission beside a refused reviewer line: %v", err)
			}
		})

		t.Run("the refusal never repeats the line's agent", func(t *testing.T) {
			run := seedReviewedRun(t, st, "run-pasted-agent")
			// A credential pasted as the agent can pass the name grammar. The
			// refusal becomes the review failure's recorded reason, so it
			// names the line and not the text.
			const pasted = "0123456789abcdef0123456789abcdef"
			line := setReviewedRunLine(t, st, run, domain.RoleReviewer, pasted, "cmd-run-pasted-agent", 0)
			req := exec.ReviewRequest{RunID: run.ID}
			refused(t, req, string(line.ID))
			if err := admit(ctx, req); strings.Contains(err.Error(), pasted) {
				t.Fatalf("review admission = %v, which repeats the line's agent", err)
			}
		})

		t.Run("the line does not hold against its command", func(t *testing.T) {
			// Each line names the composed agent, so only the anchor refuses.
			unrecorded := seedReviewedRun(t, st, "run-unrecorded")
			setReviewedRunLine(t, st, unrecorded, domain.RoleReviewer, reviewer.Line.AgentName, "cmd-never-recorded", 0)
			refused(t, exec.ReviewRequest{RunID: unrecorded.ID}, "cmd-never-recorded")

			borrowed := seedReviewedRun(t, st, "run-borrowed")
			setReviewedRunLine(t, st, borrowed, domain.RoleReviewer, reviewer.Line.AgentName, "cmd-run-same-agent", 0)
			refused(t, exec.ReviewRequest{RunID: borrowed.ID}, "that command created task")

			late := seedReviewedRun(t, st, "run-late")
			setReviewedRunLine(t, st, late, domain.RoleReviewer, reviewer.Line.AgentName, "cmd-run-late", time.Second)
			refused(t, exec.ReviewRequest{RunID: late.ID}, "its task was created at")
		})

		t.Run("the request names no stored run", func(t *testing.T) {
			// Whether the task has a line cannot be read, so the review is
			// refused instead of run on the assumption that it has none.
			refused(t, exec.ReviewRequest{RunID: "run-never-stored"}, "run-never-stored")
		})
	})
}

// TestWriterRoleFailureHoldsAdmissionAndRaisesOneItem covers the startup
// check for the writer roles: a role whose line names another prompt closes
// the gate, raises one system_health item naming the role, raises no second
// item on the next start, and the item resolves once the role admits.
func TestWriterRoleFailureHoldsAdmissionAndRaisesOneItem(t *testing.T) {
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	checkout, commit := commitAdoptedPatch(t, patch)
	selection, err := loadAgentSelection(ctx, claudeDriverConfig{
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
		if err := recordBaselineAdapterConformance(ctx, st, now); err != nil {
			t.Fatal(err)
		}
		if failure := checkWriterRoles(ctx, st, *selection, prompts, domain.ModeUnattended, now); failure != nil {
			t.Fatalf("adopted writer roles: %v", failure)
		}
		if err := agentSelectionGate(st, nil)(ctx); err != nil {
			t.Fatalf("gate with every role admitted: %v", err)
		}

		// The daemon now runs another remediation prompt than the line names.
		prompts[domain.RoleRemediator] = prompts[domain.RoleImplementer]
		failure := checkWriterRoles(ctx, st, *selection, prompts, domain.ModeUnattended, now)
		if failure == nil || failure.Role != domain.RoleRemediator || !errors.Is(failure, engine.ErrAgentNotAdmissible) {
			t.Fatalf("mismatched remediation prompt = %v", failure)
		}
		if err := agentSelectionGate(st, failure)(ctx); !errors.Is(err, engine.ErrAgentNotAdmissible) {
			t.Fatalf("gate with a failed role = %v", err)
		}
		for range 2 {
			if err := activateAgentSelection(ctx, st, failure, now); err != nil {
				t.Fatal(err)
			}
		}
		items := agentSelectionItems(t, st)
		if len(items) != 1 || !strings.Contains(items[0].Reason, "role remediator") ||
			!strings.HasPrefix(string(items[0].ID), agentSelectionItemPrefix+roleCausePrefix+"remediator-") {
			t.Fatalf("items after a role failure = %+v", items)
		}
		// A start that fails before the engine exists reports the same item.
		if err := reportRoleFailure(ctx, st, failure, now); err != nil {
			t.Fatal(err)
		}
		if got := agentSelectionItems(t, st); len(got) != 1 || got[0].ID != items[0].ID {
			t.Fatalf("items after reporting the same failure = %+v", got)
		}
		if err := activateAgentSelection(ctx, st, nil, now); err != nil {
			t.Fatal(err)
		}
		if got := agentSelectionItems(t, st); len(got) != 0 {
			t.Fatalf("items after the role admits = %+v", got)
		}
	})
}

// TestRetiredIdentityWorkHoldsAdmissionUntilItIsStopped covers retirement and
// cancellation. An open task admitted under an identity that is then disabled
// with no enrollment holds the gate and raises one item naming the identity
// and the task, and the daemon stops nothing: that state is also an identity
// nobody adopted yet. The retirement act records one Stop, a rerun records
// nothing more, and the gate opens and the item resolves once the
// cancellation is confirmed.
func TestRetiredIdentityWorkHoldsAdmissionUntilItIsStopped(t *testing.T) {
	ctx := context.Background()
	st, taskID := legacyAdmittedTask(t, "flag-era-auth")
	now := time.Now().UTC()
	gate := agentSelectionGate(st, nil)

	// An enabled identity is not retired, whatever it owns.
	if err := gate(ctx); err != nil {
		t.Fatalf("gate before retirement: %v", err)
	}
	if err := activateAgentSelection(ctx, st, nil, now); err != nil {
		t.Fatal(err)
	}
	if len(agentSelectionItems(t, st)) != 0 {
		t.Fatal("activation raised an item for an enabled identity's task")
	}

	setIdentityEnabled(t, st, "flag-era-auth", false)
	if err := gate(ctx); !errors.Is(err, errRetiredIdentityOwnsWork) {
		t.Fatalf("gate with a retired identity owning open work = %v", err)
	}
	if err := activateAgentSelection(ctx, st, nil, now); err != nil {
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
	if err := gate(ctx); !errors.Is(err, errRetiredIdentityOwnsWork) {
		t.Fatalf("gate with an unconfirmed cancellation = %v", err)
	}

	// A rerun and a restart find the Stop recorded and the item open.
	if stopped, err := stopRetiredTasks(ctx, st, "flag-era-auth", now.Add(time.Minute)); err != nil || len(stopped) != 1 {
		t.Fatalf("second stopRetiredTasks = %v, %v", stopped, err)
	}
	if again := readTask(t, st, taskID).Cancellation; again.RequestID != cancellation.RequestID {
		t.Fatalf("a rerun recorded another stop: %+v", again)
	}
	if err := activateAgentSelection(ctx, st, nil, now.Add(time.Minute)); err != nil {
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
	if err := activateAgentSelection(ctx, st, nil, now.Add(3*time.Minute)); err != nil {
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
			if strings.HasPrefix(string(item.ID), agentSelectionItemPrefix) {
				items = append(items, item)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return items
}
