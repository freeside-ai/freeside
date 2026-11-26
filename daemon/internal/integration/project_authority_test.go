package integration_test

import (
	"context"
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
