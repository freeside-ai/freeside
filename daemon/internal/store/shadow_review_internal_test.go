package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func internalShadowRecord(t *testing.T, runID domain.RunID, findingID domain.FindingID) domain.ShadowReviewRecord {
	t.Helper()
	record, err := domain.NewShadowReviewRecord(domain.ShadowReviewRecord{
		InvocationID: "shadow-internal-1", RunID: runID, ShadowedRound: 1,
		Source: domain.ShadowReviewClaudeLocal, Provider: "anthropic",
		ModelConfiguration:  "claude-opus/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head",
		CompletedAt:        time.Date(2026, 8, 24, 4, 0, 0, 0, time.UTC),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{findingID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func internalRoutedCandidate(
	t *testing.T, runID domain.RunID, invocationID domain.InvocationID,
	round int, findingIDs []domain.FindingID, at time.Time,
) domain.ReviewRecord {
	t.Helper()
	outcome := domain.ReviewClean
	if len(findingIDs) > 0 {
		outcome = domain.ReviewFindings
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: invocationID, RunID: runID, Round: round,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head", CompletedAt: at,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            outcome, FindingIDs: findingIDs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestShadowReviewReconstructionRegatesRegisteredSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/shadow-source.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run := domain.Run{ID: "run-shadow-source", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	finding := domain.Finding{
		ID: "shadow-source-finding", RunID: run.ID, Source: string(domain.ShadowReviewClaudeLocal), Severity: "P2",
		Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 1, EndLine: 1},
		Message:  "finding", RawText: "finding", CreatedAt: time.Date(2026, 8, 24, 4, 0, 0, 0, time.UTC),
	}
	record := internalShadowRecord(t, run.ID, finding.ID)
	routed := internalRoutedCandidate(t, run.ID, "routed-source-1", 1, nil, record.CompletedAt)
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, record, []domain.Finding{finding})
	}); err != nil {
		t.Fatal(err)
	}
	var body []byte
	if err := st.db.QueryRowContext(ctx,
		`SELECT body FROM shadow_review_records WHERE invocation_id = ?`, record.InvocationID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["source"] = "decoded_shadow_flag"
	body, err = json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE shadow_review_records
		SET source = ?, body_digest = ?, body = ? WHERE invocation_id = ?`,
		"decoded_shadow_flag", reviewBodyDigest(string(body)), string(body), record.InvocationID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetShadowReviewRecord(ctx, record.InvocationID)
		return err
	})
	if !errors.Is(err, domain.ErrInvalidShadowReviewSource) {
		t.Fatalf("unregistered persisted source read = %v, want ErrInvalidShadowReviewSource", err)
	}
}

func TestShadowReviewReconstructionRegatesFindingSourceSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/shadow-finding-schema.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 24, 4, 15, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-shadow-finding-schema", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	finding := domain.Finding{
		ID: "shadow-finding-schema", RunID: run.ID,
		Source: string(domain.ShadowReviewClaudeLocal), Severity: domain.FindingSeverityP2,
		Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 4, EndLine: 4},
		Message:  "unchecked error", RawText: "unchecked error", CreatedAt: at,
	}
	record := internalShadowRecord(t, run.ID, finding.ID)
	record.CompletedAt = at
	routed := internalRoutedCandidate(t, run.ID, "routed-finding-schema-1", 1, nil, at)
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, record, []domain.Finding{finding})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE findings SET body = json_set(body, '$.severity', '') WHERE id = ?`, finding.ID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetShadowReviewRecord(ctx, record.InvocationID)
		return err
	})
	if !errors.Is(err, domain.ErrInvalidFindingSeverity) {
		t.Fatalf("shadow finding schema reconstruction = %v, want ErrInvalidFindingSeverity", err)
	}
}

func TestShadowReviewReconstructionRegatesRoutedCandidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 24, 4, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*domain.ShadowReviewRecord)
	}{
		{name: "missing routed round", mutate: func(record *domain.ShadowReviewRecord) {
			record.ShadowedRound++
		}},
		{name: "different base", mutate: func(record *domain.ShadowReviewRecord) {
			record.BaseSHA = "other-base"
		}},
		{name: "different head", mutate: func(record *domain.ShadowReviewRecord) {
			record.HeadSHA = "other-head"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(ctx, t.TempDir()+"/shadow-candidate.db", Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			run := domain.Run{
				ID:        domain.RunID("run-shadow-reconstruction-" + strings.ReplaceAll(tc.name, " ", "-")),
				ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			}
			finding := domain.Finding{
				ID:    domain.FindingID("finding-shadow-reconstruction-" + strings.ReplaceAll(tc.name, " ", "-")),
				RunID: run.ID, Source: string(domain.ShadowReviewClaudeLocal), Severity: "P2",
				Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 1, EndLine: 1},
				Message:  "finding", RawText: "finding", CreatedAt: at,
			}
			shadow := internalShadowRecord(t, run.ID, finding.ID)
			shadow.CompletedAt = at
			routed := internalRoutedCandidate(t, run.ID, "routed-reconstruction-1", 1, nil, at)
			if err := st.Write(ctx, func(tx *WriteTx) error {
				if err := tx.PutRun(ctx, run); err != nil {
					return err
				}
				if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
					return err
				}
				return tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{finding})
			}); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&shadow)
			body, err := encode(shadow)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE shadow_review_records
				SET shadowed_round = ?, base_sha = ?, head_sha = ?, body_digest = ?, body = ?
				WHERE invocation_id = ?`, shadow.ShadowedRound, shadow.BaseSHA, shadow.HeadSHA,
				reviewBodyDigest(body), body, shadow.InvocationID); err != nil {
				t.Fatal(err)
			}
			err = st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetShadowReviewRecord(ctx, shadow.InvocationID)
				return err
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("mismatched routed candidate read = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

func TestShadowFindingCannotBindRoutedAdjudication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/shadow-routing.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run := domain.Run{
		ID: "run-shadow-routing", ProjectID: "project-1",
		SpecDigest:   domain.Digest("sha256:" + strings.Repeat("a", 64)),
		PolicyDigest: domain.Digest("sha256:" + strings.Repeat("b", 64)),
	}
	finding := domain.Finding{
		ID: "shadow-routing-finding", RunID: run.ID, Source: string(domain.ShadowReviewClaudeLocal), Severity: "P1",
		Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 1, EndLine: 1},
		Message:  "finding", RawText: "finding", CreatedAt: time.Date(2026, 8, 24, 5, 0, 0, 0, time.UTC),
	}
	record := internalShadowRecord(t, run.ID, finding.ID)
	routed := internalRoutedCandidate(t, run.ID, "routed-shadow-routing-1", 1, nil, record.CompletedAt)
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, record, []domain.Finding{finding})
	}); err != nil {
		t.Fatal(err)
	}
	compatibility := domain.CompatibilityAllowed
	entry, err := domain.NewEngineAdjudicationEntry(
		finding.ID, domain.GoalRequired, &compatibility, domain.RouteRemediate,
		"shadow finding", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := domain.NewFindingAdjudication(
		run.ID, 1, run.SpecDigest, routed.InstructionDigest, run.PolicyDigest,
		[]domain.FindingAdjudicationEntry{entry}, "",

		record.CompletedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		// The routed record is the round authority and has no findings. The
		// shadow finding cannot enter its exact-set adjudication binding.
		return tx.validateFindingAdjudicationBinding(ctx, artifact)
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("shadow finding adjudication binding = %v, want ErrParentKeyMismatch", err)
	}
}

func TestShadowAndRoutedReviewSchemaRejectsDualLinkedFinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 24, 6, 0, 0, 0, time.UTC)

	for _, shadowFirst := range []bool{true, false} {
		name := "routed_then_shadow"
		if shadowFirst {
			name = "shadow_then_routed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(ctx, t.TempDir()+"/dual-link.db", Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			run := domain.Run{
				ID: domain.RunID("run-schema-" + name), ProjectID: "project-1",
				SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			}
			finding := domain.Finding{
				ID: domain.FindingID("finding-schema-" + name), RunID: run.ID,
				Source: string(domain.ShadowReviewClaudeLocal), Severity: "P1",
				Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 1, EndLine: 1},
				Message:  "dual-linked finding", RawText: "dual-linked finding", CreatedAt: at,
			}
			shadow := internalShadowRecord(t, run.ID, finding.ID)
			if err := st.Write(ctx, func(tx *WriteTx) error {
				if err := tx.PutRun(ctx, run); err != nil {
					return err
				}
				if shadowFirst {
					candidate := internalRoutedCandidate(t, run.ID, "routed-schema-1", 1, nil, at)
					if err := tx.PutReviewRecord(ctx, candidate, nil); err != nil {
						return err
					}
					return tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{finding})
				}
				routed := internalRoutedCandidate(t, run.ID, "routed-schema-1", 1,
					[]domain.FindingID{finding.ID}, at)
				return tx.PutReviewRecord(ctx, routed, []domain.Finding{finding})
			}); err != nil {
				t.Fatal(err)
			}

			if shadowFirst {
				_, err = st.db.ExecContext(ctx, `INSERT INTO review_record_findings
					(invocation_id, finding_id, ordinal) VALUES (?, ?, 0)`, "routed-schema-1", finding.ID)
				if err == nil {
					t.Fatal("schema accepted a shadow finding in routed review")
				}
				return
			}

			body, err := encode(shadow)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, putShadowReviewRecordSQL,
				shadow.InvocationID, shadow.RunID, shadow.ShadowedRound, shadow.Source,
				shadow.Provider, shadow.BaseSHA, shadow.HeadSHA, shadow.Outcome,
				formatTime(shadow.CompletedAt), reviewBodyDigest(body), body); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, `INSERT INTO shadow_review_record_findings
				(invocation_id, finding_id, ordinal) VALUES (?, ?, 0)`, shadow.InvocationID, finding.ID); err == nil {
				t.Fatal("schema accepted a routed finding in shadow review")
			}
		})
	}
}

func TestShadowReviewSchemaRejectsDuplicateInvocationAndFindingParent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/shadow-identity.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 24, 6, 30, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-shadow-identity", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	firstFinding := domain.Finding{
		ID: "finding-shadow-identity-1", RunID: run.ID,
		Source: string(domain.ShadowReviewClaudeLocal), Severity: "P2",
		Location: &domain.FindingLocation{Path: "daemon/one.go", StartLine: 1, EndLine: 1},
		Message:  "first finding", RawText: "first finding", CreatedAt: at,
	}
	secondFinding := domain.Finding{
		ID: "finding-shadow-identity-2", RunID: run.ID,
		Source: string(domain.ShadowReviewClaudeLocal), Severity: "P2",
		Location: &domain.FindingLocation{Path: "daemon/two.go", StartLine: 1, EndLine: 1},
		Message:  "second finding", RawText: "second finding", CreatedAt: at,
	}
	firstRouted := internalRoutedCandidate(t, run.ID, "routed-identity-1", 1, nil, at)
	secondRouted := internalRoutedCandidate(t, run.ID, "routed-identity-2", 2, nil, at)
	thirdRouted := internalRoutedCandidate(t, run.ID, "routed-identity-3", 3, nil, at)
	firstShadow := internalShadowRecord(t, run.ID, firstFinding.ID)
	firstShadow.InvocationID = "shadow-identity-1"
	firstShadow.CompletedAt = at
	secondShadow := internalShadowRecord(t, run.ID, secondFinding.ID)
	secondShadow.InvocationID = "shadow-identity-2"
	secondShadow.ShadowedRound = 2
	secondShadow.CompletedAt = at
	failure := domain.ReviewFailure{
		InvocationID: "failure-identity-4", RunID: run.ID, Round: 4,
		BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureTransient,
		Reason: "retry exhausted", ObservedAt: at,
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		for _, routed := range []domain.ReviewRecord{firstRouted, secondRouted, thirdRouted} {
			if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
				return err
			}
		}
		if err := tx.PutReviewFailure(ctx, failure); err != nil {
			return err
		}
		if err := tx.PutShadowReviewRecord(ctx, firstShadow, []domain.Finding{firstFinding}); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, secondShadow, []domain.Finding{secondFinding})
	}); err != nil {
		t.Fatal(err)
	}

	duplicateShadow := secondShadow
	duplicateShadow.InvocationID = thirdRouted.InvocationID
	duplicateShadow.ShadowedRound = 3
	shadowBody, err := encode(duplicateShadow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, putShadowReviewRecordSQL,
		duplicateShadow.InvocationID, duplicateShadow.RunID, duplicateShadow.ShadowedRound,
		duplicateShadow.Source, duplicateShadow.Provider, duplicateShadow.BaseSHA,
		duplicateShadow.HeadSHA, duplicateShadow.Outcome, formatTime(duplicateShadow.CompletedAt),
		reviewBodyDigest(shadowBody), shadowBody); err == nil {
		t.Fatal("schema accepted routed invocation in shadow records")
	}

	duplicateRouted := internalRoutedCandidate(t, run.ID, secondShadow.InvocationID, 5, nil, at)
	routedBody, err := encode(duplicateRouted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, putReviewRecordSQL,
		duplicateRouted.InvocationID, duplicateRouted.RunID, duplicateRouted.Round,
		duplicateRouted.BaseSHA, duplicateRouted.HeadSHA, duplicateRouted.Outcome,
		formatTime(duplicateRouted.CompletedAt), reviewBodyDigest(routedBody), routedBody); err == nil {
		t.Fatal("schema accepted shadow invocation in routed records")
	}

	duplicateFailure := failure
	duplicateFailure.InvocationID = secondShadow.InvocationID
	duplicateFailure.Round = 6
	failureBody, err := encode(duplicateFailure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, putReviewFailureSQL,
		duplicateFailure.InvocationID, duplicateFailure.RunID, duplicateFailure.Round,
		duplicateFailure.Class, formatTime(duplicateFailure.ObservedAt),
		reviewBodyDigest(failureBody), failureBody); err == nil {
		t.Fatal("schema accepted shadow invocation in review failures")
	}

	for name, statement := range map[string]string{
		"shadow":  `UPDATE shadow_review_records SET invocation_id = 'routed-identity-3' WHERE invocation_id = 'shadow-identity-2'`,
		"routed":  `UPDATE review_records SET invocation_id = 'shadow-identity-2' WHERE invocation_id = 'routed-identity-3'`,
		"failure": `UPDATE review_failures SET invocation_id = 'shadow-identity-2' WHERE invocation_id = 'failure-identity-4'`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err == nil {
			t.Errorf("schema accepted %s invocation overlap update", name)
		}
	}

	if _, err := st.db.ExecContext(ctx, `INSERT INTO shadow_review_record_findings
		(invocation_id, finding_id, ordinal) VALUES (?, ?, 1)`,
		secondShadow.InvocationID, firstFinding.ID); err == nil {
		t.Error("schema accepted a second shadow parent for one finding")
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE shadow_review_record_findings
		SET finding_id = ? WHERE invocation_id = ? AND finding_id = ?`,
		firstFinding.ID, secondShadow.InvocationID, secondFinding.ID); err == nil {
		t.Error("schema accepted a shadow finding parent reassignment")
	}
}

func TestShadowReviewSchemaRejectsRetryInvocationOverlap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/shadow-retry-identity.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 24, 7, 15, 0, 0, time.UTC)
	retryRun := domain.Run{
		ID: "run-shadow-retry-routed", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	shadowRun := domain.Run{
		ID: "run-shadow-retry-shadow", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	retryCandidate := internalRoutedCandidate(t, retryRun.ID, "routed-retry-candidate", 1, nil, at)
	shadowCandidate := internalRoutedCandidate(t, shadowRun.ID, "routed-shadow-candidate", 1, nil, at)
	retry := domain.ReviewRetry{
		RunID: retryRun.ID, InvocationID: "routed-retry-invocation", Round: 2,
		BaseSHA: "base", HeadSHA: "head", ObservedAt: at, Reason: "transient poll failure",
	}
	shadow := internalShadowRecord(t, shadowRun.ID, "unused-shadow-finding")
	shadow.InvocationID = "shadow-retry-invocation"
	shadow.Outcome = domain.ReviewClean
	shadow.FindingIDs = nil
	shadow.CompletedAt = at
	if err := st.Write(ctx, func(tx *WriteTx) error {
		for _, run := range []domain.Run{retryRun, shadowRun} {
			if err := tx.PutRun(ctx, run); err != nil {
				return err
			}
		}
		for _, candidate := range []domain.ReviewRecord{retryCandidate, shadowCandidate} {
			if err := tx.PutReviewRecord(ctx, candidate, nil); err != nil {
				return err
			}
		}
		if err := tx.PutReviewRetry(ctx, retry); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, shadow, nil)
	}); err != nil {
		t.Fatal(err)
	}

	duplicateShadow := internalShadowRecord(t, retryRun.ID, "unused-retry-finding")
	duplicateShadow.InvocationID = retry.InvocationID
	duplicateShadow.Outcome = domain.ReviewClean
	duplicateShadow.FindingIDs = nil
	duplicateShadow.CompletedAt = at
	shadowBody, err := encode(duplicateShadow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, putShadowReviewRecordSQL,
		duplicateShadow.InvocationID, duplicateShadow.RunID, duplicateShadow.ShadowedRound,
		duplicateShadow.Source, duplicateShadow.Provider, duplicateShadow.BaseSHA,
		duplicateShadow.HeadSHA, duplicateShadow.Outcome, formatTime(duplicateShadow.CompletedAt),
		reviewBodyDigest(shadowBody), shadowBody); err == nil {
		t.Fatal("schema accepted retry invocation in shadow records")
	}

	duplicateRetry := retry
	duplicateRetry.RunID = shadowRun.ID
	duplicateRetry.InvocationID = shadow.InvocationID
	retryBody, err := encode(duplicateRetry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, putReviewRetrySQL,
		duplicateRetry.RunID, duplicateRetry.InvocationID, duplicateRetry.Round,
		duplicateRetry.BaseSHA, duplicateRetry.HeadSHA, formatTime(duplicateRetry.ObservedAt),
		reviewBodyDigest(retryBody), retryBody); err == nil {
		t.Fatal("schema accepted shadow invocation in review retries")
	}

	for name, statement := range map[string]string{
		"shadow": `UPDATE shadow_review_records SET invocation_id = 'routed-retry-invocation'
			WHERE invocation_id = 'shadow-retry-invocation'`,
		"retry": `UPDATE review_retries SET invocation_id = 'shadow-retry-invocation'
			WHERE invocation_id = 'routed-retry-invocation'`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err == nil {
			t.Errorf("schema accepted %s invocation overlap update", name)
		}
	}
}

func TestReviewReconstructionRejectsDuplicateShadowRetryInvocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/duplicate-shadow-retry-invocation.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 24, 7, 30, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-duplicate-shadow-retry", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	candidate := internalRoutedCandidate(t, run.ID, "routed-retry-read-candidate", 1, nil, at)
	shadow := internalShadowRecord(t, run.ID, "unused-retry-read-finding")
	shadow.InvocationID = "shared-retry-read-invocation"
	shadow.Outcome = domain.ReviewClean
	shadow.FindingIDs = nil
	shadow.CompletedAt = at
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, candidate, nil); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, shadow, nil)
	}); err != nil {
		t.Fatal(err)
	}
	retry := domain.ReviewRetry{
		RunID: run.ID, InvocationID: shadow.InvocationID, Round: 2,
		BaseSHA: "base", HeadSHA: "head", ObservedAt: at, Reason: "transient poll failure",
	}
	body, err := encode(retry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DROP TRIGGER review_retry_rejects_shadow_invocation`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, putReviewRetrySQL,
		retry.RunID, retry.InvocationID, retry.Round, retry.BaseSHA, retry.HeadSHA,
		formatTime(retry.ObservedAt), reviewBodyDigest(body), body); err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func(*ReadTx) error{
		"shadow": func(tx *ReadTx) error {
			_, err := tx.GetShadowReviewRecord(ctx, shadow.InvocationID)
			return err
		},
		"retry": func(tx *ReadTx) error {
			_, err := tx.GetReviewRetry(ctx, run.ID)
			return err
		},
	} {
		err := st.Read(ctx, read)
		if !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("%s reconstruction = %v, want ErrParentKeyMismatch", name, err)
		}
	}
}

func TestReviewReconstructionRejectsDuplicateShadowInvocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/duplicate-shadow-invocation.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 24, 6, 45, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-duplicate-shadow-invocation", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	candidate := internalRoutedCandidate(t, run.ID, "routed-duplicate-candidate", 1, nil, at)
	shadow, err := domain.NewShadowReviewRecord(domain.ShadowReviewRecord{
		InvocationID: "shared-damaged-invocation", RunID: run.ID, ShadowedRound: 1,
		Source: domain.ShadowReviewClaudeLocal, Provider: "anthropic",
		ModelConfiguration:  "claude-opus/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head", CompletedAt: at,
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
		if err := tx.PutReviewRecord(ctx, candidate, nil); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, shadow, nil)
	}); err != nil {
		t.Fatal(err)
	}
	duplicate := internalRoutedCandidate(t, run.ID, shadow.InvocationID, 2, nil, at)
	body, err := encode(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DROP TRIGGER routed_review_rejects_shadow_invocation`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, putReviewRecordSQL,
		duplicate.InvocationID, duplicate.RunID, duplicate.Round, duplicate.BaseSHA,
		duplicate.HeadSHA, duplicate.Outcome, formatTime(duplicate.CompletedAt),
		reviewBodyDigest(body), body); err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func(*ReadTx) error{
		"shadow": func(tx *ReadTx) error {
			_, err := tx.GetShadowReviewRecord(ctx, shadow.InvocationID)
			return err
		},
		"routed": func(tx *ReadTx) error {
			_, err := tx.GetReviewRecord(ctx, duplicate.InvocationID)
			return err
		},
	} {
		if err := st.Read(ctx, read); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("%s reconstruction = %v, want ErrParentKeyMismatch", name, err)
		}
	}
}

func TestShadowReconstructionRejectsFindingWithMultipleParents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/duplicate-shadow-parent.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 24, 6, 50, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-duplicate-shadow-parent", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	firstFinding := domain.Finding{
		ID: "finding-duplicate-shadow-parent-1", RunID: run.ID,
		Source: string(domain.ShadowReviewClaudeLocal), Severity: "P2",
		Location: &domain.FindingLocation{Path: "daemon/one.go", StartLine: 1, EndLine: 1},
		Message:  "first finding", RawText: "first finding", CreatedAt: at,
	}
	secondFinding := domain.Finding{
		ID: "finding-duplicate-shadow-parent-2", RunID: run.ID,
		Source: string(domain.ShadowReviewClaudeLocal), Severity: "P2",
		Location: &domain.FindingLocation{Path: "daemon/two.go", StartLine: 1, EndLine: 1},
		Message:  "second finding", RawText: "second finding", CreatedAt: at,
	}
	firstRouted := internalRoutedCandidate(t, run.ID, "routed-parent-read-1", 1, nil, at)
	secondRouted := internalRoutedCandidate(t, run.ID, "routed-parent-read-2", 2, nil, at)
	firstShadow := internalShadowRecord(t, run.ID, firstFinding.ID)
	firstShadow.InvocationID = "shadow-parent-read-1"
	firstShadow.CompletedAt = at
	secondShadow := internalShadowRecord(t, run.ID, secondFinding.ID)
	secondShadow.InvocationID = "shadow-parent-read-2"
	secondShadow.ShadowedRound = 2
	secondShadow.CompletedAt = at
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, firstRouted, nil); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, secondRouted, nil); err != nil {
			return err
		}
		if err := tx.PutShadowReviewRecord(ctx, firstShadow, []domain.Finding{firstFinding}); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, secondShadow, []domain.Finding{secondFinding})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DROP TABLE shadow_review_record_findings`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `CREATE TABLE shadow_review_record_findings (
		invocation_id TEXT NOT NULL, finding_id TEXT NOT NULL, ordinal INTEGER NOT NULL
	) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]any{
		{firstShadow.InvocationID, firstFinding.ID, 0},
		{secondShadow.InvocationID, firstFinding.ID, 0},
		{secondShadow.InvocationID, secondFinding.ID, 1},
	} {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO shadow_review_record_findings
			(invocation_id, finding_id, ordinal) VALUES (?, ?, ?)`, args...); err != nil {
			t.Fatal(err)
		}
	}
	secondShadow.FindingIDs = []domain.FindingID{firstFinding.ID, secondFinding.ID}
	body, err := encode(secondShadow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE shadow_review_records
		SET body_digest = ?, body = ? WHERE invocation_id = ?`,
		reviewBodyDigest(body), body, secondShadow.InvocationID); err != nil {
		t.Fatal(err)
	}
	for _, shadow := range []domain.ShadowReviewRecord{firstShadow, secondShadow} {
		err := st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetShadowReviewRecord(ctx, shadow.InvocationID)
			return err
		})
		if !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("shadow %q reconstruction = %v, want ErrParentKeyMismatch", shadow.InvocationID, err)
		}
	}
}

func TestReviewReconstructionRejectsDualLinkedFinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/dual-link-reconstruction.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 24, 7, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-dual-reconstruction", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	finding := domain.Finding{
		ID: "finding-dual-reconstruction", RunID: run.ID,
		Source: string(domain.ShadowReviewClaudeLocal), Severity: "P1",
		Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 1, EndLine: 1},
		Message:  "dual-linked finding", RawText: "dual-linked finding", CreatedAt: at,
	}
	shadow := internalShadowRecord(t, run.ID, finding.ID)
	candidate := internalRoutedCandidate(t, run.ID, "routed-candidate-reconstruction", 1, nil, at)
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, candidate, nil); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{finding})
	}); err != nil {
		t.Fatal(err)
	}
	routed := internalRoutedCandidate(t, run.ID, "routed-dual-reconstruction", 2,
		[]domain.FindingID{finding.ID}, at)
	body, err := encode(routed)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a damaged store that lost the insert trigger, then write the
	// forbidden second link. Both lane reconstructions must still fail closed.
	if _, err := st.db.ExecContext(ctx, `DROP TRIGGER routed_review_finding_rejects_shadow`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, putReviewRecordSQL,
		routed.InvocationID, routed.RunID, routed.Round, routed.BaseSHA,
		routed.HeadSHA, routed.Outcome, formatTime(routed.CompletedAt),
		reviewBodyDigest(body), body); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO review_record_findings
		(invocation_id, finding_id, ordinal) VALUES (?, ?, 0)`, routed.InvocationID, finding.ID); err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func(*ReadTx) error{
		"routed": func(tx *ReadTx) error {
			_, err := tx.GetReviewRecord(ctx, routed.InvocationID)
			return err
		},
		"shadow": func(tx *ReadTx) error {
			_, err := tx.GetShadowReviewRecord(ctx, shadow.InvocationID)
			return err
		},
	} {
		err := st.Read(ctx, read)
		if !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("%s reconstruction = %v, want ErrParentKeyMismatch", name, err)
		}
	}
}
