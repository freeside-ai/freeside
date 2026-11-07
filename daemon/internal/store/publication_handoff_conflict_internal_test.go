package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var handoffConflictAt = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// handoffConflictFixture is a never-published root publication reviewed clean
// at base-1, whose handoff sealed the tip base-2, and the handoff_conflict
// authority that state proves.
type handoffConflictFixture struct {
	st        *Store
	check     domain.HandoffCheck
	proof     domain.CheckProof
	authority domain.PublicationSuccessor
}

func seedHandoffConflict(t *testing.T, mutate func(*domain.HandoffCheck)) handoffConflictFixture {
	t.Helper()
	st, check, proof := handoffCheckFixture(t)
	check.PublicationID = domain.ProductionPublicationInvocationID(check.RunID)
	if mutate != nil {
		tip := check.TipSHA
		mutate(&check)
		if check.TipSHA != tip {
			// The seal binds the tip to an audit that observed it, so a
			// mutated tip needs its own audit to reach the authority's gate.
			check.WorkflowAuditID = recordHandoffAudit(t, st, "o/r", "main", check.TipSHA)
		}
	}
	prior := putReentryReview(t, st, check.RunID, 1, "base-1", check.HeadSHA)
	if err := st.WriteInternal(context.Background(), func(tx *InternalTx) error {
		return tx.SealHandoffTip(context.Background(), check, handoffConflictAt)
	}); err != nil {
		t.Fatal(err)
	}
	return handoffConflictFixture{st: st, check: check, proof: proof, authority: domain.PublicationSuccessor{
		Version: domain.PublicationHandoffConflictVersion, Origin: domain.PublicationSuccessorHandoffConflict,
		RunID: check.RunID, PredecessorItemID: domain.ProductionReadyItemID(check.RunID),
		PriorReviewInvocationID: prior.InvocationID, ReviewRound: prior.Round + 1,
		Reentry:              &domain.PublicationSuccessorReentry{BaseSHA: check.TipSHA, HeadSHA: check.HeadSHA},
		HandoffConflictCause: domain.HandoffConflictTextual, FirstPublication: true,
	}}
}

func (f handoffConflictFixture) record(authority domain.PublicationSuccessor) error {
	ctx := context.Background()
	return f.st.Write(ctx, func(tx *WriteTx) error { return tx.RecordPublicationSuccessor(ctx, authority) })
}

// TestHandoffConflictAuthoritySeals: the authority seals against the sealed
// handoff check, converges on a replay, and reads back re-gated.
func TestHandoffConflictAuthoritySeals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, cause := range domain.AllHandoffConflictCauses {
		f := seedHandoffConflict(t, nil)
		authority := f.authority
		authority.HandoffConflictCause = cause
		if err := f.record(authority); err != nil {
			t.Fatalf("%s: %v", cause, err)
		}
		if err := f.record(authority); err != nil {
			t.Fatalf("%s replay: %v", cause, err)
		}
		if err := f.st.Read(ctx, func(tx *ReadTx) error {
			got, err := tx.GetPublicationSuccessor(ctx, authority.RunID, authority.PublicationID())
			if err == nil && got.HandoffConflictCause != cause {
				t.Fatalf("read back %#v", got)
			}
			return err
		}); err != nil {
			t.Fatalf("%s read: %v", cause, err)
		}
	}
}

// TestHandoffConflictAuthorityRefusals: every coordinate is re-derived from
// the sealed check and the prior review, never taken from the record.
func TestHandoffConflictAuthorityRefusals(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*domain.PublicationSuccessor){
		"another tip":          func(s *domain.PublicationSuccessor) { s.Reentry.BaseSHA = "base-3" },
		"another head":         func(s *domain.PublicationSuccessor) { s.Reentry.HeadSHA = "head-2" },
		"a later round":        func(s *domain.PublicationSuccessor) { s.ReviewRound++ },
		"an unknown review":    func(s *domain.PublicationSuccessor) { s.PriorReviewInvocationID = "review-other" },
		"a pull request":       func(s *domain.PublicationSuccessor) { s.FirstPublication = false },
		"an unknown publisher": func(s *domain.PublicationSuccessor) { s.PredecessorItemID = "production-ready-reentry-other" },
	} {
		f := seedHandoffConflict(t, nil)
		authority := f.authority
		reentry := *authority.Reentry
		authority.Reentry = &reentry
		mutate(&authority)
		if err := f.record(authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("%s: error = %v, want ErrParentKeyMismatch", name, err)
		}
	}
	for name, mutate := range map[string]func(*domain.HandoffCheck){
		// The review covered base-1; a check on another evidence base is
		// not the conflict that review's head met.
		"a stale review": func(c *domain.HandoffCheck) { c.EvidenceBaseSHA = "base-0" },
		// A tip that never moved holds no conflict to remediate.
		"an unmoved tip": func(c *domain.HandoffCheck) { c.TipSHA = c.EvidenceBaseSHA },
	} {
		f := seedHandoffConflict(t, mutate)
		if err := f.record(f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("%s: error = %v, want ErrParentKeyMismatch", name, err)
		}
	}
	none := seedHandoffConflict(t, nil)
	ctx := context.Background()
	if _, err := none.st.db.ExecContext(ctx, `DELETE FROM publication_handoff_checks`); err != nil {
		t.Fatal(err)
	}
	if err := none.record(none.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Errorf("no handoff check: error = %v, want ErrParentKeyMismatch", err)
	}
}

// TestHandoffConflictAuthorityRequiresTheTipAudit: the authority's base is
// the sealed tip, so the gate re-reads the audit the check names, as the
// publication decision does (#2704). A check sealed before migration 0129
// names none, and one whose audit observed another tip or ref is not proof
// the tip was observed on the run's target; neither admits a remediation.
func TestHandoffConflictAuthorityRequiresTheTipAudit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, audit := range map[string]func(*testing.T, *Store) any{
		"a pre-0129 check": func(*testing.T, *Store) any { return nil },
		"an audit of another tip": func(t *testing.T, st *Store) any {
			return recordHandoffAudit(t, st, "o/r", "main", "base-9")
		},
		"an audit of another ref": func(t *testing.T, st *Store) any {
			return recordHandoffAudit(t, st, "o/r", "release", "base-2")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := seedHandoffConflict(t, nil)
			if _, err := f.st.db.ExecContext(ctx,
				`UPDATE publication_handoff_checks SET workflow_audit_id = ?`, audit(t, f.st)); err != nil {
				t.Fatal(err)
			}
			if err := f.record(f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

// TestHandoffConflictAuthorityRefusesAProvedMerge: a merge the handoff
// verified is a passed check, not a conflict.
func TestHandoffConflictAuthorityRefusesAProvedMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedHandoffConflict(t, nil)
	if err := f.st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.ProveHandoffMerge(ctx, f.check.RunID, f.check.PublicationID,
			domain.HandoffMerge{MergeSHA: "merge-1", VerificationProofDigest: f.proof.Digest}, handoffConflictAt)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.record(f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
	}
}

// TestHandoffConflictAuthorityFollowsItsParent: a conflict on a successor's
// handoff updates the published pull request, and a head_changed parent's
// head is someone else's, which no remediation may rewrite.
func TestHandoffConflictAuthorityFollowsItsParent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		reason domain.ReadinessInvalidationReason
		want   error
	}{
		{domain.ReadinessInvalidationBaseAdvanced, nil},
		{domain.ReadinessInvalidationHeadChanged, domain.ErrParentKeyMismatch},
	} {
		t.Run(string(test.reason), func(t *testing.T) {
			f := seedReentry(t, test.reason, reentryOptions{})
			if err := f.record(t, f.authority); err != nil {
				t.Fatal(err)
			}
			parent := f.authority.Reentry
			check := domain.HandoffCheck{
				RunID: f.run.ID, PublicationID: f.authority.PublicationID(),
				HeadSHA: parent.HeadSHA, EvidenceBaseSHA: parent.BaseSHA, TipSHA: reentryBase3,
				WorkflowAuditID: recordHandoffAudit(t, f.st, f.admission.Base.Repo, f.admission.Base.BaseRef, reentryBase3),
			}
			if err := f.st.WriteInternal(context.Background(), func(tx *InternalTx) error {
				return tx.SealHandoffTip(context.Background(), check, handoffConflictAt)
			}); err != nil {
				t.Fatal(err)
			}
			prior := putReentryReview(t, f.st, f.run.ID, 2, parent.BaseSHA, parent.HeadSHA)
			authority := domain.PublicationSuccessor{
				Version: domain.PublicationHandoffConflictVersion, Origin: domain.PublicationSuccessorHandoffConflict,
				RunID: f.run.ID, PredecessorItemID: f.authority.ReadyItemID(),
				PriorReviewInvocationID: prior.InvocationID, ReviewRound: 3,
				Reentry:              &domain.PublicationSuccessorReentry{BaseSHA: reentryBase3, HeadSHA: parent.HeadSHA},
				HandoffConflictCause: domain.HandoffConflictMergeVerificationFailed,
			}
			// The parent's run published, so the authority updates its pull
			// request; claiming a first publication is refused.
			first := authority
			first.FirstPublication = true
			if err := f.record(t, first); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("first publication for a published run: error = %v, want ErrParentKeyMismatch", err)
			}
			if err := f.record(t, authority); !errors.Is(err, test.want) || (test.want == nil && err != nil) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}
