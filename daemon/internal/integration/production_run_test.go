package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func productionPublicationMetadata() engine.ProductionPublication {
	return engine.ProductionPublication{
		Title: "Test production work item",
		Body:  "## Why\n\nExercise the production pipeline.\n",
		CommitAuthor: engine.ProductionCommitAuthor{
			AppSlug: "freeside-test", BotUserID: 12345,
		},
	}
}

func TestRealRunAttentionStateDetectsOpenPublicationBlock(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-real-attention-state")
	otherRunID := domain.RunID("run-other")
	items := []store.Snapshotted[domain.AttentionItem]{
		{Value: domain.AttentionItem{
			ID: "ready-other", Type: domain.AttentionReadyForFinalReview,
			Subject: domain.Subject{RunID: &otherRunID}, Status: domain.StatusOpen,
		}},
		{Value: domain.AttentionItem{
			ID: "blocked-old", Type: domain.AttentionPublishBlocked,
			Subject: domain.Subject{RunID: &runID}, Status: domain.StatusSuperseded,
		}},
		{Value: domain.AttentionItem{
			ID: "blocked-current", Type: domain.AttentionPublishBlocked,
			Subject: domain.Subject{RunID: &runID}, Status: domain.StatusOpen,
		}},
	}
	ready, blocked, err := realRunAttentionState(items, runID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.ID != "" || blocked.ID != "blocked-current" {
		t.Fatalf("real-run attention state = ready %q, blocked %q", ready.ID, blocked.ID)
	}
	_, blocked, err = realRunAttentionState(items[:2], runID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.ID != "" {
		t.Fatalf("superseded publication hold remained blocking: %q", blocked.ID)
	}
	items = append(items, items[len(items)-1])
	if _, _, err := realRunAttentionState(items, runID); err == nil ||
		!strings.Contains(err.Error(), "multiple open publish-blocked") {
		t.Fatalf("duplicate open block error = %v", err)
	}
}

// The production lane (#237): `freesided submit` persists a run whose
// implement stage the engine dispatches from an artifact-bound invocation
// (no conversation), and whose terminal result it records at most once,
// surfacing failure as an execution_failure item instead of wedging the
// reconcile loop.

const productionTerminalKind = "production_stage_terminal"

var derivedBase = domain.BaseRevision{
	Repo: "freeside-ai/candidate-repo", RepositoryID: 424242,
	BaseRef: "refs/heads/main", BaseSHA: "feedc0de",
}

type preJobRefusingDriver struct{ exec.StageDriver }

func (d preJobRefusingDriver) Start(
	context.Context, domain.InvocationID, exec.StartSpec,
) error {
	return fmt.Errorf("pre-job probe: %w", exec.ErrPreJobRefused)
}

type selectiveStartRefusingDriver struct {
	exec.StageDriver
	invocationID domain.InvocationID
	err          error
}

func (d selectiveStartRefusingDriver) Start(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	if id == d.invocationID {
		return d.err
	}
	return d.StageDriver.Start(ctx, id, spec)
}

// openProductionFixture composes a store, signet, fake driver, and an engine
// whose admission is configured with per-attempt workspace/base derivation,
// the shape the production composition uses.
func openProductionFixture(t *testing.T) *workflowFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "freeside.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: {},
		},
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, testIdentity, admittedAt)
	}); err != nil {
		t.Fatalf("record auth identity: %v", err)
	}
	blobs, err := signet.NewBlobStore(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatalf("signet.NewBlobStore: %v", err)
	}
	attention := signet.NewService(st, signet.WithBlobStore(blobs))
	driver, err := fake.NewStageDriverAt(filepath.Join(root, "driver"))
	if err != nil {
		t.Fatalf("fake.NewStageDriverAt: %v", err)
	}
	backend := fake.RunnerBackend{
		BackendName: "fake_runner",
		Caps:        exec.NewCapabilitySet(exec.CapDetachableWorkspace, exec.CapPostExitExport),
	}
	workflow, err := engine.New(st, attention, driver,
		engine.WithAdmission(backend, nil, admissionEnvironment(), func() time.Time { return admittedAt }),
		engine.WithAdmissionDerivation(func(_ context.Context, invocationID domain.InvocationID) (string, domain.BaseRevision, error) {
			return "freeside-handoff-" + string(invocationID) + "-ws", derivedBase, nil
		}),
	)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return &workflowFixture{root: root, store: st, signet: attention, driver: driver, engine: workflow}
}

func unattendedProductionOptions(t *testing.T) []engine.Option {
	t.Helper()
	profile := unattendedTrustProfile(t)
	env := admissionEnvironment()
	env.OperatingMode = domain.ModeUnattended
	env.Base.Repo, env.Base.RepositoryID = profile.Repo, profile.RepositoryID
	backend := fake.RunnerBackend{
		BackendName: string(domain.BackendFreshVMReadOnlyVolumeHandoff),
		Caps:        exec.NewCapabilitySet(conformantCeiling(t)...),
	}
	return []engine.Option{
		engine.WithAdmission(
			backend, []exec.Capability{exec.CapPostExitExport}, env,
			func() time.Time { return admittedAt },
		),
		engine.WithAdmissionDerivation(func(
			_ context.Context, invocationID domain.InvocationID,
		) (string, domain.BaseRevision, error) {
			return "freeside-handoff-" + string(invocationID) + "-ws", derivedBase, nil
		}),
	}
}

// submissionDigest derives a distinct canonical digest per run and role, so
// retargeting tests exercise the run-level binding check rather than
// colliding on shared fixture bytes.
func submissionDigest(runID, role string) domain.Digest {
	sum := sha256.Sum256([]byte(runID + "/" + role))
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// registerSubmissionArtifacts persists the digest-addressed specification and
// policy artifacts submit binds a run to.
func registerSubmissionArtifacts(
	t *testing.T, st *store.Store, runID string,
) (domain.Artifact, domain.Artifact, domain.ResolvedPolicy) {
	return registerSubmissionArtifactsWithPaths(t, st, runID, "daemon/**")
}

func registerSubmissionArtifactsWithPaths(
	t *testing.T, st *store.Store, runID, paths string,
) (domain.Artifact, domain.Artifact, domain.ResolvedPolicy) {
	t.Helper()
	ctx := context.Background()
	spec, err := domain.NewArtifact(domain.ArtifactInput{
		ID: domain.ArtifactID("artifact-spec-" + runID), Type: "specification",
		Digest: submissionDigest(runID, "specification"),
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: domain.InvocationID("submit-" + runID),
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
	}, nil)
	if err != nil {
		t.Fatalf("new spec artifact: %v", err)
	}
	resolved, err := domain.NewResolvedPolicy(domain.RunID(runID), []domain.PolicyKey{{
		Key: "paths", Value: paths,
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest(runID, "policy-source"),
		},
	}})
	if err != nil {
		t.Fatalf("new resolved policy: %v", err)
	}
	policy, err := domain.NewArtifact(domain.ArtifactInput{
		ID: domain.ArtifactID("artifact-policy-" + runID), Type: "policy",
		Digest: resolved.Digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: domain.InvocationID("submit-" + runID),
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
	}, nil)
	if err != nil {
		t.Fatalf("new policy artifact: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, spec); err != nil {
			return err
		}
		return tx.PutArtifact(ctx, policy)
	}); err != nil {
		t.Fatalf("register artifacts: %v", err)
	}
	return spec, policy, resolved
}

func seedLegacyProductionRun(
	t *testing.T,
	st *store.Store,
	runID domain.RunID,
	projectID domain.ProjectID,
	spec domain.Artifact,
	policy domain.Artifact,
	resolved domain.ResolvedPolicy,
) engine.ProductionRun {
	t.Helper()
	ctx := context.Background()
	stageID := domain.StageID("implement-" + string(runID))
	invocationID := domain.InvocationID("inv-implement-" + string(runID))
	invocation, err := domain.NewAgentInvocation(invocationID, []domain.ArtifactID{spec.ID}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: runID, ProjectID: projectID, SpecDigest: spec.Digest, PolicyDigest: policy.Digest,
		Stages: []domain.Stage{{ID: stageID, RunID: runID, Name: "implement", Attempts: []domain.Attempt{}}},
	}
	payload := []byte(fmt.Sprintf(
		`{"invocation_id":%q,"run_id":%q,"stage_id":%q}`,
		invocationID, runID, stageID,
	))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, resolved); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		_, inserted, err := tx.EnqueueOutbox(
			ctx, string(invocationID), engine.KindProductionInvocationRequested, payload,
		)
		if err == nil && !inserted {
			return errors.New("legacy production marker already existed")
		}
		return err
	}); err != nil {
		t.Fatalf("seed legacy production run: %v", err)
	}
	return engine.ProductionRun{Run: run, InvocationID: invocationID, StageID: stageID}
}

func reviseWaivedTrustProfile(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	revised, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "freeside-ai/candidate-repo", RepositoryID: 424242,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit-v2",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatalf("new revised trust profile: %v", err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, revised, admittedAt.Add(time.Hour))
	}); err != nil {
		t.Fatalf("activate revised trust profile: %v", err)
	}
}

func TestSubmitProductionRunConvergesAndRefusesRetargeting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openProductionFixture(t)
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-submit")

	submission := engine.ProductionRunSpec{
		RunID: "run-prod-submit", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	}
	first, err := engine.SubmitProductionRun(ctx, f.store, submission)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if first.Run.SpecDigest != spec.Digest || first.Run.PolicyDigest != policy.Digest {
		t.Fatalf("run digests = %q/%q, want the registered artifacts'",
			first.Run.SpecDigest, first.Run.PolicyDigest)
	}
	if len(first.Run.Stages) != 1 || first.Run.Stages[0].Name != "implement" {
		t.Fatalf("run stages = %#v, want one implement stage", first.Run.Stages)
	}

	replay, err := engine.SubmitProductionRun(ctx, f.store, submission)
	if err != nil {
		t.Fatalf("replay submit: %v", err)
	}
	if replay.InvocationID != first.InvocationID || replay.StageID != first.StageID {
		t.Fatalf("replay identities differ: %#v vs %#v", replay, first)
	}

	// A retry that would retarget the stored run's fixed bindings fails.
	otherSpec, otherPolicy, otherResolved := registerSubmissionArtifacts(t, f.store, "run-prod-other")
	otherResolved.RunID = "run-prod-submit"
	_, err = engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-submit", ProjectID: "proj-prod",
		SpecArtifactID: otherSpec.ID, PolicyArtifactID: otherPolicy.ID,
		ResolvedPolicy: otherResolved, Publication: productionPublicationMetadata(),
	})
	if !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("retargeting submit error = %v, want ErrImmutableTransition", err)
	}

	// A submission for unregistered artifacts is refused outright.
	unregisteredResolved := resolved
	unregisteredResolved.RunID = "run-prod-unregistered"
	_, err = engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-unregistered", ProjectID: "proj-prod",
		SpecArtifactID: "artifact-missing", PolicyArtifactID: policy.ID,
		ResolvedPolicy: unregisteredResolved, Publication: productionPublicationMetadata(),
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unregistered submit error = %v, want ErrNotFound", err)
	}
}

func TestSubmitProductionRunRefusesPreexistingPublicationIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openProductionFixture(t)
	runID := "run-prod-occupied-publisher"
	publicationID := domain.InvocationID("publish-production-" + runID)
	intent := publish.Intent{
		Identity:        submissionDigest(runID, "publication-identity"),
		InvocationID:    publicationID,
		Repo:            derivedBase.Repo,
		BaseRef:         derivedBase.BaseRef,
		SourceHeadSHA:   derivedBase.BaseSHA,
		AuthorizationID: submissionDigest(runID, "authorization"),
	}
	payload, err := intent.Encode()
	if err != nil {
		t.Fatal(err)
	}
	key, err := publish.IntentKey(publicationID, publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, key, publish.IntentKindPublication, payload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, runID)
	_, err = engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: domain.RunID(runID), ProjectID: "proj-prod-occupied-publisher",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID, ResolvedPolicy: resolved,
		Publication: productionPublicationMetadata(),
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("submit with occupied publication key error = %v", err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetRun(ctx, domain.RunID(runID)); !errors.Is(err, store.ErrNotFound) {
			if err == nil {
				return errors.New("new run survived refused submission")
			}
			return fmt.Errorf("read run after refused submission: %w", err)
		}
		if _, err := tx.GetOutbox(ctx, "inv-implement-"+runID); !errors.Is(err, store.ErrNotFound) {
			if err == nil {
				return errors.New("new dispatch survived refused submission")
			}
			return fmt.Errorf("read dispatch after refused submission: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitProductionRunRejectsArtifactRoleSubstitution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openProductionFixture(t)

	tests := []struct {
		name        string
		replaceSpec bool
	}{
		{name: "specification", replaceSpec: true},
		{name: "policy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run-prod-wrong-" + tc.name
			spec, policy, resolved := registerSubmissionArtifacts(t, f.store, runID)
			foreign := spec
			if tc.replaceSpec {
				foreign.ID = domain.ArtifactID("artifact-wrong-spec-role")
				foreign.Type = "evidence"
				spec = foreign
			} else {
				foreign = policy
				foreign.ID = domain.ArtifactID("artifact-wrong-policy-role")
				foreign.Type = "evidence"
				policy = foreign
			}
			if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutArtifact(ctx, foreign)
			}); err != nil {
				t.Fatalf("register substituted artifact: %v", err)
			}

			_, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
				RunID: domain.RunID(runID), ProjectID: "proj-prod",
				SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
				ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("substituted %s error = %v, want ErrParentKeyMismatch", tc.name, err)
			}
		})
	}
}

func TestSubmitProductionRunRefusesRetroactiveOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("existing run without marker", func(t *testing.T) {
		f := openProductionFixture(t)
		spec, policy, resolved := registerSubmissionArtifacts(
			t, f.store, "run-prod-foreign",
		)
		foreign := domain.Run{
			ID: "run-prod-foreign", ProjectID: "proj-prod",
			SpecDigest: spec.Digest, PolicyDigest: policy.Digest,
			Stages: []domain.Stage{{
				ID: "implement-run-prod-foreign", RunID: "run-prod-foreign",
				Name: "implement", Attempts: []domain.Attempt{},
			}},
		}
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, foreign)
		}); err != nil {
			t.Fatalf("seed foreign run: %v", err)
		}

		_, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
			RunID: "run-prod-foreign", ProjectID: "proj-prod",
			SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
			ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
		})
		if !errors.Is(err, domain.ErrImmutableTransition) {
			t.Fatalf("claim foreign run error = %v, want ErrImmutableTransition", err)
		}
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetOutbox(ctx, "inv-implement-run-prod-foreign")
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("foreign run gained marker: %v", err)
		}
	})

	t.Run("new run with orphan marker", func(t *testing.T) {
		f := openProductionFixture(t)
		spec, policy, resolved := registerSubmissionArtifacts(
			t, f.store, "run-prod-orphan-marker",
		)
		payload := []byte(
			`{"invocation_id":"inv-implement-run-prod-orphan-marker",` +
				`"run_id":"run-prod-orphan-marker",` +
				`"stage_id":"implement-run-prod-orphan-marker"}`,
		)
		if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, _, err := tx.EnqueueOutbox(
				ctx, "inv-implement-run-prod-orphan-marker",
				engine.KindProductionInvocationRequested, payload,
			)
			return err
		}); err != nil {
			t.Fatalf("seed orphan marker: %v", err)
		}

		_, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
			RunID: "run-prod-orphan-marker", ProjectID: "proj-prod",
			SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
			ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
		})
		if !errors.Is(err, domain.ErrImmutableTransition) {
			t.Fatalf("adopt orphan marker error = %v, want ErrImmutableTransition", err)
		}
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetRun(ctx, "run-prod-orphan-marker")
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("orphan marker gained a run: %v", err)
		}
	})

	t.Run("new run with orphan invocation", func(t *testing.T) {
		f := openProductionFixture(t)
		spec, policy, resolved := registerSubmissionArtifacts(
			t, f.store, "run-prod-orphan-invocation",
		)
		invocation, err := domain.NewAgentInvocation(
			"inv-implement-run-prod-orphan-invocation",
			[]domain.ArtifactID{spec.ID}, nil, 0,
		)
		if err != nil {
			t.Fatalf("new orphan invocation: %v", err)
		}
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutAgentInvocation(ctx, invocation)
		}); err != nil {
			t.Fatalf("seed orphan invocation: %v", err)
		}

		_, err = engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
			RunID: "run-prod-orphan-invocation", ProjectID: "proj-prod",
			SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
			ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
		})
		if !errors.Is(err, domain.ErrImmutableTransition) {
			t.Fatalf("adopt orphan invocation error = %v, want ErrImmutableTransition", err)
		}
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetRun(ctx, "run-prod-orphan-invocation")
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("orphan invocation gained a run: %v", err)
		}
	})
}

func TestAttendedProductionRunRemainsPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openProductionFixture(t)
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-1")

	submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-1", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		Outcome:         fake.OutcomeComplete,
		RunningInspects: 1,
		Result: exec.StageResult{
			Summary: "Implemented the work item.", HeadSHA: "cafe1234",
			Artifacts: []domain.Digest{submissionDigest("run-prod-1", "transcript")},
		},
	})

	result, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("attended reconcile: %v", err)
	}
	if result.InvocationsStarted != 0 || result.ResultsAccepted != 0 {
		t.Fatalf("attended production result = %#v, want durable hold", result)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		pending, err := tx.ListPendingOutbox(ctx, engine.KindProductionInvocationRequested)
		if err != nil {
			return err
		}
		if len(pending) != 1 || pending[0].IdempotencyKey != string(submitted.InvocationID) {
			t.Errorf("pending production intents = %#v, want submitted intent", pending)
		}
		admission, found, err := tx.LookupExecutionAdmission(ctx, submitted.InvocationID)
		if err != nil {
			return err
		}
		if found {
			t.Errorf("attended production intent gained admission %#v", admission)
		}
		run, err := tx.GetRun(ctx, "run-prod-1")
		if err != nil {
			return err
		}
		if len(run.Stages) != 1 || len(run.Stages[0].Attempts) != 0 {
			t.Errorf("attended production run gained attempts: %#v", run.Stages)
		}
		return nil
	}); err != nil {
		t.Fatalf("read attended hold: %v", err)
	}
	if replay, err := f.engine.Reconcile(ctx); err != nil || replay != (engine.ReconcileResult{}) {
		t.Fatalf("attended hold replay = %#v, %v", replay, err)
	}
}

func TestLegacyProductionRunFinishesWithoutPublicationAuthority(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openUnattendedFixture(t)
	runID := domain.RunID("run-prod-legacy-v1")
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, string(runID))
	submitted := seedLegacyProductionRun(t, f.store, runID, "proj-prod", spec, policy, resolved)
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete, RunningInspects: 1,
		Result: exec.StageResult{Summary: "completed legacy work", HeadSHA: "cafe1234"},
	})

	started, err := f.engine.Reconcile(ctx)
	if err != nil || started.InvocationsStarted != 1 {
		t.Fatalf("start legacy production = %#v, %v", started, err)
	}
	var admission domain.ExecutionAdmission
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, submitted.InvocationID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	executionExport, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: submitted.InvocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: "cafe1234",
		ManifestDigest: submissionDigest(string(runID), "manifest"), RecordedAt: admittedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RecordExecutionExport(ctx, f.store, executionExport, engine.ProductionReplay{}); err != nil {
		t.Fatalf("record legacy export-only completion: %v", err)
	}

	accepted := false
	for range 3 {
		result, err := f.engine.Reconcile(ctx)
		if err != nil {
			t.Fatalf("finish legacy production: %v", err)
		}
		accepted = accepted || result.ResultsAccepted == 1
	}
	if !accepted {
		t.Fatal("legacy completion was not accepted")
	}
	reservation, err := publish.NewReservation(
		domain.InvocationID("publish-production-"+string(runID)), runID,
	)
	if err != nil {
		t.Fatal(err)
	}
	reservationKey, err := reservation.Key()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetExecutionExportRecord(ctx, submitted.InvocationID); err != nil {
			return err
		}
		if _, err := tx.GetInbox(ctx, string(submitted.InvocationID)); err != nil {
			return err
		}
		if _, err := tx.GetOutbox(ctx, "production-publication/"+string(runID)); !errors.Is(err, store.ErrNotFound) {
			if err == nil {
				return errors.New("legacy run gained publication task")
			}
			return err
		}
		if _, err := tx.GetOutbox(ctx, reservationKey); !errors.Is(err, store.ErrNotFound) {
			if err == nil {
				return errors.New("legacy run gained publication reservation")
			}
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	emptyDriver, err := fake.NewStageDriverAt(filepath.Join(t.TempDir(), "empty-driver"))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := engine.New(
		f.store, f.signet, emptyDriver, unattendedProductionOptions(t)...,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := restarted.Reconcile(ctx); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("legacy terminal replay without private driver = %#v, %v", result, err)
	}

	var original store.QueueEntry
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		original, err = tx.GetOutbox(ctx, string(submitted.InvocationID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ProductionInvocationBackupPayloadDigests(original); err != nil {
		t.Fatalf("legacy marker poisoned backup closure: %v", err)
	}
	if _, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: runID, ProjectID: "proj-prod", SpecArtifactID: spec.ID,
		PolicyArtifactID: policy.ID, ResolvedPolicy: resolved,
		Publication: productionPublicationMetadata(),
	}); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("v2 metadata attached to legacy run: %v", err)
	}
	var current store.QueueEntry
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		current, err = tx.GetOutbox(ctx, string(submitted.InvocationID))
		return err
	}); err != nil || !reflect.DeepEqual(current, original) {
		t.Fatalf("legacy marker changed after refused v2 retry: %#v, %v", current, err)
	}
}

func TestLegacyCompletedTerminalWithoutExportStillRequiresDriverProof(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openUnattendedFixture(t)
	runID := domain.RunID("run-prod-legacy-forged-terminal")
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, string(runID))
	submitted := seedLegacyProductionRun(t, f.store, runID, "proj-prod", spec, policy, resolved)
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete, RunningInspects: 1,
		Result: exec.StageResult{Summary: "authentic legacy result", HeadSHA: "cafe1234"},
	})
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("dispatch legacy production: %v", err)
	}
	if status, err := f.driver.Inspect(ctx, submitted.InvocationID); err != nil || status.Status != exec.StatusCompleted {
		t.Fatalf("advance legacy driver = %q, %v", status.Status, err)
	}
	forged, err := json.Marshal(struct {
		InvocationID domain.InvocationID `json:"invocation_id"`
		RunID        domain.RunID        `json:"run_id"`
		StageID      domain.StageID      `json:"stage_id"`
		Status       exec.Status         `json:"status"`
		HeadSHA      string              `json:"head_sha"`
	}{
		InvocationID: submitted.InvocationID, RunID: runID, StageID: submitted.StageID,
		Status: exec.StatusCompleted, HeadSHA: "deadbeef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, inserted, err := tx.RecordInbox(
			ctx, string(submitted.InvocationID), productionTerminalKind, forged,
		)
		if err == nil && !inserted {
			return errors.New("forged legacy terminal already existed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Reconcile(ctx); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("legacy terminal without export = %v, want parent-key mismatch", err)
	}
}

func TestProductionCompletionHoldsAfterCurrentPolicyDrifts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openUnattendedFixture(t)
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-policy-hold")
	submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-policy-hold", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete, RunningInspects: 1,
		Result: exec.StageResult{Summary: "completed under the admitted profile"},
	})
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("dispatch reconcile: %v", err)
	}
	reviseWaivedTrustProfile(t, f.store)

	held, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("policy drift stopped the reconcile loop: %v", err)
	}
	if held.ResultsAccepted != 0 {
		t.Fatalf("ResultsAccepted = %d, want completed result held", held.ResultsAccepted)
	}
	err = f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetInbox(ctx, string(submitted.InvocationID))
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("held completion durable terminal = %v, want no accepted terminal", err)
	}
}

func TestUnpublishedProductionCompletionNeverCreatesATerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openUnattendedFixture(t)
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-policy-replay")
	submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-policy-replay", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete, RunningInspects: 1,
		Result: exec.StageResult{Summary: "accepted under the admitted profile"},
	})
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("dispatch reconcile: %v", err)
	}
	if accepted, err := f.engine.Reconcile(ctx); err == nil ||
		!strings.Contains(err.Error(), "publication workflow is not configured") ||
		accepted.ResultsAccepted != 0 {
		t.Fatalf("unpublished completion = %#v, %v", accepted, err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetInbox(ctx, string(submitted.InvocationID))
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unpublished completion terminal = %v, want none", err)
	}
}

func TestPreJobRefusalHoldsThePendingIntentWithoutStoppingReconcile(t *testing.T) {
	ctx := context.Background()
	f := openUnattendedFixture(t)
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-prejob")
	submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-prejob", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete, RunningInspects: 1,
		Result: exec.StageResult{Summary: "completed after the pre-job recovered"},
	})

	refusing, err := engine.New(
		f.store, f.signet, preJobRefusingDriver{StageDriver: f.driver},
		unattendedProductionOptions(t)...,
	)
	if err != nil {
		t.Fatal(err)
	}
	held, err := refusing.Reconcile(ctx)
	if err != nil {
		t.Fatalf("transient pre-job refusal stopped reconcile: %v", err)
	}
	if held.InvocationsStarted != 0 {
		t.Fatalf("pre-job refusal started %d invocations, want 0", held.InvocationsStarted)
	}

	resumed, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("healthy reconcile did not resume the pending intent: %v", err)
	}
	if resumed.InvocationsStarted != 1 {
		t.Fatalf("healthy reconcile started %d invocations, want 1", resumed.InvocationsStarted)
	}
}

func TestInputIORefusalHoldsThePendingIntentWithoutStoppingReconcile(t *testing.T) {
	ctx := context.Background()
	f := openUnattendedFixture(t)
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-input-io")
	submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-input-io", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete, RunningInspects: 1,
		Result: exec.StageResult{Summary: "completed after input storage recovered"},
	})

	refusing, err := engine.New(
		f.store, f.signet, startRefusingDriver{
			StageDriver: f.driver,
			err:         fmt.Errorf("materialize policy: %w", exec.ErrInputUnavailable),
		},
		unattendedProductionOptions(t)...,
	)
	if err != nil {
		t.Fatal(err)
	}
	held, err := refusing.Reconcile(ctx)
	if err != nil {
		t.Fatalf("transient input I/O stopped reconcile: %v", err)
	}
	if held.InvocationsStarted != 0 {
		t.Fatalf("input I/O refusal started %d invocations, want 0", held.InvocationsStarted)
	}

	resumed, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("healthy reconcile did not resume the pending intent: %v", err)
	}
	if resumed.InvocationsStarted != 1 {
		t.Fatalf("healthy reconcile started %d invocations, want 1", resumed.InvocationsStarted)
	}
}

func TestInputIORefusalDoesNotStarveLaterProductionIntent(t *testing.T) {
	ctx := context.Background()
	f := openUnattendedFixture(t)
	spec1, policy1, resolved1 := registerSubmissionArtifacts(t, f.store, "run-prod-input-held")
	held, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-input-held", ProjectID: "proj-prod",
		SpecArtifactID: spec1.ID, PolicyArtifactID: policy1.ID,
		ResolvedPolicy: resolved1, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec2, policy2, resolved2 := registerSubmissionArtifacts(t, f.store, "run-prod-input-healthy")
	healthy, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-input-healthy", ProjectID: "proj-prod",
		SpecArtifactID: spec2.ID, PolicyArtifactID: policy2.ID,
		ResolvedPolicy: resolved2, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []domain.InvocationID{held.InvocationID, healthy.InvocationID} {
		f.driver.Script(id, fake.StageScript{
			Outcome: fake.OutcomeComplete, RunningInspects: 3,
			Result: exec.StageResult{Summary: "completed after materialization"},
		})
	}

	selective, err := engine.New(
		f.store, f.signet, selectiveStartRefusingDriver{
			StageDriver: f.driver, invocationID: held.InvocationID,
			err: fmt.Errorf("materialize policy: %w", exec.ErrInputUnavailable),
		},
		unattendedProductionOptions(t)...,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := selective.Reconcile(ctx)
	if err != nil {
		t.Fatalf("selective input hold stopped reconcile: %v", err)
	}
	if result.InvocationsStarted != 1 {
		t.Fatalf("selective reconcile started %d invocations, want later healthy intent only",
			result.InvocationsStarted)
	}
	if _, err := f.driver.Inspect(ctx, held.InvocationID); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Fatalf("held invocation inspect = %v, want not started", err)
	}
	if status, err := f.driver.Inspect(ctx, healthy.InvocationID); err != nil ||
		status.Status != exec.StatusRunning {
		t.Fatalf("healthy invocation = %q, %v, want running", status.Status, err)
	}

	resumed, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("healthy reconcile did not resume held intent: %v", err)
	}
	if resumed.InvocationsStarted != 1 {
		t.Fatalf("healthy reconcile started %d invocations, want held intent", resumed.InvocationsStarted)
	}
}

func TestProductionRunRefusesAWellShapedForgedTerminalRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openUnattendedFixture(t)
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-forged-terminal")
	submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-forged-terminal", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete, RunningInspects: 1,
		Result: exec.StageResult{
			Summary: "Authentic result.", HeadSHA: "cafe1234",
			Artifacts: []domain.Digest{submissionDigest("run-prod-forged-terminal", "transcript")},
		},
	})
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("dispatch reconcile: %v", err)
	}
	if status, err := f.driver.Inspect(ctx, submitted.InvocationID); err != nil {
		t.Fatalf("advance scripted driver: %v", err)
	} else if status.Status != exec.StatusCompleted {
		t.Fatalf("scripted driver status = %q, want completed before terminal collection", status.Status)
	}

	forged, err := json.Marshal(struct {
		InvocationID domain.InvocationID `json:"invocation_id"`
		RunID        domain.RunID        `json:"run_id"`
		StageID      domain.StageID      `json:"stage_id"`
		Status       exec.Status         `json:"status"`
		HeadSHA      string              `json:"head_sha"`
	}{
		InvocationID: submitted.InvocationID,
		RunID:        "run-prod-forged-terminal",
		StageID:      submitted.StageID,
		Status:       exec.StatusCompleted,
		HeadSHA:      "deadbeef",
	})
	if err != nil {
		t.Fatalf("marshal forged terminal: %v", err)
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, inserted, err := tx.RecordInbox(
			ctx, string(submitted.InvocationID), productionTerminalKind, forged)
		if err == nil && !inserted {
			return errors.New("fixture: forged terminal row already existed")
		}
		return err
	}); err != nil {
		t.Fatalf("record forged terminal: %v", err)
	}

	if _, err := f.engine.Reconcile(ctx); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("reconcile forged terminal = %v, want ErrParentKeyMismatch", err)
	}
}

func TestProductionFailureSurfacesItemWithoutWedgingTheLoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name       string
		outcome    fake.Outcome
		wantStatus string
	}{
		{"failed result", fake.OutcomeFail, "failed"},
		{"lost session", fake.OutcomeCrashBeforeResult, "gone"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := openUnattendedFixture(t)
			runID := "run-prod-" + strings.ReplaceAll(tc.name, " ", "-")
			spec, policy, resolved := registerSubmissionArtifacts(t, f.store, runID)
			submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
				RunID: domain.RunID(runID), ProjectID: "proj-prod",
				SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
				ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
			})
			if err != nil {
				t.Fatalf("submit: %v", err)
			}
			f.driver.Script(submitted.InvocationID, fake.StageScript{
				Outcome: tc.outcome,
				Result:  exec.StageResult{Summary: "The stage went sideways."},
			})
			if _, err := f.engine.Reconcile(ctx); err != nil {
				t.Fatalf("dispatch reconcile: %v", err)
			}
			result, err := f.engine.Reconcile(ctx)
			if err != nil {
				t.Fatalf("terminal reconcile: %v", err)
			}
			if result.ResultsAccepted != 0 {
				t.Fatalf("ResultsAccepted = %d, want 0 for %s", result.ResultsAccepted, tc.name)
			}
			// The loop stays healthy on replay and writes nothing twice.
			if _, err := f.engine.Reconcile(ctx); err != nil {
				t.Fatalf("replay reconcile: %v", err)
			}

			if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
				entry, err := tx.GetInbox(ctx, string(submitted.InvocationID))
				if err != nil {
					return err
				}
				var terminal struct {
					Status string `json:"status"`
				}
				if err := json.Unmarshal(entry.Payload, &terminal); err != nil {
					return err
				}
				if terminal.Status != tc.wantStatus {
					t.Errorf("terminal status = %q, want %q", terminal.Status, tc.wantStatus)
				}
				item, err := tx.GetAttentionItem(ctx, domain.ItemID("execution-failure-"+string(submitted.InvocationID)))
				if err != nil {
					return err
				}
				if item.Type != domain.AttentionExecutionFailure || item.Status != domain.StatusOpen {
					t.Errorf("failure item = %q/%q, want open execution_failure", item.Type, item.Status)
				}
				if item.ItemVersion != 1 {
					t.Errorf("failure item version = %d, want 1 (no duplicate raise)", item.ItemVersion)
				}
				return nil
			}); err != nil {
				t.Fatalf("read failure state: %v", err)
			}
		})
	}
}

// TestSubmitProductionRunCapturesWorkUnitDeclaration (§5.18, issue #443):
// a declared submission persists the operator's work-unit declaration in
// the run's transaction, a converged replay re-states it, a disagreeing
// re-declaration is refused like any other fixed binding, and an
// undeclared submission records nothing.
func TestSubmitProductionRunCapturesWorkUnitDeclaration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openProductionFixture(t)
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-declared")

	boundIssue := 443
	unit := domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
		BoundIssue:          &boundIssue,
		DependsOnIssues:     []int{440, 442},
		// The engine records the input verbatim, and the store's read
		// re-gate requires the declared scope to equal the resolved
		// policy's paths key; the fixture policy declares daemon/**.
		DeclaredPaths:      []string{"daemon/**"},
		ContractSerialized: true,
	}
	submission := engine.ProductionRunSpec{
		RunID: "run-prod-declared", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
		WorkUnit: &unit,
	}
	if _, err := engine.SubmitProductionRun(ctx, f.store, submission); err != nil {
		t.Fatalf("submit: %v", err)
	}

	var stored domain.WorkUnitDeclaration
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetWorkUnitDeclarationByRun(ctx, "run-prod-declared")
		return err
	}); err != nil {
		t.Fatalf("read declaration: %v", err)
	}
	if stored.ID != domain.WorkUnitIDForRun("run-prod-declared") ||
		stored.BoundIssue == nil || *stored.BoundIssue != 443 ||
		len(stored.DependsOnIssues) != 2 || len(stored.DeclaredPaths) != 1 ||
		!stored.ContractSerialized {
		t.Fatalf("stored declaration = %+v", stored)
	}

	if _, err := engine.SubmitProductionRun(ctx, f.store, submission); err != nil {
		t.Fatalf("declared replay must converge: %v", err)
	}

	changed := unit
	changed.ContractSerialized = false
	divergent := submission
	divergent.WorkUnit = &changed
	if _, err := engine.SubmitProductionRun(ctx, f.store, divergent); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("divergent re-declaration error = %v, want ErrImmutableTransition", err)
	}

	invalid := unit
	invalid.BoundIssue = nil
	malformed := submission
	malformed.WorkUnit = &invalid
	if _, err := engine.SubmitProductionRun(ctx, f.store, malformed); !errors.Is(err, domain.ErrWorkUnitInconsistent) {
		t.Fatalf("criterion without bound issue error = %v, want ErrWorkUnitInconsistent", err)
	}

	otherSpec, otherPolicy, otherResolved := registerSubmissionArtifacts(t, f.store, "run-prod-undeclared")
	if _, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-undeclared", ProjectID: "proj-prod",
		SpecArtifactID: otherSpec.ID, PolicyArtifactID: otherPolicy.ID,
		ResolvedPolicy: otherResolved, Publication: productionPublicationMetadata(),
	}); err != nil {
		t.Fatalf("undeclared submit: %v", err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetWorkUnitDeclarationByRun(ctx, "run-prod-undeclared")
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("undeclared run's declaration read = %v, want ErrNotFound", err)
	}

	// Whether a run is declared is fixed at intake: re-submitting the
	// stored undeclared run with a declaration is refused, so a run that
	// may already be executing, published, or terminal can never gain a
	// retroactive unit.
	retroactive := engine.ProductionRunSpec{
		RunID: "run-prod-undeclared", ProjectID: "proj-prod",
		SpecArtifactID: otherSpec.ID, PolicyArtifactID: otherPolicy.ID,
		ResolvedPolicy: otherResolved, Publication: productionPublicationMetadata(),
		WorkUnit: &unit,
	}
	if _, err := engine.SubmitProductionRun(ctx, f.store, retroactive); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("retroactive declaration error = %v, want ErrImmutableTransition", err)
	}
}
