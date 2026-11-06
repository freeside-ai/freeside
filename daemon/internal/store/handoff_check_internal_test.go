package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindHandoffChecks returns a store at head to the shape a daemon at 0125
// holds: no handoff checks.
func rewindHandoffChecks(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindDiscoveredModelCatalogs(t, ctx, db)
	for _, statement := range []string{
		`DROP TABLE publication_handoff_checks`,
		`DELETE FROM schema_migrations WHERE name = '0126_publication_handoff_checks.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

// TestHandoffChecksMigration: a daemon at 0125 migrates to a store that
// seals handoff tips.
func TestHandoffChecksMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, check, _ := handoffCheckFixture(t)
	rewindHandoffChecks(t, ctx, st.db)
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)
	// The rewind dropped the fixture audit's ref column, so the tip seals
	// from an audit recorded on the migrated store.
	check.WorkflowAuditID = recordHandoffAudit(t, st, "o/r", "main", check.TipSHA)
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.SealHandoffTip(ctx, check, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	}); err != nil {
		t.Fatal(err)
	}
}

// handoffCheckFixture opens a store holding one run admitted on o/r at
// base-1, a recorded audit of its main that observed base-2, which the
// returned check names, and a recorded proof that the merge of head-1 into
// base-2 passed verification.
func handoffCheckFixture(t *testing.T) (*Store, domain.HandoffCheck, domain.CheckProof) {
	t.Helper()
	ctx := context.Background()
	resolution, err := domain.NewRequirementResolution(domain.RequirementResolutionInput{
		RequirementKey: "clean-verification", CheckClass: domain.CheckClassCleanVerification,
		Kind: domain.RequirementRequired, Applicable: true, BaseDependent: true,
		RequirementSetDigest:    "sha256:requirements",
		FloorRegistryGeneration: domain.CurrentVerificationFloorRegistryGeneration,
		ResolvedPolicyDigest:    "sha256:policy",
	})
	if err != nil {
		t.Fatal(err)
	}
	check := domain.HandoffCheck{
		RunID: "run-handoff", PublicationID: "publish-run-handoff", HeadSHA: "head-1",
		EvidenceBaseSHA: "base-1", TipSHA: "base-2",
	}
	proof, err := domain.NewProspectiveMergeCheckProof(resolution, domain.ProspectiveMergeIdentity{
		BaseSHA: check.TipSHA, HeadSHA: check.HeadSHA, MergeSHA: "merge-1",
	}, domain.BaseRevision{Repo: "o/r", RepositoryID: 1, BaseRef: "main", BaseSHA: check.TipSHA}, "sha256:recipe")
	if err != nil {
		t.Fatal(err)
	}
	st := openTemplateStoreAt(t, t.TempDir()+"/handoff.db", Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedRecipes: map[domain.Digest]bool{"sha256:recipe": true},
		TrustedRequirementSets: map[domain.Digest][]domain.RequirementDefinition{
			"sha256:requirements": {{
				Key: "clean-verification", Class: domain.CheckClassCleanVerification,
				Kind: domain.RequirementRequired, Applicable: true, BaseDependent: true,
			}},
		},
	})
	t.Cleanup(func() { _ = st.Close() })
	policy, err := domain.NewResolvedPolicy(check.RunID, []domain.PolicyKey{{
		Key: "driver", Value: "claude", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	stageID, attemptID := domain.StageID("stage-handoff"), domain.AttemptID("attempt-handoff")
	invocationID := domain.InvocationID("inv-handoff")
	run := domain.Run{
		ID: check.RunID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		Stages: []domain.Stage{{
			ID: stageID, RunID: check.RunID, Name: "implementation",
			Attempts: []domain.Attempt{{ID: attemptID, StageID: stageID, Number: 1, InvocationID: invocationID}},
		}},
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: check.RunID, StageID: stageID, AttemptID: attemptID,
		Backend: "handoff-test", Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:input",
		Base:      domain.BaseRevision{Repo: "o/r", RepositoryID: 1, BaseRef: "main", BaseSHA: check.EvidenceBaseSHA},
		Workspace: "workspace", AdmittedAt: time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		if err := tx.RecordRequirementResolution(ctx, resolution); err != nil {
			return err
		}
		return tx.RecordCheckProof(ctx, proof)
	}); err != nil {
		t.Fatal(err)
	}
	check.WorkflowAuditID = recordHandoffAudit(t, st, "o/r", "main", check.TipSHA)
	return st, check, proof
}

// recordHandoffAudit records an audit of repo that resolved ref to tip and
// returns its row id.
func recordHandoffAudit(t *testing.T, st *Store, repo, ref, tip string) int64 {
	t.Helper()
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"` + repo + `","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := st.WriteInternal(context.Background(), func(tx *InternalTx) error {
		rec, err := tx.RecordWorkflowAudit(context.Background(), domain.WorkflowAudit{
			Repo: repo, AuditedCommitSHA: tip, AuditedRef: ref,
			AuditedAt:           time.Date(2026, 11, 5, 11, 30, 0, 0, time.UTC),
			WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
			EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
		})
		id = rec.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestHandoffTipSealsOnce pins that a publication's tip is resolved once: a
// replay converges, and another tip or audit is refused rather than
// re-resolved. A tip seals only from a recorded audit that resolved the
// run's admitted repository and ref to that tip (#2073).
func TestHandoffTipSealsOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, check, _ := handoffCheckFixture(t)
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	seal := func(c domain.HandoffCheck) error {
		return st.WriteInternal(ctx, func(tx *InternalTx) error { return tx.SealHandoffTip(ctx, c, at) })
	}
	for name, auditID := range map[string]int64{
		"no audit":                0,
		"unrecorded audit":        check.WorkflowAuditID + 100,
		"audit of another repo":   recordHandoffAudit(t, st, "o/other", "main", check.TipSHA),
		"audit of another commit": recordHandoffAudit(t, st, "o/r", "main", "base-3"),
		"audit of another ref":    recordHandoffAudit(t, st, "o/r", "release", check.TipSHA),
		"audit that names no ref": recordHandoffAudit(t, st, "o/r", "", check.TipSHA),
	} {
		unbound := check
		unbound.WorkflowAuditID = auditID
		if err := seal(unbound); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("sealing with %s error = %v, want ErrParentKeyMismatch", name, err)
		}
	}
	if err := seal(check); err != nil {
		t.Fatal(err)
	}
	if err := seal(check); err != nil {
		t.Fatalf("re-sealing the same tip: %v", err)
	}
	advanced := check
	advanced.TipSHA = "base-3"
	advanced.WorkflowAuditID = recordHandoffAudit(t, st, "o/r", "main", advanced.TipSHA)
	if err := seal(advanced); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("sealing another tip error = %v, want ErrImmutableConflict", err)
	}
	reaudited := check
	reaudited.WorkflowAuditID = recordHandoffAudit(t, st, "o/r", "main", check.TipSHA)
	if err := seal(reaudited); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("sealing from another audit error = %v, want ErrImmutableConflict", err)
	}
	withMerge := check
	withMerge.Merge = &domain.HandoffMerge{MergeSHA: "merge-1", VerificationProofDigest: "sha256:proof"}
	if err := seal(withMerge); !errors.Is(err, domain.ErrHandoffCheckInconsistent) {
		t.Fatalf("sealing with a merge error = %v, want ErrHandoffCheckInconsistent", err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetHandoffCheck(ctx, check.RunID, check.PublicationID)
		if err != nil {
			return err
		}
		if got != check {
			t.Fatalf("sealed check = %+v, want %+v", got, check)
		}
		_, err = tx.GetHandoffCheck(ctx, check.RunID, "publish-other")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("unsealed publication error = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestHandoffMergeProvesOnce pins that a moved tip's merge is recorded once,
// only with a recorded proof of exactly that head, tip, and merge.
func TestHandoffMergeProvesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, check, proof := handoffCheckFixture(t)
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	prove := func(merge domain.HandoffMerge) error {
		return st.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.ProveHandoffMerge(ctx, check.RunID, check.PublicationID, merge, at)
		})
	}
	merge := domain.HandoffMerge{MergeSHA: "merge-1", VerificationProofDigest: proof.Digest}
	if err := prove(merge); !errors.Is(err, ErrNotFound) {
		t.Fatalf("proving an unsealed handoff error = %v, want ErrNotFound", err)
	}
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.SealHandoffTip(ctx, check, at)
	}); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		merge   domain.HandoffMerge
		wantErr error
	}{
		"another merge":    {domain.HandoffMerge{MergeSHA: "merge-2", VerificationProofDigest: proof.Digest}, domain.ErrParentKeyMismatch},
		"unrecorded proof": {domain.HandoffMerge{MergeSHA: "merge-1", VerificationProofDigest: "sha256:missing"}, ErrNotFound},
	} {
		if err := prove(test.merge); !errors.Is(err, test.wantErr) {
			t.Fatalf("%s error = %v, want %v", name, err, test.wantErr)
		}
	}
	if err := prove(merge); err != nil {
		t.Fatal(err)
	}
	if err := prove(merge); err != nil {
		t.Fatalf("re-proving the same merge: %v", err)
	}
	if err := prove(domain.HandoffMerge{MergeSHA: "merge-2", VerificationProofDigest: proof.Digest}); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("proving another merge error = %v, want ErrImmutableConflict", err)
	}
	// The tip stays sealed once the merge is recorded.
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.SealHandoffTip(ctx, check, at)
	}); err != nil {
		t.Fatalf("re-sealing a proved handoff: %v", err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetHandoffCheck(ctx, check.RunID, check.PublicationID)
		if err != nil {
			return err
		}
		if got.Merge == nil || *got.Merge != merge || !got.Proved() {
			t.Fatalf("proved check = %+v, want merge %+v", got, merge)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestHandoffCheckUnchangedTipTakesNoMerge pins that an unchanged tip is
// complete on its seal and refuses a merge.
func TestHandoffCheckUnchangedTipTakesNoMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, check, proof := handoffCheckFixture(t)
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	check.TipSHA = check.EvidenceBaseSHA
	check.WorkflowAuditID = recordHandoffAudit(t, st, "o/r", "main", check.TipSHA)
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.SealHandoffTip(ctx, check, at); err != nil {
			return err
		}
		err := tx.ProveHandoffMerge(ctx, check.RunID, check.PublicationID,
			domain.HandoffMerge{MergeSHA: "merge-1", VerificationProofDigest: proof.Digest}, at)
		if !errors.Is(err, domain.ErrHandoffCheckInconsistent) {
			t.Fatalf("merge on an unchanged tip error = %v, want ErrHandoffCheckInconsistent", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
