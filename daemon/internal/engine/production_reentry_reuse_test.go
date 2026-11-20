package engine

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// reuseRecipe is the recipe the fixture's image binds and current trust
// approves.
var reuseRecipe = domain.Digest("sha256:" + strings.Repeat("a", 64))

// reusedInvocation is the verification the predecessor's ready item holds.
const reusedInvocation = domain.InvocationID("verify-run-reentry-first")

// predecessorVerification is the verification the predecessor's ready item
// holds, which a cycle may reuse.
type predecessorVerification struct {
	head, base, evaluated string
	// reportHead, when set, is the head the report names in place of head,
	// which the ready item and its binding still name.
	reportHead string
	outcome    verify.Outcome
	findings   []verify.Finding
	// secondReport adds another verification report to the evidence.
	secondReport bool
}

// reentryReuseFixture is an external review cycle over a real head and base,
// whose predecessor ready item holds a verification of them.
type reentryReuseFixture struct {
	repo    upkeepRepo
	st      *store.Store
	blobs   *findingAdjudicationArtifactStore
	lane    *productionPublicationWorkflow
	task    productionPublicationTask
	binding productionBinding
	// evidence is the predecessor ready item's evidence snapshot.
	evidence []domain.Artifact
	base     string
	head     string
}

// newReentryReuseFixture records the predecessor's ready item with the
// verification edit makes of a clean one of the cycle's head and base.
func newReentryReuseFixture(t *testing.T, edit func(*predecessorVerification)) reentryReuseFixture {
	t.Helper()
	ctx := t.Context()
	repo := newUpkeepRepo(t)
	base := repo.commit(map[string]string{"README.md": "base\n"})
	head := repo.commit(map[string]string{"README.md": "base\nhead\n"})
	predecessor := predecessorVerification{head: head, base: base, outcome: verify.OutcomePassed}
	if edit != nil {
		edit(&predecessor)
	}

	successor := externalReviewSuccessorForTest()
	successor.Reentry.BaseSHA, successor.Reentry.HeadSHA = base, head
	task := newReentryTask("project-reentry", successor)
	runID := successor.RunID

	reportHead := predecessor.head
	if predecessor.reportHead != "" {
		reportHead = predecessor.reportHead
	}
	raw, err := json.MarshalIndent(verify.Report{
		HeadSHA: reportHead, BaseSHA: predecessor.base, EvaluatedSHA: predecessor.evaluated,
		RecipeDigest: reuseRecipe, Outcome: predecessor.outcome, Findings: predecessor.findings,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	transcript := []byte("$ make test\nok\n")
	approved := map[domain.Digest]bool{reuseRecipe: true}
	blobs := &findingAdjudicationArtifactStore{bodies: map[domain.Digest][]byte{}}
	type evidenceBody struct {
		kind    domain.ArtifactKind
		media   domain.EvidenceMediaType
		content []byte
	}
	bodies := []evidenceBody{
		{domain.ArtifactKindVerificationReport, domain.EvidenceMediaApplicationJSON, raw},
		{domain.ArtifactKindCommandTranscript, domain.EvidenceMediaTextPlain, transcript},
	}
	if predecessor.secondReport {
		bodies = append(bodies, evidenceBody{
			domain.ArtifactKindVerificationReport, domain.EvidenceMediaApplicationJSON, append(raw, '\n'),
		})
	}
	var evidence []domain.Artifact
	for _, body := range bodies {
		digest := domain.Digest(contentaddr.Sum(body.content))
		recipe := reuseRecipe
		artifact, err := domain.NewArtifact(domain.ArtifactInput{
			ID:     domain.ArtifactID(string(body.kind) + ":" + string(reusedInvocation) + ":" + string(digest)),
			Type:   body.kind,
			Digest: digest,
			Provenance: domain.Provenance{
				ProducerClass: domain.ProducerVerifier, ProducerInvocationID: reusedInvocation,
				HeadBinding: domain.HeadBound, SourceHeadSHA: predecessor.head,
				VerificationRecipeDigest: &recipe, SensitivityClass: domain.SensitivityNormal,
			},
			Metadata: domain.EvidenceMetadata{
				MediaType: body.media, SizeBytes: int64(len(body.content)),
				CreatedAt: time.Date(2026, 11, 20, 8, 30, 0, 0, time.UTC),
				Source:    domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
			},
		}, approved)
		if err != nil {
			t.Fatal(err)
		}
		blobs.bodies[digest] = body.content
		evidence = append(evidence, artifact)
	}

	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "driver", Value: "claude",
		Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: "sha256:reuse-test-policy"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: runID, ProjectID: task.ProjectID,
		SpecDigest: "sha256:reuse-test-spec", PolicyDigest: policy.Digest,
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: successor.PredecessorItemID, ProjectID: task.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "Published and completed production verification.",
		RequestedDecision: []domain.Action{domain.ActionOpenPR},
		EvidenceSnapshot:  evidence,
		PRHeadSHA:         predecessor.head,
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 7},
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{ApprovedRecipes: approved})
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		for _, artifact := range evidence {
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}

	profile := domain.Digest("sha256:profile")
	binding := productionBinding{
		run:   run,
		image: domain.ProjectImage{ID: "sha256:image", RecipeDigest: reuseRecipe},
		reentry: &reentryCycle{predecessor: domain.ReadyItemPRBinding{
			ItemID: successor.PredecessorItemID, RunID: runID, HeadSHA: predecessor.head,
		}},
	}
	binding.admission.Base.BaseSHA = base
	binding.admission.TrustProfileDigest = &profile
	return reentryReuseFixture{
		repo: repo, st: st, blobs: blobs,
		lane:    &productionPublicationWorkflow{store: st, artifacts: blobs, workDir: t.TempDir()},
		task:    task,
		binding: binding, evidence: evidence, base: base, head: head,
	}
}

func (f reentryReuseFixture) reuse(t *testing.T) (productionReentryCheckpoint, bool) {
	t.Helper()
	checkpoint, reused, err := f.lane.reuseReentryVerification(t.Context(), f.task, f.binding, f.repo.dir)
	if err != nil {
		t.Fatalf("reuseReentryVerification: %v", err)
	}
	return checkpoint, reused
}

func (f reentryReuseFixture) load(t *testing.T, key string) (productionReentryCheckpoint, bool, error) {
	t.Helper()
	return f.lane.loadReentryCheckpoint(t.Context(), key, f.task, f.binding)
}

// A cycle whose predecessor ready item holds a clean verification of the
// same head on the same base under the same recipe records a checkpoint
// that names that verification and runs none of its own: the lane has no
// room, verifier, or memory reservation to run one with. The tree and diff
// are this cycle's, resolved in its checkout, and a later pass reloads the
// checkpoint by reading the reused report.
func TestReentryReusesItsPredecessorsCleanVerification(t *testing.T) {
	t.Parallel()
	f := newReentryReuseFixture(t, nil)
	checkpoint, reused := f.reuse(t)
	if !reused {
		t.Fatal("a clean verification of the same input was not reused")
	}
	tree := f.repo.git("rev-parse", f.head+"^{tree}")
	if checkpoint.Version != productionReentryCheckpointVersion ||
		checkpoint.ReusedInvocationID != reusedInvocation ||
		checkpoint.TaskKey != f.task.intentKey() ||
		checkpoint.BaseSHA != f.base || checkpoint.HeadSHA != f.head || checkpoint.EvaluatedSHA != "" ||
		checkpoint.TreeSHA != tree || checkpoint.ProjectImage != f.binding.image.ID ||
		checkpoint.RecipeDigest != reuseRecipe || !checkpoint.clean() ||
		!reflect.DeepEqual(checkpoint.Artifacts, f.evidence) {
		t.Fatalf("reused checkpoint = %+v, want the predecessor's verification under this cycle's tree %s", checkpoint, tree)
	}
	if stats := checkpoint.DiffStats; stats == nil || stats.BaseSHA != f.base || stats.HeadSHA != f.head ||
		stats.FilesChanged != 1 || stats.Additions != 1 {
		t.Fatalf("reused diff stats = %+v, want this cycle's diff of the head", stats)
	}
	// It ran no verifier, so it records no verification of its own.
	if err := f.st.Read(t.Context(), func(tx *store.ReadTx) error {
		milestones, err := tx.ListRunMilestones(t.Context(), f.task.RunID)
		for _, m := range milestones {
			if m.Kind == domain.MilestoneVerificationStarted || m.Kind == domain.MilestoneVerificationPassed ||
				m.Kind == domain.MilestoneVerificationFailed {
				t.Errorf("reused cycle recorded %s, want no verification milestone", m.Kind)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	loaded, found, err := f.load(t, f.task.reentryCheckpointKey())
	if err != nil || !found || !reflect.DeepEqual(loaded, checkpoint) {
		t.Fatalf("loadReentryCheckpoint = %+v, %t, %v; want the reused checkpoint", loaded, found, err)
	}
}

// Every single difference from the predecessor's input, and every
// verification that was not clean, verifies again: nothing is recorded and
// the caller runs verification as before. The repair path is excluded in
// TestImageRepairExternalReviewVerifiesAgain, which needs a repair run.
func TestReentryVerifiesAgainUnlessThePredecessorVerifiedTheSameInput(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		predecessor func(*predecessorVerification)
		cycle       func(*reentryReuseFixture)
	}{
		"another head": {predecessor: func(p *predecessorVerification) {
			p.head = strings.Repeat("e", 40)
		}},
		"another base": {predecessor: func(p *predecessorVerification) {
			p.base = strings.Repeat("e", 40)
		}},
		"a report of another head": {predecessor: func(p *predecessorVerification) {
			p.reportHead = strings.Repeat("e", 40)
		}},
		"two reports": {predecessor: func(p *predecessorVerification) {
			p.secondReport = true
		}},
		"another recipe": {cycle: func(f *reentryReuseFixture) {
			f.binding.image.RecipeDigest = domain.Digest("sha256:" + strings.Repeat("f", 64))
		}},
		"findings": {predecessor: func(p *predecessorVerification) {
			p.findings = []verify.Finding{{Path: "README.md", Kind: verify.FindingVerificationControlPath}}
		}},
		"failed": {predecessor: func(p *predecessorVerification) {
			p.outcome = verify.OutcomeFailed
		}},
		"a merge the predecessor verified": {predecessor: func(p *predecessorVerification) {
			p.evaluated = strings.Repeat("e", 40)
		}},
		"a merge the cycle verifies": {cycle: func(f *reentryReuseFixture) {
			f.binding.evaluatedSHA = strings.Repeat("e", 40)
		}},
		// The verifier's control patterns come from the trust profile, which
		// only a pinned admission holds fixed across cycles.
		"an unpinned trust profile": {cycle: func(f *reentryReuseFixture) {
			f.binding.admission.TrustProfileDigest = nil
		}},
		// A hand-back's predecessor binding is the latest published ready
		// item, which need not be the cycle's own predecessor.
		"a hand-back": {cycle: func(f *reentryReuseFixture) {
			successor := *f.task.Successor
			successor.Origin = domain.PublicationSuccessorHandBack
			f.task.Successor = &successor
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newReentryReuseFixture(t, tc.predecessor)
			if tc.cycle != nil {
				tc.cycle(&f)
			}
			if checkpoint, reused := f.reuse(t); reused {
				t.Fatalf("reused %+v, want verification run again", checkpoint)
			}
			if _, found, err := f.load(t, f.task.reentryCheckpointKey()); err != nil || found {
				t.Fatalf("checkpoint found = %t, %v; want none recorded", found, err)
			}
		})
	}
}

// A predecessor blob that no longer matches its digest fails closed at
// reuse, rather than falling back to verifying again, and records nothing.
func TestReentryReuseFailsClosedOnAPredecessorBlobMismatch(t *testing.T) {
	t.Parallel()
	f := newReentryReuseFixture(t, nil)
	f.blobs.bodies[f.evidence[0].Digest] = []byte("{}\n")
	checkpoint, reused, err := f.lane.reuseReentryVerification(t.Context(), f.task, f.binding, f.repo.dir)
	if !errors.Is(err, domain.ErrParentKeyMismatch) || reused {
		t.Fatalf("reuseReentryVerification = %+v, %t, %v; want %v", checkpoint, reused, err, domain.ErrParentKeyMismatch)
	}
	if _, found, err := f.load(t, f.task.reentryCheckpointKey()); err != nil || found {
		t.Fatalf("checkpoint found = %t, %v; want none recorded", found, err)
	}
}

// A reused checkpoint is held to the reused report as a cycle's own is to
// its own: a blob that no longer matches, or a row whose head, base,
// outcome, findings, version, or reused invocation disagrees with the
// report, fails closed.
func TestReusedReentryCheckpointFailsClosedOnDisagreement(t *testing.T) {
	t.Parallel()
	// The row the reuse records, built by hand so each case can record a
	// variant of it.
	row := func(t *testing.T, f reentryReuseFixture) productionReentryCheckpoint {
		t.Helper()
		digest, err := domain.ComputeEvidenceSnapshotDigest(f.evidence)
		if err != nil {
			t.Fatal(err)
		}
		return productionReentryCheckpoint{
			Version: productionReentryCheckpointVersion, TaskKey: f.task.intentKey(),
			BaseSHA: f.base, HeadSHA: f.head, TreeSHA: f.repo.git("rev-parse", f.head+"^{tree}"),
			ProjectImage: f.binding.image.ID, Outcome: domain.VerificationPassed, RecipeDigest: reuseRecipe,
			EvidenceSnapshotDigest: digest, Artifacts: f.evidence, ReusedInvocationID: reusedInvocation,
		}
	}
	for name, tc := range map[string]struct {
		predecessor func(*predecessorVerification)
		row         func(*productionReentryCheckpoint)
		blobs       func(reentryReuseFixture)
		want        error
	}{
		"agrees": {},
		"a blob that no longer matches": {blobs: func(f reentryReuseFixture) {
			f.blobs.bodies[f.evidence[0].Digest] = []byte("{}\n")
		}, want: domain.ErrParentKeyMismatch},
		"a report of another head": {predecessor: func(p *predecessorVerification) {
			p.head = strings.Repeat("e", 40)
		}, want: domain.ErrParentKeyMismatch},
		"a report of another base": {predecessor: func(p *predecessorVerification) {
			p.base = strings.Repeat("e", 40)
		}, want: domain.ErrParentKeyMismatch},
		"an outcome the report does not give": {row: func(c *productionReentryCheckpoint) {
			c.Outcome = domain.VerificationFailed
		}, want: domain.ErrParentKeyMismatch},
		"findings the report does not give": {row: func(c *productionReentryCheckpoint) {
			c.Findings = verificationCandidateFindings([]verify.Finding{{Path: "README.md", Kind: verify.FindingVerificationControlPath}})
		}, want: domain.ErrParentKeyMismatch},
		"a reused invocation that wrote no report": {row: func(c *productionReentryCheckpoint) {
			c.ReusedInvocationID = "verify-run-reentry-other"
		}, want: domain.ErrParentKeyMismatch},
		"a pre-reuse version": {row: func(c *productionReentryCheckpoint) {
			c.Version = productionReentryCheckpointVersionV2
		}, want: domain.ErrParentKeyMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newReentryReuseFixture(t, tc.predecessor)
			checkpoint := row(t, f)
			if tc.row != nil {
				tc.row(&checkpoint)
			}
			if tc.blobs != nil {
				tc.blobs(f)
			}
			payload, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			key := f.task.reentryCheckpointKey()
			if err := f.st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
				_, _, err := tx.RecordInbox(t.Context(), key, productionReentryCheckpointKind, payload)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			_, found, err := f.load(t, key)
			if tc.want == nil {
				if err != nil || !found {
					t.Fatalf("loadReentryCheckpoint = %t, %v; want the reused checkpoint", found, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("loadReentryCheckpoint = %v, want %v", err, tc.want)
			}
		})
	}
}
