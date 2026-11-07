package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The selection leg of admission (#2596): a ward role's agent must stand for
// the selection its enrollment's catalog and the launch shape resolve, and
// the binding records that selection.

// recordCatalog stores a discovered catalog for the enrollment listing one
// model with the given effort levels.
func (f *agentAdmissionFixture) recordCatalog(
	t *testing.T, enrollment domain.ClientEnrollmentID, model string, efforts ...string,
) {
	t.Helper()
	if efforts == nil {
		efforts = []string{}
	}
	catalog := domain.DiscoveredModelCatalog{
		EnrollmentID: enrollment, HarnessBuild: "2.1.0 (Claude Code)", ObservedAt: agentTestAt.Add(-time.Minute),
		Models: []domain.DiscoveredModel{{ID: model, DisplayName: "Test model", EffortLevels: efforts}},
	}
	if err := f.store.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.RecordDiscoveredModelCatalog(context.Background(), catalog)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStageAdmissionRecordsTheSelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	prompt := agentTestPrompts[domain.RoleImplementer].Digest
	admit := func(t *testing.T, f *agentAdmissionFixture) (agentAdmission, error) {
		t.Helper()
		return resolveAgentAdmission(ctx, f.store, f.selection, f.run.ID, domain.RoleImplementer, prompt, agentTestAt)
	}
	assertSelected := func(t *testing.T, f *agentAdmissionFixture, admitted agentAdmission) {
		t.Helper()
		binding := admitted.binding
		if binding.SelectionModelID != "claude-implementer-test" || binding.SelectionEffort != "high" ||
			binding.SelectionDigest != admitted.resolved.Selection.Digest {
			t.Fatalf("binding selection = %s %s %s, want the agent's reduction %+v",
				binding.SelectionDigest, binding.SelectionModelID, binding.SelectionEffort, admitted.resolved.Selection)
		}
		if admitted.resolved.Selection.AdapterDigest != admitted.resolved.Adapter.Digest ||
			admitted.resolved.Selection.EnrollmentID != f.enrollment.ID {
			t.Fatalf("selection = %+v, want the agent's adapter and enrollment", admitted.resolved.Selection)
		}
	}

	t.Run("no catalog admits under the bridge", func(t *testing.T) {
		t.Parallel()
		f := newAgentAdmissionFixture(t)
		admitted, err := admit(t, f)
		if err != nil {
			t.Fatal(err)
		}
		assertSelected(t, f, admitted)
	})
	t.Run("a catalog listing the model and effort admits", func(t *testing.T) {
		t.Parallel()
		f := newAgentAdmissionFixture(t)
		f.recordCatalog(t, f.enrollment.ID, "claude-implementer-test", "low", "high", "xhigh")
		admitted, err := admit(t, f)
		if err != nil {
			t.Fatal(err)
		}
		assertSelected(t, f, admitted)
	})

	refusals := []struct {
		name    string
		efforts []string
		model   string
		want    string
	}{
		{"a catalog lacking the model", []string{"high"}, "claude-other-test", "is not in enrollment"},
		{
			"a catalog whose model lacks the effort",
			[]string{"low", "medium"},
			"claude-implementer-test",
			"is not one the model reports",
		},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAgentAdmissionFixture(t)
			f.recordCatalog(t, f.enrollment.ID, tc.model, tc.efforts...)
			_, err := admit(t, f)
			if !errors.Is(err, ErrAgentNotAdmissible) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resolve = %v, want a refusal naming %q", err, tc.want)
			}
			assertAdmissionDetail(t, err, domain.AdmissionCheckAgentResolution,
				agentTestWriterAgent, agentTestLaunch(t, f, domain.RoleImplementer))
		})
	}

	t.Run("two adapters covering the launch refuse as ambiguous", func(t *testing.T) {
		t.Parallel()
		f := newAgentAdmissionFixture(t)
		second, err := agentbaseline.ClaudeWardAdapter()
		if err != nil {
			t.Fatal(err)
		}
		second.AdapterBuild = "claude_code_ward_v2"
		if second.Digest, err = second.ComputeDigest(); err != nil {
			t.Fatal(err)
		}
		f.selection.Tree.Adapters = append(slices.Clone(f.selection.Tree.Adapters),
			agenttree.Adapter{Name: "claude_code_ward_v2", Fragment: second})
		_, err = admit(t, f)
		if !errors.Is(err, ErrAgentNotAdmissible) || !strings.Contains(err.Error(), "more than one adapter") {
			t.Fatalf("resolve = %v, want an ambiguous-adapter refusal", err)
		}
		assertAdmissionDetail(t, err, domain.AdmissionCheckAgentResolution,
			agentTestWriterAgent, agentTestLaunch(t, f, domain.RoleImplementer))
	})
	t.Run("a full model id an alias resolves to admits", func(t *testing.T) {
		t.Parallel()
		f := newAgentAdmissionFixture(t)
		catalog := domain.DiscoveredModelCatalog{
			EnrollmentID: f.enrollment.ID, HarnessBuild: "2.1.0 (Claude Code)", ObservedAt: agentTestAt.Add(-time.Minute),
			Models: []domain.DiscoveredModel{{
				ID: "implementer", ResolvedModel: new("claude-implementer-test"), DisplayName: "Implementer",
				EffortLevels: []string{"high"},
			}},
		}
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.RecordDiscoveredModelCatalog(ctx, catalog)
		}); err != nil {
			t.Fatal(err)
		}
		admitted, err := admit(t, f)
		if err != nil {
			t.Fatal(err)
		}
		assertSelected(t, f, admitted)
	})

	t.Run("the attempt's admission records the selection", func(t *testing.T) {
		t.Parallel()
		f := newAgentAdmissionFixture(t)
		admission, admitted, err := f.engine.admitAttempt(ctx, f.binding, f.stage, f.binding.invocation.ID)
		if err != nil || !admitted {
			t.Fatalf("admitAttempt = %t, %v", admitted, err)
		}
		if b := admission.AgentBinding; b.SelectionDigest == "" || b.SelectionModelID != "claude-implementer-test" ||
			b.SelectionEffort != "high" {
			t.Fatalf("admitted binding = %+v", *b)
		}
		// The store's write gate accepts it.
		f.record(t, admission)
	})
}

func TestReviewAdmissionRecordsTheSelection(t *testing.T) {
	t.Parallel()
	t.Run("claude reviewer", func(t *testing.T) {
		t.Parallel()
		f := newAgentAdmissionFixture(t)
		routed, _ := f.reviewTree(t)
		admission, err := f.admitReview(t, routed)
		if err != nil {
			t.Fatal(err)
		}
		if b := admission.AgentBinding; b.SelectionDigest == "" || b.SelectionModelID != "claude-review-test" ||
			b.SelectionEffort != "high" {
			t.Fatalf("review binding = %+v", b)
		}
		f.recordCatalog(t, f.enrollment.ID, "claude-implementer-test", "high")
		_, err = f.admitReview(t, routed)
		if !errors.Is(err, ErrAgentNotAdmissible) || !strings.Contains(err.Error(), "is not in enrollment") {
			t.Fatalf("review with a catalog lacking the reviewer's model = %v", err)
		}
	})
	t.Run("codex reviewer without a catalog", func(t *testing.T) {
		t.Parallel()
		f := newAgentAdmissionFixture(t)
		spec := f.codexReviewTree(t)
		admission, err := f.admitReview(t, spec)
		if err != nil {
			t.Fatal(err)
		}
		// Codex sends max as xhigh, and the selection records what it sends.
		if b := admission.AgentBinding; b.SelectionDigest == "" || b.SelectionModelID != "gpt-review-test" ||
			b.SelectionEffort != "xhigh" || b.NativeEffort != "xhigh" {
			t.Fatalf("codex review binding = %+v", b)
		}
	})
	// The README's review upgrade lets codex_review_v1 stay beside v2. It
	// sends only harness_default, so it never competes for the reviewer's
	// effort.
	t.Run("codex reviewer on a tree keeping codex_review_v1", func(t *testing.T) {
		t.Parallel()
		f := newAgentAdmissionFixture(t)
		spec := f.codexReviewTree(t)
		v1, err := agentbaseline.CodexReviewAdapterV1()
		if err != nil {
			t.Fatal(err)
		}
		f.selection.Tree.Adapters = append(slices.Clone(f.selection.Tree.Adapters),
			agenttree.Adapter{Name: agentbaseline.CodexReviewAdapterV1Build, Fragment: v1})
		if _, err := f.admitReview(t, spec); err != nil {
			t.Fatal(err)
		}
	})
}

// codexReviewTree seeds a Codex review identity and gives the fixture the
// baseline tree with its Codex reviewer, returning the reviewer's role.
func (f *agentAdmissionFixture) codexReviewTree(t *testing.T) ReviewRole {
	t.Helper()
	ctx := context.Background()
	identity := domain.AuthIdentity{
		ID: "codex-review", Provider: "codex", AccountBinding: "acct-fixture-0003",
		AuthStoreMutationLease: true, Enabled: true, CostOwner: "operator",
		Interim: domain.InterimClientFacts{AuthStoreVolume: "codex-interim", RefreshStrategy: domain.RefreshOnDemand},
	}
	enrollment := domain.ClientEnrollment{
		ID: "codex-review/codex_cli", AuthIdentityID: identity.ID,
		HarnessClient: domain.HarnessClientCodexCLI, Route: "openai_chatgpt_codex",
		AuthMethod: domain.AuthMethodOAuth, CredentialMode: domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: identity.AccountBinding,
	}
	manifest := agentTestDigest("9")
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordAuthIdentity(ctx, identity, agentTestAt.Add(-4*time.Minute)); err != nil {
			return err
		}
		if err := tx.RecordClientEnrollment(ctx, enrollment, agentTestAt.Add(-3*time.Minute)); err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, identity.ID, "inv-adopt-codex",
			&domain.LeaseGenerationBinding{
				EnrollmentID: enrollment.ID, Generation: 0,
				AuthStoreVolume: "codex-generation", StoreManifestDigest: manifest,
			}, agentTestAt.Add(-2*time.Minute), agentTestAt.Add(10*time.Minute))
		if err != nil {
			return err
		}
		_, err = tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: enrollment.ID, AuthStoreVolume: "codex-generation",
			StoreManifestDigest: manifest, LeaseFence: lease.Fence,
			TokenExpiry:    new(agentTestAt.Add(48 * time.Hour)),
			AccountBinding: identity.AccountBinding, RecordedAt: agentTestAt.Add(-time.Minute),
		}, agentTestAt.Add(-time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	prompts := make(map[domain.RoleName]agentbaseline.Prompt)
	for role, prompt := range agentTestPrompts {
		prompts[role] = prompt
	}
	prompts[domain.RoleReviewer] = agentbaseline.Prompt{Name: "review", Digest: agentTestDigest("6")}
	tree, err := agentbaseline.Tree(agentbaseline.TreeInput{
		ClaudeEnrollment: f.enrollment, CodexEnrollment: &enrollment,
		Writers: agentTestWriters(), ReviewModel: "gpt-review-test", ReviewEffort: domain.EffortMax,
		TermsBasisDate: "2026-08-01", PricingRevision: "pricing-2026-08",
		OfferNotAfter: agentTestAt.Add(24 * time.Hour), Prompts: prompts,
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := agenttree.Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if f.selection.LineupRevision, err = agenttree.Revision(files); err != nil {
		t.Fatal(err)
	}
	if f.selection.Tree, err = agenttree.Parse(files); err != nil {
		t.Fatal(err)
	}
	return ReviewRole{
		Role: domain.RoleReviewer, PromptName: "review", PromptDigest: agentTestDigest("6"),
		HarnessClient: domain.HarnessClientCodexCLI, Vendor: domain.AgentVendorCodex,
		EffectiveEgress: []string{"chatgpt.com:443"},
	}
}
