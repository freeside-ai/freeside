package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	externalReviewerID    = int64(41)
	externalReviewerLogin = "codex[bot]"
)

// externalFindingOn builds an external finding left on a published head by
// the fixture reviewer.
func externalFindingOn(t *testing.T, runID domain.RunID, head string) domain.Finding {
	t.Helper()
	finding, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: runID, Forge: domain.ExternalReviewForgeGitHub,
		ReviewerAccountID: externalReviewerID, ReviewerLogin: externalReviewerLogin,
		ThreadID: "PRRT_kwDOexample", HeadSHA: head,
		Severity: domain.FindingSeverityP1,
		Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 42, EndLine: 42},
		Message:  "unchecked error", RawText: "P1: the error return is dropped",
		CreatedAt: reentryAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return finding
}

func putExternalFinding(t *testing.T, st *Store, finding domain.Finding) {
	t.Helper()
	ctx := context.Background()
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutExternalFinding(ctx, finding) }); err != nil {
		t.Fatal(err)
	}
}

func reviewerEntry(accountID int64, login string) domain.ExternalReviewer {
	return domain.ExternalReviewer{
		Forge: domain.ExternalReviewForgeGitHub, AccountID: accountID, Login: login,
		Authority: domain.ExternalReviewDriveRound,
	}
}

// activateProfile records a profile for a repository and makes it the active
// one. The review config digest varies with the allowlist only through the
// content address, so two calls with different reviewers are two revisions.
func activateProfile(
	t *testing.T, st *Store, repo string, repositoryID int64, at time.Time, reviewers ...domain.ExternalReviewer,
) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: repo, RepositoryID: repositoryID,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review:                     domain.ReviewSettings{Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config"},
		ExternalReviewers:          reviewers,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.RecordInactiveTrustProfile(ctx, profile, at); err != nil {
			return err
		}
		return tx.ActivateTrustProfile(ctx, repo, profile.ProfileDigest, at)
	}); err != nil {
		t.Fatal(err)
	}
	return profile
}

func admittedRead(st *Store, id domain.FindingID) (domain.Finding, error) {
	ctx := context.Background()
	var finding domain.Finding
	err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		finding, err = tx.GetAdmittedExternalFinding(ctx, id)
		return err
	})
	return finding, err
}

// TestExternalFindingWriteDoor: an external finding has one write door, a
// replay through it converges, and storing one needs no trust profile at all,
// because storing grants nothing.
func TestExternalFindingWriteDoor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedReadyItemBinding(t, "feat/meaningful-task", reentryBase1, reentryHead1)
	external := externalFindingOn(t, f.run.ID, reentryHead1)

	if err := f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutFinding(ctx, external) }); !errors.Is(err, domain.ErrExternalFindingQuarantined) {
		t.Fatalf("PutFinding of an external finding: %v, want ErrExternalFindingQuarantined", err)
	}
	putExternalFinding(t, f.st, external)
	putExternalFinding(t, f.st, external) // replayed ingest

	// An edited comment is a new finding, not a conflict with the stored one.
	edited := externalFindingOn(t, f.run.ID, reentryHead1)
	edited, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: f.run.ID, Forge: domain.ExternalReviewForgeGitHub,
		ReviewerAccountID: externalReviewerID, ReviewerLogin: externalReviewerLogin,
		ThreadID: edited.External.ThreadID, HeadSHA: reentryHead1,
		Message: "unchecked error", RawText: "P1: the error return is dropped (edited)", CreatedAt: reentryAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	putExternalFinding(t, f.st, edited)

	// History reads need no admission: the repository has no profile yet.
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetFinding(ctx, external.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, external) {
			t.Fatalf("stored external finding = %+v, want %+v", got, external)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	native := domain.Finding{
		ID: "find-native", RunID: f.run.ID, Source: "codex_github", Message: "m", RawText: "r", CreatedAt: reentryAt,
	}
	if err := f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutExternalFinding(ctx, native) }); !errors.Is(err, domain.ErrExternalFindingInconsistent) {
		t.Fatalf("PutExternalFinding of a native finding: %v, want ErrExternalFindingInconsistent", err)
	}
}

// TestAdmittedExternalFindingRegatesActiveProfile: nothing stored says a
// finding is admitted, so the one read that grants authority re-runs the
// allowlist check against the repository's active profile every time and
// fails closed on each way the identity can differ.
func TestAdmittedExternalFindingRegatesActiveProfile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedReadyItemBinding(t, "feat/meaningful-task", reentryBase1, reentryHead1)
	repo, repositoryID := f.binding.Repo, f.binding.RepositoryID
	external := externalFindingOn(t, f.run.ID, reentryHead1)
	putExternalFinding(t, f.st, external)

	requireRefused := func(t *testing.T, when string) {
		t.Helper()
		if _, err := admittedRead(f.st, external.ID); !errors.Is(err, domain.ErrExternalReviewNotAdmitted) {
			t.Fatalf("admitted read %s: %v, want ErrExternalReviewNotAdmitted", when, err)
		}
		// The history read is unaffected by every allowlist state.
		if err := f.st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetFinding(ctx, external.ID)
			return err
		}); err != nil {
			t.Fatalf("history read %s: %v", when, err)
		}
	}

	requireRefused(t, "with no active profile")
	at := reentryAt
	next := func() time.Time { at = at.Add(time.Minute); return at }

	activateProfile(t, f.st, repo, repositoryID, next())
	requireRefused(t, "under a profile with no allowlist")

	activateProfile(t, f.st, repo, repositoryID, next(), reviewerEntry(900, "maintainer"))
	requireRefused(t, "under a profile listing someone else")

	activateProfile(t, f.st, repo, repositoryID, next(), reviewerEntry(externalReviewerID, "codex-renamed[bot]"))
	requireRefused(t, "after the listed account was renamed")

	activateProfile(t, f.st, repo, repositoryID, next(), reviewerEntry(externalReviewerID+1, externalReviewerLogin))
	requireRefused(t, "when another account holds the login")

	// A profile for an earlier repository of the same name admits nobody
	// here: the binding's immutable repository ID has to match.
	activateProfile(t, f.st, repo, repositoryID+1, next(), reviewerEntry(externalReviewerID, externalReviewerLogin))
	requireRefused(t, "under a profile for another repository ID")

	admitting := activateProfile(t, f.st, repo, repositoryID, next(), reviewerEntry(externalReviewerID, externalReviewerLogin))
	got, err := admittedRead(f.st, external.ID)
	if err != nil {
		t.Fatalf("admitted read under a listing profile: %v", err)
	}
	if !reflect.DeepEqual(got, external) {
		t.Fatalf("admitted finding = %+v, want %+v", got, external)
	}

	// Removing the reviewer withdraws the authority for the same stored row.
	activateProfile(t, f.st, repo, repositoryID, next())
	requireRefused(t, "after the owner removed the reviewer")

	// Re-activating the admitting revision restores it: the answer follows
	// the active profile, not anything written at ingest.
	if err := f.st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.ActivateTrustProfile(ctx, repo, admitting.ProfileDigest, next())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := admittedRead(f.st, external.ID); err != nil {
		t.Fatalf("admitted read after re-activation: %v", err)
	}

	// A finding from a Freeside review is never an admitted external one.
	native := domain.Finding{
		ID: "find-native", RunID: f.run.ID, Source: "codex_github", Message: "m", RawText: "r", CreatedAt: reentryAt,
	}
	if err := f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutFinding(ctx, native) }); err != nil {
		t.Fatal(err)
	}
	if _, err := admittedRead(f.st, native.ID); !errors.Is(err, domain.ErrExternalFindingInconsistent) {
		t.Fatalf("admitted read of a native finding: %v, want ErrExternalFindingInconsistent", err)
	}
	if _, err := admittedRead(f.st, "external-unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("admitted read of an unknown finding: %v, want ErrNotFound", err)
	}
}

// TestAdmittedExternalFindingNeedsAPublishedBinding: the repository an
// external finding is checked against comes from the run's authenticated
// ready binding, so a finding on a run that published nothing is refused even
// under a profile that lists its reviewer.
func TestAdmittedExternalFindingNeedsAPublishedBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedReadyItemBinding(t, "feat/meaningful-task", reentryBase1, reentryHead1)
	activateProfile(t, f.st, f.binding.Repo, f.binding.RepositoryID, reentryAt,
		reviewerEntry(externalReviewerID, externalReviewerLogin))
	unpublished := domain.Run{ID: "run-unpublished", ProjectID: f.run.ProjectID, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	if err := f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutRun(ctx, unpublished) }); err != nil {
		t.Fatal(err)
	}
	stray := externalFindingOn(t, unpublished.ID, reentryHead1)
	putExternalFinding(t, f.st, stray)
	if _, err := admittedRead(f.st, stray.ID); err == nil {
		t.Fatal("admitted an external finding on a run with no published binding")
	}
}

// TestExternalFindingQuarantine pins plan §5.19's quarantine at the store. It
// protects the §7 review requirement (engine assertReviewedCandidate) and
// round completeness (engine reviewRoundDispositionComplete), which read only
// review records and ReviewRecord.FindingIDs: as long as no external finding
// can become part of a review record, a shadow review record, or a native
// review observation, none can satisfy the review requirement or ReviewSource
// freshness, independence, or review-completeness.
func TestExternalFindingQuarantine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedReadyItemBinding(t, "feat/meaningful-task", reentryBase1, reentryHead1)
	// Admission changes nothing here: even an admitted reviewer's finding is
	// quarantined.
	activateProfile(t, f.st, f.binding.Repo, f.binding.RepositoryID, reentryAt,
		reviewerEntry(externalReviewerID, externalReviewerLogin))
	external := externalFindingOn(t, f.run.ID, reentryHead1)
	// A copy relabelled as a Freeside review's finding, for the relink cases.
	relabelled := external
	relabelled.External, relabelled.Source = nil, "codex_github"

	// The shadow pass needs the routed round it shadows, so round 1 is a
	// clean routed review and the refused routed record is round 2.
	clean := internalRoutedCandidate(t, f.run.ID, "review-quarantine-1", 1, nil, reentryAt)
	if err := f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutReviewRecord(ctx, clean, nil) }); err != nil {
		t.Fatal(err)
	}
	routed := internalRoutedCandidate(t, f.run.ID, "review-quarantine-2", 2,
		[]domain.FindingID{external.ID}, reentryAt.Add(time.Minute))
	shadow := internalShadowRecord(t, f.run.ID, external.ID)
	shadow.CompletedAt = reentryAt
	shadowRelabelled := relabelled
	shadowRelabelled.Source = string(shadow.Source)

	for _, stored := range []bool{false, true} {
		name := "before the external finding is stored"
		if stored {
			name = "after the external finding is stored"
			putExternalFinding(t, f.st, external)
		}
		t.Run(name, func(t *testing.T) {
			if err := f.st.Write(ctx, func(tx *WriteTx) error {
				return tx.PutReviewRecord(ctx, routed, []domain.Finding{external})
			}); !errors.Is(err, domain.ErrExternalFindingQuarantined) {
				t.Fatalf("review record with an external finding: %v, want ErrExternalFindingQuarantined", err)
			}
			if err := f.st.Write(ctx, func(tx *WriteTx) error {
				return tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{external})
			}); !errors.Is(err, domain.ErrExternalFindingQuarantined) {
				t.Fatalf("shadow review record with an external finding: %v, want ErrExternalFindingQuarantined", err)
			}
			if !stored {
				return
			}
			// A second write cannot relink the stored ID under a Freeside
			// label: the stored bytes are immutable.
			if err := f.st.Write(ctx, func(tx *WriteTx) error {
				return tx.PutReviewRecord(ctx, routed, []domain.Finding{relabelled})
			}); !errors.Is(err, ErrImmutableConflict) {
				t.Fatalf("review record relinking a stored external finding: %v, want ErrImmutableConflict", err)
			}
			if err := f.st.Write(ctx, func(tx *WriteTx) error {
				return tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{shadowRelabelled})
			}); !errors.Is(err, ErrImmutableConflict) {
				t.Fatalf("shadow review record relinking a stored external finding: %v, want ErrImmutableConflict", err)
			}
		})
	}
	var linked int
	if err := f.st.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM review_record_findings WHERE finding_id = ?1) +
		(SELECT COUNT(*) FROM shadow_review_record_findings WHERE finding_id = ?1)`, external.ID).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 0 {
		t.Fatalf("external finding is linked to %d review records", linked)
	}

	observation := domain.NativeReviewObservation{
		Repo: f.binding.Repo, RepositoryID: f.binding.RepositoryID, PRNumber: f.binding.PRNumber,
		Provider: domain.NativeReviewCodexGitHub, Kind: domain.NativeReviewFindings,
		NativeID: 900100, AuthorLogin: externalReviewerLogin,
		ReviewCommitSHA: reentryHead1, ReviewState: "COMMENTED", BindingHeadSHA: reentryHead1,
		SubmittedAt: reentryAt, ObservedAt: reentryAt,
		Findings: []domain.Finding{external},
	}
	if err := f.st.WriteInternal(ctx, func(tx *InternalTx) error {
		_, err := tx.AppendNativeReviewObservation(ctx, observation)
		return err
	}); !errors.Is(err, domain.ErrExternalFindingQuarantined) {
		t.Fatalf("native review observation with an external finding: %v, want ErrExternalFindingQuarantined", err)
	}
}

// The domain finding golden as it stood before the external block existed,
// compacted: the body a build without the field stored for that finding.
const preExternalFindingBody = `{"id":"find-1","run_id":"run-1","source":"codex_github","severity":"P2","location":{"path":"daemon/main.go","start_line":42,"end_line":42},"message":"unchecked error","raw_text":"err not handled","created_at":"2026-01-02T03:04:05Z"}`

// TestPreExternalFindingRowConverges: Finding gained an optional block, and
// PutFinding converges a replay only on byte-identical bodies, so a finding
// stored before the block existed must read back and re-encode to the same
// bytes.
func TestPreExternalFindingRowConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedReadyItemBinding(t, "feat/meaningful-task", reentryBase1, reentryHead1)
	run := domain.Run{ID: "run-1", ProjectID: f.run.ProjectID, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	if err := f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.db.ExecContext(ctx,
		`INSERT INTO findings (id, run_id, entity_version, as_of_revision, body) VALUES (?, ?, 1, 1, ?)`,
		"find-1", "run-1", preExternalFindingBody); err != nil {
		t.Fatalf("insert pre-change finding row: %v", err)
	}
	var stored domain.Finding
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		var err error
		stored, err = tx.GetFinding(ctx, "find-1")
		return err
	}); err != nil {
		t.Fatalf("pre-change finding read: %v", err)
	}
	if stored.External != nil {
		t.Fatalf("pre-change finding carries external provenance %+v", stored.External)
	}
	if err := f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutFinding(ctx, stored) }); err != nil {
		t.Fatalf("replayed write of a pre-change finding: %v", err)
	}
}
