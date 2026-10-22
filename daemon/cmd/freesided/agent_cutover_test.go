package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
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
)

// flagEraReviewConfig is a daemon configuration as the removed flags set it
// for the adoption fixture's two identities, with the shadow arm on, and the
// Codex provider the fixture's reviewer line selects.
func flagEraReviewConfig() claudeDriverConfig {
	return claudeDriverConfig{
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
	ctx := context.Background()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.shadowArgs())
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
	cfg.ReviewProvider = ward.RoutedReviewProvider{}
	cfg.ReviewAuthIdentityID, cfg.ReviewCostOwner = "", ""
	cfg.ReviewModel, cfg.ReviewReasoningEffort = "", ""
	cfg.ShadowReviewAuthIdentityID, cfg.ShadowReviewCostOwner = "", ""
	cfg.ShadowReviewModel, cfg.ShadowReviewReasoningEffort = "", ""
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
		var mismatch *reviewProviderMismatch
		if !errors.As(err, &mismatch) || !errors.Is(err, engine.ErrAgentNotAdmissible) {
			t.Fatalf("Codex reviewer under setup_token = %v", err)
		}
		// The shadow arm is on but adoption emitted no shadow reviewer line.
		_, err = resolvePreflightAgents(ctx, st, tree, revision, ward.CodexAuthSubscription, true, now)
		var failure *roleAdmissionError
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
		got, err := resolveReviewSelection(ctx, st, tree, cfg, now)
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

// treeAdoptedBeforeReviewAgentsNamedALaunch rewrites an adopted tree's review
// agents into the shape adoption wrote before they named a model and an
// effort: the reviewer on codex_review_v1 asking harness_default from an
// offer that allows only that, and the shadow reviewer line on the writer
// agent of that era, `claude-code-default` on the offer that names no model,
// put back here because the baseline no longer emits it. Every digest,
// lineup line, and attended mark follows, so the only thing wrong with the
// tree is what it lacks.
func treeAdoptedBeforeReviewAgentsNamedALaunch(t *testing.T, tree agenttree.Tree) agenttree.Tree {
	t.Helper()
	old := tree
	old.Agents, old.Offers = slices.Clone(tree.Agents), slices.Clone(tree.Offers)
	old.Lineup, old.Marks = slices.Clone(tree.Lineup), slices.Clone(tree.Marks)
	v1, err := agentbaseline.CodexReviewAdapterV1()
	if err != nil {
		t.Fatal(err)
	}
	old.Adapters = append(slices.Clone(tree.Adapters),
		agenttree.Adapter{Name: agentbaseline.CodexReviewAdapterV1Build, Fragment: v1})
	for i := range old.Agents {
		if old.Agents[i].Name == agentbaseline.CodexReviewAgentName {
			old.Agents[i].Adapter, old.Agents[i].Effort = agentbaseline.CodexReviewAdapterV1Build, domain.EffortHarnessDefault
		}
	}
	for i := range old.Offers {
		if old.Offers[i].Name != agentbaseline.CodexOfferName {
			continue
		}
		old.Offers[i].Fragment.AllowedEfforts = []domain.EffortLevel{domain.EffortHarnessDefault}
		if old.Offers[i].Fragment.Digest, err = old.Offers[i].Fragment.ComputeDigest(); err != nil {
			t.Fatal(err)
		}
	}
	writer := agentbaseline.WriterAgentName(domain.RoleImplementer)
	for _, offer := range tree.Offers {
		if offer.Name != writer {
			continue
		}
		native := offer
		native.Name = agentbaseline.ClaudeOfferName
		native.Fragment.RouteModelID = domain.ClaudeCodeNativeDefaultRouteModelID
		native.Fragment.IdentityStability = domain.IdentityOpaque
		native.Fragment.AllowedEfforts = []domain.EffortLevel{domain.EffortHarnessDefault}
		if native.Fragment.Digest, err = native.Fragment.ComputeDigest(); err != nil {
			t.Fatal(err)
		}
		old.Offers = append(old.Offers, native)
	}
	for _, agent := range tree.Agents {
		if agent.Name != writer {
			continue
		}
		agent.Name, agent.Offer, agent.Effort = agentbaseline.ClaudeAgentName, agentbaseline.ClaudeOfferName, domain.EffortHarnessDefault
		old.Agents = append(old.Agents, agent)
	}
	old.Sort()
	review, err := agentbaseline.RoleLaunch(domain.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	for i := range old.Lineup {
		line := &old.Lineup[i].Selection
		switch domain.RoleName(old.Lineup[i].Key) {
		case domain.RoleReviewer:
		case domain.RoleShadowReviewer:
			line.AgentName = agentbaseline.ClaudeAgentName
		default:
			continue
		}
		if line.AgentDigest, err = old.AgentDigest(line.AgentName); err != nil {
			t.Fatal(err)
		}
		old.Marks = append(old.Marks, agenttree.Mark{
			Agent: line.AgentName, AgentDigest: line.AgentDigest, LaunchDigest: review.Digest,
		})
	}
	return old
}

// TestReviewRoleNeedsAModelAndAnEffort runs this build on a tree adopted
// before the review agents named what their launch passes. Each review role
// is refused by name with what its agent lacks, at startup, in preflight,
// and before a review, instead of launching on the harness's own defaults.
func TestReviewRoleNeedsAModelAndAnEffort(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.shadowArgs())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	old := treeAdoptedBeforeReviewAgentsNamedALaunch(t, loadAdoptedPatch(t, patch))
	f.withStore(t, func(st *store.Store) {
		for role, lacks := range map[domain.RoleName]string{
			domain.RoleReviewer:       "asks effort harness_default, which sends none",
			domain.RoleShadowReviewer: "is on an offer that names no model",
		} {
			_, err := reviewRoleAgent(ctx, st, old, role, now)
			var failure *roleAdmissionError
			if !errors.As(err, &failure) || failure.Role != role || !errors.Is(err, engine.ErrAgentNotAdmissible) ||
				!strings.Contains(err.Error(), "role "+string(role)) || !strings.Contains(err.Error(), lacks) {
				t.Fatalf("%s on the earlier tree = %v, want it refused because its agent %s", role, err, lacks)
			}
			// The identity is never compared: the agent is refused first, so
			// the refusal has to name what the agent lacks.
			admit := reviewAdmission(st, old, testReviewRole(role, nil), "", time.Now)
			if err := admit(ctx, exec.ReviewRequest{}); !errors.Is(err, engine.ErrAgentNotAdmissible) ||
				!strings.Contains(err.Error(), lacks) {
				t.Fatalf("%s review admission on the earlier tree = %v, want it refused because its agent %s", role, err, lacks)
			}
		}
		var failure *roleAdmissionError
		if _, err := resolveReviewSelection(ctx, st, old, flagEraReviewConfig(), now); !errors.As(err, &failure) ||
			failure.Role != domain.RoleReviewer {
			t.Fatalf("startup selection on the earlier tree = %v", err)
		}
		_, err := resolvePreflightAgents(ctx, st, old, domain.Digest("sha256:"+strings.Repeat("4", 64)), ward.CodexAuthSubscription, true, now)
		if !errors.As(err, &failure) || failure.Role != domain.RoleReviewer {
			t.Fatalf("preflight agents on the earlier tree = %v", err)
		}
	})
}

// TestReviewRoleRefusesAModelTheLaunchCannotPass hand-authors each review
// agent's offer with a model that is no single argument: a comma, which the
// review launch's own rule refuses, and a leading dash, which a CLI would
// read as another option. Adoption refuses both as flags; a committed offer
// is held to the same rule.
func TestReviewRoleRefusesAModelTheLaunchCannotPass(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	f := newAuthAdoptFixture(t)
	_, patch, err := f.run(t, f.shadowArgs())
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	adopted := loadAdoptedPatch(t, patch)
	review, err := agentbaseline.RoleLaunch(domain.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		role         domain.RoleName
		agent, offer string
		model        string
	}{
		{domain.RoleReviewer, agentbaseline.CodexReviewAgentName, agentbaseline.CodexOfferName, "gpt,fixture"},
		{
			domain.RoleShadowReviewer, agentbaseline.ClaudeShadowReviewAgentName,
			agentbaseline.ClaudeShadowReviewOfferName, "--dangerously-skip-permissions",
		},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			tree := adopted
			tree.Offers, tree.Lineup = slices.Clone(adopted.Offers), slices.Clone(adopted.Lineup)
			tree.Marks = slices.Clone(adopted.Marks)
			offer := slices.IndexFunc(tree.Offers, func(o agenttree.Offer) bool { return o.Name == tc.offer })
			if offer < 0 {
				t.Fatalf("adopted tree has no offer %s", tc.offer)
			}
			tree.Offers[offer].Fragment.RouteModelID = tc.model
			if tree.Offers[offer].Fragment.Digest, err = tree.Offers[offer].Fragment.ComputeDigest(); err != nil {
				t.Fatal(err)
			}
			digest, err := tree.AgentDigest(tc.agent)
			if err != nil {
				t.Fatal(err)
			}
			for i := range tree.Lineup {
				if domain.RoleName(tree.Lineup[i].Key) == tc.role {
					tree.Lineup[i].Selection.AgentDigest = digest
				}
			}
			tree.Marks = append(tree.Marks, agenttree.Mark{Agent: tc.agent, AgentDigest: digest, LaunchDigest: review.Digest})
			f.withStore(t, func(st *store.Store) {
				_, err := reviewRoleAgent(ctx, st, tree, tc.role, now)
				var failure *roleAdmissionError
				if !errors.As(err, &failure) || failure.Role != tc.role || !errors.Is(err, engine.ErrAgentNotAdmissible) ||
					!strings.Contains(err.Error(), "cannot pass as one argument") {
					t.Fatalf("%s on an offer naming %q = %v, want it refused for the model's shape", tc.role, tc.model, err)
				}
			})
		})
	}
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
		_, patch, err := f.run(t, f.shadowArgs())
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
				admit := reviewAdmission(st, tree, testReviewRole(role, nil), agent.Identity.ID, func() time.Time { return expired })
				if err := admit(ctx, exec.ReviewRequest{}); !errors.Is(err, engine.ErrAgentNotAdmissible) {
					t.Fatalf("%s review admission after the offer's not_after = %v", role, err)
				}
			}
		})
	})

	t.Run("no attended mark", func(t *testing.T) {
		f := newAuthAdoptFixture(t)
		_, patch, err := f.run(t, f.shadowArgs())
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
				admit := reviewAdmission(st, unmarked, testReviewRole(role, nil), agent.Identity.ID, time.Now)
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
			req := admittedReviewRequest(t, st, tree, domain.RoleReviewer, seedReviewedRun(t, st, "run-reviewed").ID)
			admit := reviewAdmission(st, tree, testReviewRole(domain.RoleReviewer, nil), "codex-review", time.Now)
			if err := admit(ctx, req); err != nil {
				t.Fatalf("review admission of the adopted reviewer: %v", err)
			}
			other := reviewAdmission(st, tree, testReviewRole(domain.RoleReviewer, nil), "codex-other", time.Now)
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
	_, patch, err := f.run(t, f.shadowArgs())
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
		admit := reviewAdmission(st, tree, testReviewRole(domain.RoleReviewer, nil), reviewer.Identity.ID, time.Now)
		admitShadow := reviewAdmission(st, tree, testReviewRole(domain.RoleShadowReviewer, nil), shadow.Identity.ID, time.Now)
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
			req := admittedReviewRequest(t, st, tree, domain.RoleReviewer, run.ID)
			if err := admit(ctx, req); err != nil {
				t.Fatalf("review admission with no reviewer line: %v", err)
			}
			// The source launches under the identity, model, and effort it was
			// composed with, so a snapshot naming another of them, or the
			// other review role, is not this source's to run.
			for name, change := range map[string]func(*domain.ReviewAdmission){
				"another role":     func(a *domain.ReviewAdmission) { a.Role = domain.RoleShadowReviewer },
				"another identity": func(a *domain.ReviewAdmission) { a.AuthIdentityID = shadow.Identity.ID },
				"another model":    func(a *domain.ReviewAdmission) { a.AgentBinding.RouteModelID += "-next" },
				"another effort":   func(a *domain.ReviewAdmission) { a.AgentBinding.NativeEffort += "er" },
			} {
				changed := req
				change(&changed.Admission)
				refused(t, changed, "the invocation was admitted as")
				if err := admit(ctx, changed); err == nil || !strings.Contains(err.Error(), "the review source runs") {
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
			admitter := reviewAdmitter(st, engine.AgentSelection{
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
				definition, offer := arm.agent.Resolved.Definition, arm.agent.Resolved.Offer
				if admitted.InvocationID != id || admitted.RunID != run.ID || admitted.Role != role ||
					admitted.AuthIdentityID != arm.agent.Identity.ID || admitted.LineageGroup != offer.LineageGroup ||
					admitted.AgentBinding.AgentDigest != definition.Digest ||
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
			// The reviewer line of this task names another agent: the engine
			// refuses the invocation before any snapshot exists. The shadow
			// reviewer reads no line.
			setReviewedRunLine(t, st, run, domain.RoleReviewer, shadow.Line.AgentName, "cmd-run-admitted", 0)
			var failure *roleAdmissionError
			if _, err := admitter(ctx, domain.RoleReviewer, run.ID, "review-admitted-refused", now); !errors.As(err, &failure) ||
				failure.Role != domain.RoleReviewer || !errors.Is(err, engine.ErrAgentNotAdmissible) {
				t.Fatalf("admit a reviewer invocation whose task line names another agent = %v", err)
			}
			if _, err := admitter(ctx, domain.RoleShadowReviewer, run.ID, "review-admitted-shadow-2", now); err != nil {
				t.Fatalf("admit a shadow invocation beside a refused reviewer line: %v", err)
			}
		})

		t.Run("the line names the agent the source runs", func(t *testing.T) {
			run := seedReviewedRun(t, st, "run-same-agent")
			setReviewedRunLine(t, st, run, domain.RoleReviewer, reviewer.Line.AgentName, "cmd-run-same-agent", 0)
			if err := admit(ctx, admittedReviewRequest(t, st, tree, domain.RoleReviewer, run.ID)); err != nil {
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
			if err := admitShadow(ctx, admittedReviewRequest(t, st, tree, domain.RoleShadowReviewer, run.ID)); err != nil {
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

// TestReviewInvocationIsRefusedBeforeItIsAdmitted admits a review invocation
// for each review role and then breaks, one at a time, what review admission
// rests on. Each refusal names the role, and none returns a snapshot the
// engine could store: a refused invocation is never requested.
func TestReviewInvocationIsRefusedBeforeItIsAdmitted(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	review, err := agentbaseline.RoleLaunch(domain.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
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
		"the line selects an agent on the other harness": {
			breakIt: func(_ *testing.T, f *fixture, role domain.RoleName, _ engine.ReviewRoleAgent) {
				theirs := lineOf(f.tree, other[role])
				mine := lineOf(f.tree, role)
				mine.AgentName, mine.AgentDigest = theirs.AgentName, theirs.AgentDigest
			},
			want: "runs harness client",
		},
		"the offer is past its not_after": {
			breakIt: func(_ *testing.T, f *fixture, _ domain.RoleName, agent engine.ReviewRoleAgent) {
				f.at = agent.Resolved.Offer.NotAfter.Add(time.Second)
			},
			want: "not_after",
		},
		"the identity holds no current credential": {
			breakIt: func(t *testing.T, f *fixture, _ domain.RoleName, agent engine.ReviewRoleAgent) {
				setIdentityEnabled(t, f.st, agent.Identity.ID, false)
			},
			want: "role ",
		},
		"the agent has no attended mark for the review launch": {
			breakIt: func(_ *testing.T, f *fixture, _ domain.RoleName, agent engine.ReviewRoleAgent) {
				f.tree.Marks = slices.DeleteFunc(slices.Clone(f.tree.Marks), func(mark agenttree.Mark) bool {
					return mark.Agent == agent.Line.AgentName && mark.LaunchDigest == review.Digest
				})
			},
			want: "attended mark",
		},
		// Only the routed reviewer reads a task line.
		"the task's line names another agent": {
			roles: []domain.RoleName{domain.RoleReviewer},
			breakIt: func(t *testing.T, f *fixture, role domain.RoleName, _ engine.ReviewRoleAgent) {
				setReviewedRunLine(t, f.st, f.run, role, lineOf(f.tree, other[role]).AgentName, "cmd-run-refused", 0)
			},
			want: "cannot run another agent for one task",
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
						return reviewAdmitter(st, engine.AgentSelection{
							Tree: f.tree, LineupRevision: revision, Launch: agentbaseline.RoleLaunch,
						}, testReviewRoles(map[domain.RoleName][]string{role: {"review.example:443"}}))(ctx, role, f.run.ID, id, f.at)
					}
					if _, err := admit("review-before"); err != nil {
						t.Fatalf("admit the %s invocation on the adopted tree: %v", role, err)
					}
					agent, err := reviewRoleAgent(ctx, st, f.tree, role, now)
					if err != nil {
						t.Fatal(err)
					}
					tc.breakIt(t, f, role, agent)
					admitted, err := admit("review-after")
					var failure *roleAdmissionError
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

// removedReviewFlags are the four flags whose values the review agents now
// name. A command line that still passes one is refused, not silently
// ignored: an operator who kept the flag would otherwise believe it chose
// the model the review ran on.
var removedReviewFlags = []string{
	"-review-model", "-review-reasoning-effort", "-shadow-review-model", "-shadow-review-reasoning-effort",
}

// The daemon's review flags bind as before and name no model and no effort.
func TestReviewFlags(t *testing.T) {
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
	agent, err := reviewRoleAgent(context.Background(), st, tree, role, time.Now().UTC())
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
	provider := shadowReviewProvider()
	if role == domain.RoleReviewer {
		provider, _ = ward.LookupRoutedReviewProvider(domain.HarnessClientCodexCLI)
	}
	return reviewRole(role, provider, egress)
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
	claudeSpec := reviewRole(domain.RoleReviewer, mustRoutedReviewProvider(domain.HarnessClientClaudeCode), nil)
	wrongVendor := claudeSpec
	wrongVendor.Vendor = domain.AgentVendorCodex
	f.withStore(t, func(st *store.Store) {
		run := seedReviewedRun(t, st, "run-switched").ID
		codexReq := admittedReviewRequest(t, st, codexTree, domain.RoleReviewer, run)
		claudeReq := admittedReviewRequest(t, st, claudeTree, domain.RoleReviewer, run)
		for name, tc := range map[string]struct {
			tree     agenttree.Tree
			spec     engine.ReviewRole
			identity domain.AuthIdentityID
			req      exec.ReviewRequest
			refusal  string
		}{
			"Codex source, Claude line":    {claudeTree, codexSpec, "claude-review", claudeReq, "harness"},
			"Claude source, Codex line":    {codexTree, claudeSpec, "codex-review", codexReq, "harness"},
			"Claude harness, Codex vendor": {claudeTree, wrongVendor, "claude-review", claudeReq, "adapter"},
			"recomposed Claude source":     {claudeTree, claudeSpec, "claude-review", claudeReq, ""},
		} {
			err := reviewAdmission(st, tc.tree, tc.spec, tc.identity, time.Now)(ctx, tc.req)
			if tc.refusal == "" {
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				continue
			}
			var failure *roleAdmissionError
			if !errors.As(err, &failure) || failure.Role != domain.RoleReviewer ||
				!errors.Is(err, engine.ErrAgentNotAdmissible) || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("%s = %v, want a refusal naming the %s", name, err, tc.refusal)
			}
		}
	})
}
