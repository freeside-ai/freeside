package engine

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestDeriveReviewYieldHistoryTracksNewRecurringAndDispositions(t *testing.T) {
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	findings := map[domain.FindingID]domain.Finding{
		"finding-round-1": {
			ID: "finding-round-1", RunID: "run-1", Source: "codex_local",
			Location: &domain.FindingLocation{Path: "daemon/review.go", StartLine: 10, EndLine: 10},
			Message:  "unchecked   review result", RawText: "unchecked review result", CreatedAt: at,
		},
		"finding-recurring-round-2": {
			ID: "finding-recurring-round-2", RunID: "run-1", Source: "codex_local",
			Location: &domain.FindingLocation{Path: "daemon/review.go", StartLine: 40, EndLine: 40},
			Message:  "unchecked review result", RawText: "unchecked review result", CreatedAt: at,
		},
		"finding-new-round-2": {
			ID: "finding-new-round-2", RunID: "run-1", Source: "codex_local",
			Location: &domain.FindingLocation{Path: "daemon/store.go", StartLine: 20, EndLine: 20},
			Message:  "store error is ignored", RawText: "store error is ignored", CreatedAt: at,
		},
	}
	records := []domain.ReviewRecord{
		{Round: 1, Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"finding-round-1"}},
		{Round: 2, Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"finding-new-round-2", "finding-recurring-round-2"}},
		{Round: 3, Outcome: domain.ReviewClean},
	}
	dispositions := []domain.ReviewDispositionRecord{
		{Round: 1, Disposition: domain.ReviewDispositionFixed},
		{Round: 2, Disposition: domain.ReviewDispositionDeclined},
		{Round: 2, Disposition: domain.ReviewDispositionDeferred},
	}

	got, err := deriveReviewYieldHistory(records, dispositions, findings)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ReviewYieldHistory{
		Rounds: []domain.ReviewYieldRound{
			{Round: 1, FindingsIngested: 1, NewFindings: 1, Fixed: 1, Outcome: domain.ReviewFindings},
			{Round: 2, FindingsIngested: 2, NewFindings: 1, RecurringFindings: 1, Declined: 1, Deferred: 1, Outcome: domain.ReviewFindings},
			{Round: 3, Outcome: domain.ReviewClean},
		},
		TerminalOutcome: domain.ReviewClean,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %#v, want %#v", got, want)
	}
}

func TestDeriveReviewYieldHistoryCountsUnfingerprintableFindingsAsNew(t *testing.T) {
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	findings := map[domain.FindingID]domain.Finding{
		"review-level-round-1": {
			ID: "review-level-round-1", RunID: "run-1", Source: "codex_local",
			Message: "review-level finding", RawText: "review-level finding", CreatedAt: at,
		},
		"blank-message-round-1": {
			ID: "blank-message-round-1", RunID: "run-1", Source: "codex_local",
			Location: &domain.FindingLocation{Path: "daemon/review.go"},
			Message:  " \t\n", RawText: "blank message", CreatedAt: at,
		},
		"review-level-round-2": {
			ID: "review-level-round-2", RunID: "run-1", Source: "codex_local",
			Message: "review-level finding", RawText: "review-level finding", CreatedAt: at,
		},
		"blank-message-round-2": {
			ID: "blank-message-round-2", RunID: "run-1", Source: "codex_local",
			Location: &domain.FindingLocation{Path: "daemon/review.go"},
			Message:  " \t\n", RawText: "blank message", CreatedAt: at,
		},
	}
	records := []domain.ReviewRecord{
		{Round: 1, Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{
			"review-level-round-1", "blank-message-round-1",
		}},
		{Round: 2, Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{
			"review-level-round-2", "blank-message-round-2",
		}},
		{Round: 3, Outcome: domain.ReviewClean},
	}
	dispositions := []domain.ReviewDispositionRecord{
		{Round: 1, Disposition: domain.ReviewDispositionDeclined},
		{Round: 1, Disposition: domain.ReviewDispositionDeferred},
		{Round: 2, Disposition: domain.ReviewDispositionDeclined},
		{Round: 2, Disposition: domain.ReviewDispositionDeferred},
	}

	got, err := deriveReviewYieldHistory(records, dispositions, findings)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ReviewYieldHistory{
		Rounds: []domain.ReviewYieldRound{
			{
				Round: 1, FindingsIngested: 2, NewFindings: 2,
				Declined: 1, Deferred: 1, Outcome: domain.ReviewFindings,
			},
			{
				Round: 2, FindingsIngested: 2, NewFindings: 2,
				Declined: 1, Deferred: 1, Outcome: domain.ReviewFindings,
			},
			{Round: 3, Outcome: domain.ReviewClean},
		},
		TerminalOutcome: domain.ReviewClean,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %#v, want %#v", got, want)
	}
}

func TestDeriveReviewYieldHistoryResetsRecurrenceForNewReviewerConfiguration(t *testing.T) {
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	findings := map[domain.FindingID]domain.Finding{}
	for round := 1; round <= 4; round++ {
		id := domain.FindingID(fmt.Sprintf("finding-round-%d", round))
		findings[id] = domain.Finding{
			ID: id, RunID: "run-1", Source: "codex_local",
			Location: &domain.FindingLocation{
				Path: "daemon/review.go", StartLine: round * 10, EndLine: round * 10,
			},
			Message: "unchecked review result", RawText: "unchecked review result", CreatedAt: at,
		}
	}
	firstConfiguration := domain.Digest("sha256:" + strings.Repeat("a", 64))
	secondConfiguration := domain.Digest("sha256:" + strings.Repeat("b", 64))
	records := []domain.ReviewRecord{
		{
			Round: 1, ConfigurationDigest: firstConfiguration, Outcome: domain.ReviewFindings,
			FindingIDs: []domain.FindingID{"finding-round-1"},
		},
		{
			Round: 2, ConfigurationDigest: firstConfiguration, Outcome: domain.ReviewFindings,
			FindingIDs: []domain.FindingID{"finding-round-2"},
		},
		{
			Round: 3, ConfigurationDigest: secondConfiguration, Outcome: domain.ReviewFindings,
			FindingIDs: []domain.FindingID{"finding-round-3"},
		},
		{
			Round: 4, ConfigurationDigest: secondConfiguration, Outcome: domain.ReviewFindings,
			FindingIDs: []domain.FindingID{"finding-round-4"},
		},
		{Round: 5, ConfigurationDigest: secondConfiguration, Outcome: domain.ReviewClean},
	}

	got, err := deriveReviewYieldHistory(records, nil, findings)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ReviewYieldHistory{
		Rounds: []domain.ReviewYieldRound{
			{Round: 1, FindingsIngested: 1, NewFindings: 1, Outcome: domain.ReviewFindings},
			{Round: 2, FindingsIngested: 1, RecurringFindings: 1, Outcome: domain.ReviewFindings},
			{Round: 3, FindingsIngested: 1, NewFindings: 1, Outcome: domain.ReviewFindings},
			{Round: 4, FindingsIngested: 1, RecurringFindings: 1, Outcome: domain.ReviewFindings},
			{Round: 5, Outcome: domain.ReviewClean},
		},
		TerminalOutcome: domain.ReviewClean,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %#v, want %#v", got, want)
	}
}

func TestReviewYieldHistoryDerivesPersistedMultiRoundHistoryAfterRestart(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	run := domain.Run{
		ID: "run-yield-restart", ProjectID: "project-yield-restart",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	first := domain.Finding{
		ID: "finding-round-1", RunID: run.ID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/review.go", StartLine: 10, EndLine: 10},
		Message:  "unchecked review result", RawText: "unchecked review result", CreatedAt: at,
	}
	recurring := first
	recurring.ID = "finding-recurring-round-2"
	recurring.Location = &domain.FindingLocation{
		Path: "daemon/review.go", StartLine: 40, EndLine: 40,
	}
	recurring.CreatedAt = at.Add(time.Minute)
	newFinding := domain.Finding{
		ID: "finding-new-round-2", RunID: run.ID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/store.go", StartLine: 20, EndLine: 20},
		Message:  "store error is ignored", RawText: "store error is ignored",
		CreatedAt: at.Add(time.Minute),
	}
	record := func(round int, head string, outcome domain.ReviewOutcome, ids ...domain.FindingID) domain.ReviewRecord {
		t.Helper()
		got, recordErr := domain.NewReviewRecord(domain.ReviewRecord{
			InvocationID: domain.InvocationID(fmt.Sprintf("review-yield-%d", round)),
			RunID:        run.ID, Round: round, Provider: "openai",
			ModelConfiguration:  "gpt-codex/high",
			ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
			InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
			CostOwner:           "owner", BaseSHA: "base", HeadSHA: head,
			CompletedAt:        at.Add(time.Duration(round) * time.Minute),
			CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
			Outcome:            outcome, FindingIDs: ids,
		})
		if recordErr != nil {
			t.Fatalf("new round %d review record: %v", round, recordErr)
		}
		return got
	}
	roundOne := record(1, "head-1", domain.ReviewFindings, first.ID)
	roundTwo := record(2, "head-2", domain.ReviewFindings, recurring.ID, newFinding.ID)
	roundThree := record(3, "head-2", domain.ReviewClean)
	fixed := domain.ReviewDispositionRecord{
		FindingID: first.ID, RunID: run.ID, Round: 1,
		Disposition: domain.ReviewDispositionFixed, Reason: "fixed",
		RemediationInvocationID: roundTwo.InvocationID, CreatedAt: at.Add(time.Minute),
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, roundOne, []domain.Finding{first}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, roundTwo, []domain.Finding{recurring, newFinding}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, roundThree, nil); err != nil {
			return err
		}
		return tx.PutFindingDisposition(ctx, fixed)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = storetest.Open(t, dbPath, store.Options{})

	got, err := (&productionPublicationWorkflow{store: st}).reviewYieldHistory(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ReviewYieldHistory{
		Rounds: []domain.ReviewYieldRound{
			{Round: 1, FindingsIngested: 1, NewFindings: 1, Fixed: 1, Outcome: domain.ReviewFindings},
			{Round: 2, FindingsIngested: 2, NewFindings: 1, RecurringFindings: 1, Outcome: domain.ReviewFindings},
			{Round: 3, Outcome: domain.ReviewClean},
		},
		TerminalOutcome: domain.ReviewClean,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history after restart = %#v, want %#v", got, want)
	}
}
