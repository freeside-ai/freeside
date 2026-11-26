package integration_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestReviewApprovalReadsTheRunsProjectRepository: two projects name two
// repositories, the composed one's trust profile approving the daemon's
// review configuration and the run's project's profile pinning another.
// Admission reads the profile of the run's project's repository, so the run
// holds unstarted; once that profile approves the configuration, it starts.
func TestReviewApprovalReadsTheRunsProjectRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openUnattendedFixture(t)
	composed := unattendedTrustProfile(t)
	other := unattendedTrustProfileFor(t, "freeside-ai/other-repo", 424243, "sha256:review-config-other")
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, other, admittedAt)
	}); err != nil {
		t.Fatal(err)
	}
	env := admissionEnvironment()
	env.OperatingMode = domain.ModeUnattended
	env.ReviewConfigurationDigest = composed.Review.ConfigDigest
	env.Base.Repo, env.Base.RepositoryID = composed.Repo, composed.RepositoryID
	warded := domain.BackendFreshVMReadOnlyVolumeHandoff
	// Every project but the composed one names the other repository, so the
	// fixture's run belongs to it.
	projects := func(_ context.Context, _ *store.ReadTx, id domain.ProjectID) (engine.ProjectAuthority, error) {
		authority := engine.ProjectAuthority{
			ProjectID: id, Repo: other.Repo, RepositoryID: other.RepositoryID,
			BaseRef: env.Base.BaseRef, WriterClass: &warded,
		}
		if id == "project-composed" {
			authority.Repo, authority.RepositoryID = composed.Repo, composed.RepositoryID
		}
		return authority, nil
	}
	backend := fake.RunnerBackend{
		BackendName: string(domain.BackendFreshVMReadOnlyVolumeHandoff),
		Caps:        exec.NewCapabilitySet(conformantCeiling(t)...),
	}
	workflow, err := engine.New(f.store, f.signet, f.driver,
		engine.WithAdmission(backend, []exec.Capability{exec.CapPostExitExport}, env,
			func() time.Time { return admittedAt }),
		engine.WithProjectAuthority(projects),
		engine.WithAdmissionDerivation(func(
			_ context.Context, _ domain.InvocationID, project engine.ProjectAuthority,
		) (string, domain.BaseRevision, error) {
			base := env.Base
			base.Repo, base.RepositoryID, base.BaseRef = project.Repo, project.RepositoryID, project.BaseRef
			return env.Workspace, base, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	f.engine = workflow
	invocationID, held, err := dispatchOneInvocation(t, f)
	if err != nil {
		t.Fatalf("review refusal = %v, want a quiet hold", err)
	}
	if held.InvocationsStarted != 0 {
		t.Fatalf("a run admitted under the composed repository's review approval: started %d", held.InvocationsStarted)
	}
	assertNoAttemptRecorded(t, f, invocationID)

	approved := unattendedTrustProfileFor(t, other.Repo, other.RepositoryID, composed.Review.ConfigDigest)
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, approved, admittedAt.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	resumed, err := f.engine.Reconcile(ctx)
	if err != nil || resumed.InvocationsStarted != 1 {
		t.Fatalf("after the run's repository approved the configuration: started %d, %v", resumed.InvocationsStarted, err)
	}
}

// TestPublishGateRefusesAnotherProjectsRepository: a run admitted at its
// project's repository whose project names another repository ID by
// publication time holds at the publish gate with nothing pushed or opened.
// Once the project names the admitted repository again, it publishes.
func TestPublishGateRefusesAnotherProjectsRepository(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	var moved atomic.Bool
	p.engineOptions = append(p.engineOptions, engine.WithProjectAuthority(func(
		_ context.Context, _ *store.ReadTx, id domain.ProjectID,
	) (engine.ProjectAuthority, error) {
		authority := engine.ProjectAuthority{
			ProjectID: id, Repo: fakePublicationRepo, RepositoryID: p.profile.RepositoryID, BaseRef: "main",
			WriterClass: new(domain.BackendFreshVMReadOnlyVolumeHandoff),
		}
		if moved.Load() {
			authority.RepositoryID++
		}
		return authority, nil
	}))
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)

	moved.Store(true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("a refused publication failed the pass: %v", err)
	}
	if pulls, pushes := len(p.forge.PullRequests()), p.transport.PushCount(); pulls != 0 || pushes != 0 {
		t.Fatalf("publication under another repository opened %d pull requests and pushed %d times", pulls, pushes)
	}

	moved.Store(false)
	p.now = p.now.Add(2 * time.Minute)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || len(p.forge.PullRequests()) != 1 {
		t.Fatalf("after the project named the admitted repository again: %#v, %v, %d pull requests",
			result, err, len(p.forge.PullRequests()))
	}
}

// recipeAuthority installs a project authority source on p in which the
// run's project approves the recipe current holds and another project
// approves the harness's real recipe, so a gate that read the other project,
// or a union of every project's recipe, would pass where the run's own
// project's recipe fails.
func recipeAuthority(p *productionPublicationHarness, current *atomic.Pointer[domain.Digest]) {
	p.engineOptions = append(p.engineOptions, engine.WithProjectAuthority(func(
		_ context.Context, _ *store.ReadTx, id domain.ProjectID,
	) (engine.ProjectAuthority, error) {
		authority := engine.ProjectAuthority{
			ProjectID: id, Repo: fakePublicationRepo, RepositoryID: p.profile.RepositoryID, BaseRef: "main",
			WriterClass: new(domain.BackendFreshVMReadOnlyVolumeHandoff), ApprovedRecipe: p.recipeD,
		}
		if id == "project-production-publication" {
			authority.ApprovedRecipe = *current.Load()
		}
		return authority, nil
	}))
}

// TestRecipeGateReadsTheRunsProjectRecipe: the publication lane's recipe set
// is the operator's recipes plus the run's own project's current recipe
// (#3209). Another project approving the run's recipe never counts, and an
// edit to the run's project's recipe holds or releases the run at its next
// recipe gate, with the same engine and no restart.
func TestRecipeGateReadsTheRunsProjectRecipe(t *testing.T) {
	t.Parallel()
	unrelated := productionDigest([]byte("unrelated recipe"))
	operator := map[domain.Digest]bool{productionDigest([]byte("operator recipe")): true}

	t.Run("another project's recipe never counts", func(t *testing.T) {
		t.Parallel()
		p := newProductionPublicationHarness(t, "")
		var current atomic.Pointer[domain.Digest]
		current.Store(&unrelated)
		recipeAuthority(p, &current)
		p.startAndRecordExport(t)
		p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{}, true, operator)
		result, err := p.reconcileLanes()
		if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
			t.Fatalf("recipe only another project approves: %#v, %v", result, err)
		}
		blocked, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(blocked.Item.Reason, "no longer approves") {
			t.Fatalf("blocked reason = %q, want the recipe refusal", blocked.Item.Reason)
		}
		if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
			t.Fatalf("a recipe only another project approves caused effects: %d/%d", refs, prs)
		}
	})

	t.Run("a live edit holds and releases the run", func(t *testing.T) {
		t.Parallel()
		p := newProductionPublicationHarness(t, "")
		var current atomic.Pointer[domain.Digest]
		current.Store(&p.recipeD)
		recipeAuthority(p, &current)
		p.startAndRecordExport(t)
		var stopped atomic.Bool
		p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{
			afterVerification: func() error {
				if stopped.CompareAndSwap(false, true) {
					return errors.New("stop after checkpoint")
				}
				return nil
			},
		}, true, operator)
		if _, err := p.reconcileLanes(); err == nil {
			t.Fatal("verification seam did not stop after checkpoint")
		}

		current.Store(&unrelated)
		result, err := p.reconcileLanes()
		if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
			t.Fatalf("after the project's recipe changed: %#v, %v", result, err)
		}
		hold, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(hold.Item.Reason, "Restore that approval to recover") {
			t.Fatalf("hold reason = %q, want the recoverable recipe hold", hold.Item.Reason)
		}
		if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
			t.Fatalf("a changed recipe caused effects: %d/%d", refs, prs)
		}

		current.Store(&p.recipeD)
		p.now = p.now.Add(2 * time.Minute)
		result, err = p.reconcileLanes()
		if err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
			t.Fatalf("after the project's recipe was restored: %#v, %v", result, err)
		}
		p.assertReady(t)
	})
}
