package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestFindingDispositionMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0037_")
	if got := rawVersion(t, db); got != 36 {
		t.Fatalf("prior schema version = %d, want 36", got)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if got := rawVersion(t, db); got != 57 {
		t.Fatalf("schema version = %d, want 57", got)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM finding_dispositions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("new disposition table contains %d rows", count)
	}
}

func seedFindingDisposition(t *testing.T) (*Store, domain.ReviewDispositionRecord, domain.Finding) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/store.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run := domain.Run{ID: "run-tamper", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	finding := domain.Finding{ID: "finding-tamper", RunID: run.ID, Source: "codex", CreatedAt: at}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-tamper-1", RunID: run.ID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head", CompletedAt: at,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	remediation, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-tamper-2", RunID: run.ID, Round: 2,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "remediated-head", CompletedAt: at.Add(time.Minute),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	disposition := domain.ReviewDispositionRecord{
		FindingID: finding.ID, RunID: run.ID, Round: 1,
		Disposition: domain.ReviewDispositionFixed, Reason: "fixed",
		RemediationInvocationID: remediation.InvocationID, CreatedAt: at,
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, remediation, nil); err != nil {
			return err
		}
		return tx.PutFindingDisposition(ctx, disposition)
	}); err != nil {
		t.Fatal(err)
	}
	return st, disposition, finding
}

func TestFindingDispositionReadFailsClosedOnTamper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tamper := range map[string]func(*Store, domain.Finding) error{
		"copied column": func(st *Store, _ domain.Finding) error {
			_, err := st.db.ExecContext(ctx, `UPDATE finding_dispositions SET reason = 'forged'`)
			return err
		},
		"review membership": func(st *Store, finding domain.Finding) error {
			foreign := finding
			foreign.ID = "finding-other"
			if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutFinding(ctx, foreign) }); err != nil {
				return err
			}
			_, err := st.db.ExecContext(ctx,
				`UPDATE review_record_findings SET finding_id = ? WHERE finding_id = ?`, foreign.ID, finding.ID)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, disposition, finding := seedFindingDisposition(t)
			if err := tamper(st, finding); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetFindingDisposition(ctx, disposition.FindingID, disposition.Round)
				return err
			})
			if err == nil || (!errors.Is(err, errRowInconsistent) && !errors.Is(err, domain.ErrParentKeyMismatch)) {
				t.Fatalf("tampered disposition read = %v", err)
			}
		})
	}
}

func TestFindingDispositionReadRejectsLegacyReasonOnlyRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, disposition, _ := seedFindingDisposition(t)
	if _, err := st.db.ExecContext(ctx, `DELETE FROM finding_dispositions`); err != nil {
		t.Fatal(err)
	}
	disposition.Disposition = domain.ReviewDispositionDeclined
	disposition.RemediationInvocationID = ""
	disposition.Reason = "legacy prose cites sha256:deadbeef"
	body, err := json.Marshal(disposition)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, putFindingDispositionSQL,
		disposition.FindingID, disposition.RunID, disposition.Round,
		disposition.Disposition, disposition.Reason, disposition.RemediationInvocationID,
		formatTime(disposition.CreatedAt), reviewBodyDigest(string(body)), string(body)); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListFindingDispositions(ctx, disposition.RunID)
		return err
	})
	if !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("legacy reason-only disposition read = %v, want ErrEmptyField", err)
	}
}

func TestFindingDispositionTriggerRejectsUnlistedFinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, disposition, finding := seedFindingDisposition(t)
	foreign := finding
	foreign.ID = "finding-unlisted"
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutFinding(ctx, foreign) }); err != nil {
		t.Fatal(err)
	}
	disposition.FindingID = foreign.ID
	body, err := encode(disposition)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.db.ExecContext(ctx, putFindingDispositionSQL,
		disposition.FindingID, disposition.RunID, disposition.Round,
		disposition.Disposition, disposition.Reason, disposition.RemediationInvocationID,
		formatTime(disposition.CreatedAt),
		reviewBodyDigest(body), body)
	if err == nil {
		t.Fatal("direct SQL inserted a disposition for a finding absent from the review round")
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_record_findings SET finding_id = ? WHERE finding_id = ?`,
		foreign.ID, finding.ID); err != nil {
		t.Fatal(err)
	}
	_, err = st.db.ExecContext(ctx, putFindingDispositionSQL,
		disposition.FindingID, disposition.RunID, disposition.Round,
		disposition.Disposition, disposition.Reason, disposition.RemediationInvocationID,
		formatTime(disposition.CreatedAt),
		reviewBodyDigest(body), body)
	if err == nil {
		t.Fatal("direct SQL trusted a bridge row that disagreed with the review body")
	}

	otherRun := domain.Run{
		ID: "run-cross", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	crossRunFinding := finding
	crossRunFinding.ID = "finding-cross-run"
	crossRunFinding.RunID = otherRun.ID
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, otherRun); err != nil {
			return err
		}
		return tx.PutFinding(ctx, crossRunFinding)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_record_findings SET finding_id = ? WHERE finding_id = ?`,
		crossRunFinding.ID, foreign.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_records SET body = json_set(body, '$.finding_ids', json_array(?))`,
		crossRunFinding.ID); err != nil {
		t.Fatal(err)
	}
	disposition.FindingID = crossRunFinding.ID
	body, err = encode(disposition)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.db.ExecContext(ctx, putFindingDispositionSQL,
		disposition.FindingID, disposition.RunID, disposition.Round,
		disposition.Disposition, disposition.Reason, disposition.RemediationInvocationID,
		formatTime(disposition.CreatedAt),
		reviewBodyDigest(body), body)
	if err == nil {
		t.Fatal("direct SQL rebound a cross-run finding into a review round")
	}
}

func TestFindingDispositionTriggerRejectsUnboundRemediationBase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _, finding := seedFindingDisposition(t)
	at := time.Date(2026, 8, 10, 12, 1, 0, 0, time.UTC)
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-base-tamper-3", RunID: finding.RunID, Round: 3,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head-2", CompletedAt: at,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	remediation := record
	remediation.InvocationID = "review-base-tamper-4"
	remediation.Round = 4
	remediation.BaseSHA = "other-base"
	remediation.HeadSHA = "head-3"
	remediation.CompletedAt = at.Add(time.Minute)
	if err := remediation.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, remediation, []domain.Finding{finding})
	}); err != nil {
		t.Fatal(err)
	}
	disposition := domain.ReviewDispositionRecord{
		FindingID: finding.ID, RunID: finding.RunID, Round: 3,
		Disposition: domain.ReviewDispositionFixed, Reason: "fixed",
		RemediationInvocationID: remediation.InvocationID, CreatedAt: at,
	}
	body, err := encode(disposition)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.db.ExecContext(ctx, putFindingDispositionSQL,
		disposition.FindingID, disposition.RunID, disposition.Round,
		disposition.Disposition, disposition.Reason, disposition.RemediationInvocationID,
		formatTime(disposition.CreatedAt),
		reviewBodyDigest(body), body)
	if err == nil {
		t.Fatal("direct SQL accepted a remediation review on a different base")
	}
}

func TestFindingDispositionReplayRejectsCopiedColumnTamper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, disposition, _ := seedFindingDisposition(t)
	if _, err := st.db.ExecContext(ctx, `UPDATE finding_dispositions SET reason = 'forged'`); err != nil {
		t.Fatal(err)
	}
	err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutFindingDisposition(ctx, disposition)
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("replay over copied-column tamper = %v, want row inconsistency", err)
	}
}

func TestListFindingDispositionsRejectsRunIDOmissionTamper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, disposition, _ := seedFindingDisposition(t)
	otherRun := domain.Run{
		ID: "run-other", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	otherFinding := domain.Finding{
		ID: "finding-other-run", RunID: otherRun.ID, Source: "codex",
		CreatedAt: disposition.CreatedAt,
	}
	otherRecord, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-other-1", RunID: otherRun.ID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head", CompletedAt: disposition.CreatedAt,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{otherFinding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, otherRun); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, otherRecord, []domain.Finding{otherFinding})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE finding_dispositions SET run_id = ?`, otherRun.ID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListFindingDispositions(ctx, disposition.RunID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("list after run_id omission tamper = %v, want row inconsistency", err)
	}
}

func TestGetFindingDispositionRejectsKeyOmissionTamper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, prepareAndTamper := range map[string]func(*Store, domain.ReviewDispositionRecord, domain.Finding) error{
		"finding_id": func(st *Store, disposition domain.ReviewDispositionRecord, finding domain.Finding) error {
			foreign := finding
			foreign.ID = "finding-moved-key"
			if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutFinding(ctx, foreign) }); err != nil {
				return err
			}
			_, err := st.db.ExecContext(ctx,
				`UPDATE finding_dispositions SET finding_id = ?`, foreign.ID)
			return err
		},
		"round": func(st *Store, disposition domain.ReviewDispositionRecord, finding domain.Finding) error {
			record, err := domain.NewReviewRecord(domain.ReviewRecord{
				InvocationID: "review-round-tamper-3", RunID: disposition.RunID, Round: 3,
				Provider: "openai", ModelConfiguration: "gpt-codex/high",
				ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
				InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
				CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head", CompletedAt: disposition.CreatedAt,
				CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
				Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
			})
			if err != nil {
				return err
			}
			if err := st.Write(ctx, func(tx *WriteTx) error {
				return tx.PutReviewRecord(ctx, record, []domain.Finding{finding})
			}); err != nil {
				return err
			}
			_, err = st.db.ExecContext(ctx, `UPDATE finding_dispositions SET round = 3`)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, disposition, finding := seedFindingDisposition(t)
			if err := prepareAndTamper(st, disposition, finding); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetFindingDisposition(ctx, disposition.FindingID, disposition.Round)
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("get after %s omission tamper = %v, want row inconsistency", name, err)
			}
		})
	}
}
