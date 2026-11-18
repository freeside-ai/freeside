package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

func TestReviewRecordReadRevalidatesCanonicalBody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/review.db", Options{})
	run := domain.Run{
		ID: "run-review-tamper", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	when := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-tamper-1", RunID: run.ID, Round: 1,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, run.ID, "review-tamper-1"),
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		BaseSHA:             "base", HeadSHA: "head", CompletedAt: when,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_records SET body = json_set(body, '$.configuration_digest', ?)
		 WHERE invocation_id = ?`, "sha256:"+strings.Repeat("f", 64), record.InvocationID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetReviewRecord(ctx, record.InvocationID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("partially rewritten review record read = %v", err)
	}
}

func TestListReviewRecordsRejectsRunKeyOmissionTamper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/review-list.db", Options{})
	first := domain.Run{ID: "run-review-list", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	second := domain.Run{ID: "run-review-list-foreign", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-list-1", RunID: first.ID, Round: 1,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, first.ID, "review-list-1"),
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		BaseSHA:             "base", HeadSHA: "head",
		CompletedAt:        time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, first); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, second); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_records SET run_id = ? WHERE invocation_id = ?`, second.ID, record.InvocationID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListReviewRecords(ctx, first.ID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("run-key omission tamper list = %v, want row inconsistency", err)
	}
}

func TestReviewFailureReadRevalidatesCanonicalBody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/review-failure.db", Options{})
	run := domain.Run{
		ID: "run-review-failure", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	failure := domain.ReviewFailure{
		InvocationID: "review-failure-tamper-1", RunID: run.ID, Round: 1,
		BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureConfiguration,
		Reason: "configuration refused", ObservedAt: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewFailure(ctx, failure)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_failures SET body = json_set(body, '$.reason', 'rewritten')
		 WHERE invocation_id = ?`, failure.InvocationID); err != nil {
		t.Fatal(err)
	}
	err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetReviewFailure(ctx, failure.InvocationID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("partially rewritten review failure read = %v", err)
	}
}

func TestReviewRetryReadRevalidatesCanonicalBody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/review-retry.db", Options{})
	run := domain.Run{
		ID: "run-review-retry-tamper", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	retry := domain.ReviewRetry{
		RunID: run.ID, InvocationID: "review-retry-tamper-1", Round: 1,
		BaseSHA: "base", HeadSHA: "head",
		ObservedAt: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC), Reason: "transient",
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewRetry(ctx, retry)
	}); err != nil {
		t.Fatal(err)
	}
	// Rewrite the body's round without recomputing body_digest: the read must
	// fail closed rather than trust the decoded delay claim. A restored, larger
	// round would otherwise extend the backoff a rewriter never authorized.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_retries SET body = json_set(body, '$.round', 9)
		 WHERE run_id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetReviewRetry(ctx, run.ID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("partially rewritten review retry read = %v", err)
	}
}

// TestReviewRecordAttributionAtWriteAndRead covers the persistence boundary
// of the attribution rule for both record kinds: a record with its admission
// snapshot reads back exactly as stored, and a record without one is refused
// at write and, when a row holds the shape stored before review admission, at
// read.
func TestReviewRecordAttributionAtWriteAndRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/review-attribution.db", Options{})
	run := domain.Run{
		ID: "run-review-attribution", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	when := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	digest := func(fill string) domain.Digest { return domain.Digest("sha256:" + strings.Repeat(fill, 64)) }
	routed := domain.ReviewRecord{
		InvocationID: "review-attribution-1", RunID: run.ID, Round: 1,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, run.ID, "review-attribution-1"),
		ConfigurationDigest: digest("c"), InstructionDigest: digest("d"),
		BaseSHA: "base", HeadSHA: "head", CompletedAt: when,
		CompletionEvidence: digest("e"), Outcome: domain.ReviewClean,
	}
	shadow := domain.ShadowReviewRecord{
		InvocationID: "shadow-attribution-1", RunID: run.ID, ShadowedRound: 1,
		Source:              domain.ShadowReviewClaudeLocal,
		Admission:           fake.RecordAdmission(domain.RoleShadowReviewer, run.ID, "shadow-attribution-1"),
		ConfigurationDigest: digest("c"), InstructionDigest: digest("d"),
		BaseSHA: "base", HeadSHA: "head", CompletedAt: when,
		CompletionEvidence: digest("e"), Outcome: domain.ReviewClean,
	}
	write := func(routed domain.ReviewRecord, shadow domain.ShadowReviewRecord) (routedErr, shadowErr error) {
		routedErr = st.Write(ctx, func(tx *WriteTx) error { return tx.PutReviewRecord(ctx, routed, nil) })
		shadowErr = st.Write(ctx, func(tx *WriteTx) error { return tx.PutShadowReviewRecord(ctx, shadow, nil) })
		return routedErr, shadowErr
	}
	read := func() (gotRouted domain.ReviewRecord, gotShadow domain.ShadowReviewRecord, routedErr, shadowErr error) {
		_ = st.Read(ctx, func(tx *ReadTx) error {
			gotRouted, routedErr = tx.GetReviewRecord(ctx, routed.InvocationID)
			gotShadow, shadowErr = tx.GetShadowReviewRecord(ctx, shadow.InvocationID)
			return nil
		})
		return gotRouted, gotShadow, routedErr, shadowErr
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}

	unattributed, unattributedShadow := routed, shadow
	unattributed.Admission, unattributedShadow.Admission = nil, nil
	if routedErr, shadowErr := write(unattributed, unattributedShadow); !errors.Is(routedErr, domain.ErrEmptyField) ||
		!errors.Is(shadowErr, domain.ErrEmptyField) {
		t.Fatalf("write with no admission = %v, %v", routedErr, shadowErr)
	}

	if routedErr, shadowErr := write(routed, shadow); routedErr != nil || shadowErr != nil {
		t.Fatalf("write records = %v, %v", routedErr, shadowErr)
	}
	gotRouted, gotShadow, routedErr, shadowErr := read()
	if routedErr != nil || shadowErr != nil {
		t.Fatalf("read records = %v, %v", routedErr, shadowErr)
	}
	if !reflect.DeepEqual(gotRouted, routed) || !reflect.DeepEqual(gotShadow, shadow) {
		t.Fatalf("records did not read back as stored:\n%#v\n%#v", gotRouted, gotShadow)
	}

	// Rewrite each stored row, with a matching body digest, into the shape a
	// record had before review admission: the three reviewer strings, no
	// snapshot, and no instruction digest. The record decode ignores the
	// strings, so the missing snapshot is what refuses the row. The shadow
	// provider column follows the old body, as it was stored.
	legacyBody := func(record any, provider string) string {
		t.Helper()
		body, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, "admission")
		delete(fields, "instruction_digest")
		fields["provider"], fields["model_configuration"], fields["cost_owner"] = provider, "gpt-codex/high", "owner"
		body, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	routedBody, shadowBody := legacyBody(routed, "openai"), legacyBody(shadow, "anthropic")
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_records SET body_digest = ?, body = ? WHERE invocation_id = ?`,
		reviewBodyDigest(routedBody), routedBody, routed.InvocationID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE shadow_review_records SET provider = ?, body_digest = ?, body = ? WHERE invocation_id = ?`,
		"anthropic", reviewBodyDigest(shadowBody), shadowBody, shadow.InvocationID); err != nil {
		t.Fatal(err)
	}
	if _, _, routedErr, shadowErr := read(); !errors.Is(routedErr, domain.ErrEmptyField) ||
		!errors.Is(shadowErr, domain.ErrEmptyField) {
		t.Fatalf("read of a row stored before review admission = %v, %v", routedErr, shadowErr)
	}
}
