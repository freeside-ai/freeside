package wardstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestCodexEnrollmentFailureClassMapsEveryRegisteredValue(t *testing.T) {
	for _, class := range ward.AllCodexAuthEnrollmentFailures {
		if _, err := codexEnrollmentFailureClass(class); err != nil {
			t.Fatalf("registered failure class %q is unmapped: %v", class, err)
		}
	}
	if _, err := codexEnrollmentFailureClass("unknown"); err == nil {
		t.Fatal("unknown enrollment failure class mapped")
	}
}

func TestDecodeCodexReviewRejectsUnknownAndTrailingFields(t *testing.T) {
	t.Parallel()
	for _, body := range [][]byte{
		[]byte(`{"run_id":"run-1","unknown":true}`),
		[]byte(`{"run_id":"run-1"} {"run_id":"run-2"}`),
	} {
		_, err := decodeCodexReview[struct {
			RunID string `json:"run_id"`
		}](body)
		if err == nil {
			t.Fatalf("decodeCodexReview(%q) succeeded", body)
		}
	}
}

func TestCodexAuthReenrollmentOccurrencesRequireCanonicalNumericOrder(t *testing.T) {
	id := domain.AuthIdentityID("codex-order")
	prefix := store.CodexReenrollmentMarkerPrefix(id)
	item := func(occurrence int) domain.AttentionItem {
		got, err := codexAuthReenrollmentItem(
			id, occurrence, "project-1", 1, domain.StatusOpen, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, suffix := range []string{"+1", "01", " 1", "0", "-1", "999999999999999999999999999999999999"} {
		malformed := item(1)
		malformed.ID = domain.ItemID(prefix + suffix)
		if _, err := validateCodexAuthReenrollmentItem(malformed, id); !errors.Is(err, domain.ErrCodexReenrollmentMarkerMismatch) {
			t.Errorf("occurrence suffix %q accepted", suffix)
		}
	}
	items := []store.Snapshotted[domain.AttentionItem]{
		{Value: item(10)},
		{Value: item(2)},
	}
	occurrences, err := scanCodexAuthReenrollmentOccurrences(items, id)
	if err != nil {
		t.Fatal(err)
	}
	if occurrences.latestOccurrence != 10 || occurrences.latest.ID != domain.ItemID(prefix+"10") {
		t.Fatalf("latest occurrence = %d, %q", occurrences.latestOccurrence, occurrences.latest.ID)
	}
	if _, err := store.NextCodexReenrollmentMarkerOccurrence(math.MaxInt); !errors.Is(err, domain.ErrCodexReenrollmentMarkerMismatch) {
		t.Fatalf("exhausted occurrence = %v, want marker mismatch", err)
	}
}

func TestCodexReviewJournalClassifiesRejectedPersistedRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/seed/candidate", Verification: exec.ReviewVerificationEvidence{
			Outcome:                domain.VerificationPassed,
			RecipeDigest:           domain.Digest("sha256:" + strings.Repeat("c", 64)),
			EvidenceSnapshotDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		}, Instructions: instructions, RequestedAt: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	}
	if err := adapters.Journal.PutCodexReviewRequest(ctx, "review-run-1", request); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, body := range []string{`{"run_id":"run-1"}`, `{"run_id":`} {
		if _, err := db.ExecContext(ctx,
			`UPDATE codex_review_requests SET body = ? WHERE invocation_id = ?`,
			body, "review-run-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := adapters.Journal.GetCodexReviewRequest(ctx, "review-run-1"); !errors.Is(err, ward.ErrCodexReviewRequestRejected) {
			t.Fatalf("invalid persisted request %q = %v, want ErrCodexReviewRequestRejected", body, err)
		}
	}
	requestFields := make(map[string]json.RawMessage)
	requestBody, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(requestBody, &requestFields); err != nil {
		t.Fatal(err)
	}
	delete(requestFields, "instructions")
	legacyBody, err := json.Marshal(requestFields)
	if err != nil {
		t.Fatal(err)
	}
	legacyDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(legacyBody))
	if _, err := db.ExecContext(ctx,
		`UPDATE codex_review_requests SET body = ?, body_digest = ? WHERE invocation_id = ?`,
		string(legacyBody), legacyDigest, "review-run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := adapters.Journal.GetCodexReviewRequest(ctx, "review-run-1"); !errors.Is(err, exec.ErrLegacyReviewRequest) {
		t.Fatalf("legacy persisted request = %v, want ErrLegacyReviewRequest", err)
	}
	outcome := ward.CodexReviewSourceOutcome{
		InvocationID: "review-run-1", FailureClass: domain.ReviewFailureContradiction,
		Failure: "invalid reviewer result",
	}
	if err := adapters.Journal.PutCodexReviewOutcome(ctx, "review-run-1", outcome); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"invocation_id":"review-run-1"}`, `{"invocation_id":`} {
		if _, err := db.ExecContext(ctx,
			`UPDATE codex_review_outcomes SET body = ? WHERE invocation_id = ?`,
			body, "review-run-1"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := adapters.Journal.GetCodexReviewOutcome(ctx, "review-run-1"); !errors.Is(err, ward.ErrCodexReviewOutcomeRejected) {
			t.Fatalf("invalid persisted outcome %q = %v, want ErrCodexReviewOutcomeRejected", body, err)
		}
	}
	workspace := ward.CodexReviewWorkspaceBinding{
		SourceRunID: "review-run-1", Volume: "review-workspace",
		OwnershipToken: strings.Repeat("a", 32),
	}
	if err := adapters.Journal.PutCodexReviewWorkspaceBinding(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE codex_review_workspaces SET volume = ? WHERE source_run_id = ?`,
		"rewritten-workspace", "review-run-1"); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Journal.DeleteCodexReviewWorkspaceBinding(ctx, workspace); !errors.Is(err, ward.ErrConformance) {
		t.Fatalf("delete rewritten workspace volume = %v, want ErrConformance", err)
	}
	if _, err := adapters.Journal.GetCodexReviewWorkspaceBinding(ctx, "review-run-1"); !errors.Is(err, ward.ErrConformance) {
		t.Fatalf("workspace after refused delete = %v, want ErrConformance", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE codex_review_workspaces SET volume = ? WHERE source_run_id = ?`,
		workspace.Volume, "review-run-1"); err != nil {
		t.Fatal(err)
	}
	intent := ward.CodexReviewLaunchIntent{
		RunID: "review-run-1", State: ward.CodexReviewIntentPreparing,
	}
	if err := adapters.Journal.BeginCodexReviewIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	binding := ward.CodexReviewJournalBinding{RunID: "review-run-1"}
	if err := adapters.Journal.PutCodexReviewBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE codex_review_intents SET state = 'started' WHERE run_id = ?`, "review-run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := adapters.Journal.GetCodexReviewIntent(ctx, "review-run-1"); !errors.Is(err, ward.ErrConformance) {
		t.Fatalf("rewritten intent state = %v, want ErrConformance", err)
	}
	if got, err := adapters.Journal.ListCodexReviewIntentIDs(ctx); err != nil ||
		!slices.Equal(got, []string{"review-run-1"}) {
		t.Fatalf("listed rewritten intent id = %v, %v", got, err)
	}
	if err := adapters.Journal.BeginCodexReviewIntent(ctx, intent); !errors.Is(err, ward.ErrConformance) {
		t.Fatalf("restart with rewritten intent state = %v, want ErrConformance", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE codex_review_intents SET state = 'preparing' WHERE run_id = ?`, "review-run-1"); err != nil {
		t.Fatal(err)
	}
	rewrittenIntent := intent
	rewrittenIntent.OwnershipToken = strings.Repeat("f", 32)
	rewrittenBody, err := marshalCodexReview(rewrittenIntent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE codex_review_intents SET body = ? WHERE run_id = ?`,
		string(rewrittenBody), "review-run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := adapters.Journal.GetCodexReviewIntent(ctx, "review-run-1"); !errors.Is(err, ward.ErrConformance) {
		t.Fatalf("validly rewritten intent = %v, want ErrConformance", err)
	}
	for _, tc := range []struct {
		name    string
		corrupt func() error
		read    func() error
	}{
		{
			name: "codex_review_workspaces",
			corrupt: func() error {
				_, err := db.ExecContext(ctx,
					`UPDATE codex_review_workspaces SET body = ? WHERE source_run_id = ?`,
					`{"broken":`, "review-run-1")
				return err
			},
			read: func() error {
				_, err := adapters.Journal.GetCodexReviewWorkspaceBinding(ctx, "review-run-1")
				return err
			},
		},
		{
			name: "codex_review_intents",
			corrupt: func() error {
				_, err := db.ExecContext(ctx,
					`UPDATE codex_review_intents SET body = ? WHERE run_id = ?`,
					`{"broken":`, "review-run-1")
				return err
			},
			read: func() error {
				_, err := adapters.Journal.GetCodexReviewIntent(ctx, "review-run-1")
				return err
			},
		},
		{
			name: "codex_review_bindings",
			corrupt: func() error {
				_, err := db.ExecContext(ctx,
					`UPDATE codex_review_bindings SET body = ? WHERE run_id = ?`,
					`{"broken":`, "review-run-1")
				return err
			},
			read: func() error {
				_, err := adapters.Journal.GetCodexReviewBinding(ctx, "review-run-1")
				return err
			},
		},
	} {
		if err := tc.corrupt(); err != nil {
			t.Fatal(err)
		}
		if err := tc.read(); !errors.Is(err, ward.ErrConformance) {
			t.Fatalf("rejected %s row = %v, want ErrConformance", tc.name, err)
		}
	}
}

func TestCodexReviewJournalRejectsRewrittenOutcomeAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	when := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ready := range []bool{false, true} {
		id := "review-outcome-collected"
		if ready {
			id = "review-outcome-ready"
		}
		request := exec.ReviewRequest{
			RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42,
			BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
			Workspace: "/seed/candidate", Verification: exec.ReviewVerificationEvidence{
				Outcome:                domain.VerificationPassed,
				RecipeDigest:           domain.Digest("sha256:" + strings.Repeat("c", 64)),
				EvidenceSnapshotDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
			}, Instructions: instructions, RequestedAt: when,
		}
		if err := adapters.Journal.PutCodexReviewRequest(ctx, id, request); err != nil {
			t.Fatal(err)
		}
		collectionEvidence := domain.Digest("sha256:" + strings.Repeat("e", 64))
		result := exec.ReviewResult{
			InvocationID: domain.InvocationID(id), BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA,
			Provider: "openai", ModelConfiguration: "gpt-codex/high",
			ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("f", 64)),
			InstructionDigest:   instructions.ResultDigest, CostOwner: "owner",
			CompletedAt: when,
			Findings: []domain.Finding{{
				ID: "finding-1", RunID: request.RunID, Source: "codex_local", Severity: "P1",
				Location: &domain.FindingLocation{Path: "main.go", StartLine: 1, EndLine: 1}, Message: "unsafe", RawText: "unsafe", CreatedAt: when,
			}},
		}
		result.CompletionEvidence, err = ward.CodexReviewResultEvidence(result, collectionEvidence)
		if err != nil {
			t.Fatal(err)
		}
		outcome := ward.CodexReviewSourceOutcome{
			InvocationID: domain.InvocationID(id), Result: &result, CollectionEvidence: collectionEvidence,
		}
		if err := adapters.Journal.PutCodexReviewOutcome(ctx, id, outcome); err != nil {
			t.Fatal(err)
		}
		if !ready {
			if _, err := db.ExecContext(ctx,
				`UPDATE codex_review_outcomes SET state = 'ready' WHERE invocation_id = ?`, id); err != nil {
				t.Fatal(err)
			}
			if _, _, err := adapters.Journal.GetCodexReviewOutcome(ctx, id); !errors.Is(err, ward.ErrCodexReviewOutcomeRejected) {
				t.Fatalf("rewritten outcome state = %v, want ErrCodexReviewOutcomeRejected", err)
			}
			if err := adapters.Journal.MarkCodexReviewOutcomeReady(ctx, id); !errors.Is(err, ward.ErrConformance) {
				t.Fatalf("ready transition with rewritten state = %v, want ErrConformance", err)
			}
			if _, err := db.ExecContext(ctx,
				`UPDATE codex_review_outcomes SET state = 'collected' WHERE invocation_id = ?`, id); err != nil {
				t.Fatal(err)
			}
		}
		if ready {
			if err := adapters.Journal.MarkCodexReviewOutcomeReady(ctx, id); err != nil {
				t.Fatal(err)
			}
			if _, gotReady, err := adapters.Journal.GetCodexReviewOutcome(ctx, id); err != nil || !gotReady {
				t.Fatalf("authenticated ready outcome = %v, %v", gotReady, err)
			}
		}
		result.Findings = nil
		result.CompletionEvidence, err = ward.CodexReviewResultEvidence(result, collectionEvidence)
		if err != nil {
			t.Fatal(err)
		}
		body, err := marshalCodexReview(ward.CodexReviewSourceOutcome{
			InvocationID: domain.InvocationID(id), Result: &result, CollectionEvidence: collectionEvidence,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE codex_review_outcomes SET body = ? WHERE invocation_id = ?`, string(body), id); err != nil {
			t.Fatal(err)
		}
		if _, _, err := adapters.Journal.GetCodexReviewOutcome(ctx, id); !errors.Is(err, ward.ErrCodexReviewOutcomeRejected) {
			t.Fatalf("rewritten outcome ready=%v = %v, want ErrCodexReviewOutcomeRejected", ready, err)
		}
	}
}
