package engine

import (
	"context"
	"errors"
	mrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type scriptedFindingAdjudicator struct {
	entries []domain.FindingAdjudicationEntry
	err     error
	calls   int
}

func (f *scriptedFindingAdjudicator) Adjudicate(
	_ context.Context, _ findingAdjudicationRequest,
) ([]domain.FindingAdjudicationEntry, error) {
	f.calls++
	return append([]domain.FindingAdjudicationEntry(nil), f.entries...), f.err
}

type findingAdjudicationFixture struct {
	ctx      context.Context
	store    *store.Store
	workflow *productionPublicationWorkflow
	task     productionPublicationTask
	binding  productionBinding
	record   domain.ReviewRecord
	finding  domain.Finding
	baseRoot string
	headRoot string
}

func adjudicationDigest(seed string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(seed, 64))
}

func newFindingAdjudicationFixture(
	t *testing.T, severity domain.FindingSeverity, location *domain.FindingLocation,
	materiality, confidence string,
) *findingAdjudicationFixture {
	return newFindingAdjudicationFixtureWithNote(
		t, severity, location, materiality, confidence, "producer=fake/test; fixture")
}

func newFindingAdjudicationFixtureWithNote(
	t *testing.T, severity domain.FindingSeverity, location *domain.FindingLocation,
	materiality, confidence, note string,
) *findingAdjudicationFixture {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runID := domain.RunID("run-adjudication")
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{
		{Key: "paths", Value: "daemon/**", Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset, Digest: adjudicationDigest("a"),
		}},
		{Key: findingConfidenceThresholdKey, Value: "high", Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset, Digest: adjudicationDigest("b"),
		}},
		{Key: findingMaterialityThresholdKey, Value: "high", Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset, Digest: adjudicationDigest("c"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: runID, ProjectID: "project-adjudication",
		SpecDigest: adjudicationDigest("d"), PolicyDigest: policy.Digest,
	}
	createdAt := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	finding := domain.Finding{
		ID: "finding-a", RunID: runID, Source: "codex_local", Severity: severity,
		Location: location, Message: "review finding", RawText: "review finding",
		CreatedAt: createdAt,
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-a", RunID: runID, Round: 1,
		Provider: "codex", ModelConfiguration: "test",
		ConfigurationDigest: adjudicationDigest("e"),
		InstructionDigest:   adjudicationDigest("f"),
		CostOwner:           "test", BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
		CompletedAt: createdAt, CompletionEvidence: adjudicationDigest("9"),
		Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		return tx.PutClassification(ctx, domain.Classification{
			FindingID: finding.ID, Version: 1,
			Materiality: materiality, Confidence: confidence, Note: note,
		})
	}); err != nil {
		t.Fatal(err)
	}
	baseRoot := filepath.Join(dir, "base")
	headRoot := filepath.Join(dir, "candidate")
	for _, root := range []string{baseRoot, headRoot} {
		if err := os.MkdirAll(filepath.Join(root, "daemon"), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	now := createdAt.Add(time.Minute)
	advisoryStore, err := advisory.Open(
		filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	limits := inference.Limits{
		Calls: 10, ComputeUnits: 100_000, AttentionItems: 10, Starvation: time.Hour,
	}
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(dir, "inference.json"),
		Binding:   inference.Binding{Provider: "fake", Model: "test"},
		Sites: []inference.Site{inference.ClassifierSite(inference.Budget{
			Window: time.Hour, Site: limits, Project: limits, Global: limits,
			MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
		})},
		Advisory: advisoryStore, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	workflow := &productionPublicationWorkflow{
		store: st, attention: signet.NewService(st), now: func() time.Time { return now },
		inference: client,
	}
	return &findingAdjudicationFixture{
		ctx: ctx, store: st, workflow: workflow,
		task: productionPublicationTask{
			RunID: runID, ProjectID: run.ProjectID, HeadSHA: record.HeadSHA,
		},
		binding: productionBinding{run: run, resolvedPolicy: policy}, record: record,
		finding: finding, baseRoot: baseRoot, headRoot: headRoot,
	}
}

func (f *findingAdjudicationFixture) writePath(t *testing.T, root string) {
	t.Helper()
	if f.finding.Location == nil {
		t.Fatal("fixture has no location")
	}
	path := filepath.Join(root, filepath.FromSlash(f.finding.Location.Path))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func modelRouteEntry(
	t *testing.T, findingID domain.FindingID, route domain.AdjudicationRoute,
	confidence domain.AdjudicationConfidence,
) domain.FindingAdjudicationEntry {
	t.Helper()
	goal := domain.GoalRequired
	var compatibility *domain.ProposedCompatibility
	switch route {
	case domain.RouteParkRevision:
		value := domain.ProposedWorkUnitRevision
		compatibility = &value
	case domain.RouteParkSeparateWork:
		value := domain.ProposedSeparateWork
		compatibility = &value
	case domain.RouteAttentionHumanDecision:
		value := domain.ProposedHumanDecision
		compatibility = &value
	case domain.RouteParkUnknown:
		value := domain.ProposedUnknown
		compatibility = &value
	case domain.RouteDefer:
		goal = domain.GoalAdjacent
	case domain.RouteDecline, domain.RouteDispute:
		goal = domain.GoalContradictory
	case domain.RouteAttentionUnclear:
		goal = domain.GoalUnclear
	case domain.RouteRemediate:
		t.Fatal("remediate is engine-only")
	}
	entry, err := domain.NewModelAdjudicationEntry(
		findingID, goal, compatibility, route, confidence,
		"deterministic fake route", nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestResolvedFindingThresholdDefaultsFailClosed(t *testing.T) {
	if got := resolvedFindingThreshold(domain.ResolvedPolicy{}, findingConfidenceThresholdKey); got != domain.DispatchThresholdHigh {
		t.Fatalf("absent threshold = %q, want high", got)
	}
	invalid := domain.ResolvedPolicy{Keys: []domain.PolicyKey{{
		Key: findingConfidenceThresholdKey, Value: "low",
	}}}
	if got := resolvedFindingThreshold(invalid, findingConfidenceThresholdKey); got != domain.DispatchThresholdHigh {
		t.Fatalf("invalid threshold = %q, want high", got)
	}
	medium := domain.ResolvedPolicy{Keys: []domain.PolicyKey{{
		Key: findingConfidenceThresholdKey, Value: "medium",
	}}}
	if got := resolvedFindingThreshold(medium, findingConfidenceThresholdKey); got != domain.DispatchThresholdMedium {
		t.Fatalf("medium threshold = %q, want medium", got)
	}
}

func TestFindingAdjudicationRoutesEveryTableRow(t *testing.T) {
	for _, route := range []domain.AdjudicationRoute{
		domain.RouteParkRevision, domain.RouteParkSeparateWork,
		domain.RouteAttentionHumanDecision, domain.RouteParkUnknown,
		domain.RouteDefer, domain.RouteDecline, domain.RouteDispute,
		domain.RouteAttentionUnclear,
	} {
		t.Run(string(route), func(t *testing.T) {
			location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
			f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
			f.writePath(t, f.headRoot)
			fake := newDeterministicFindingAdjudicator(
				modelRouteEntry(t, f.finding.ID, route, domain.ConfidenceHigh))
			f.workflow.findingAdjudicator = fake
			state, err := f.workflow.reconcileFindingAdjudication(
				f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
			if err != nil {
				t.Fatal(err)
			}
			var artifact domain.FindingAdjudication
			if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
				var readErr error
				artifact, readErr = tx.GetFindingAdjudicationForRound(f.ctx, f.record.RunID, f.record.Round)
				return readErr
			}); err != nil {
				t.Fatal(err)
			}
			if artifact.Entries[0].Route != route {
				t.Fatalf("artifact route = %q, want %q", artifact.Entries[0].Route, route)
			}
			switch route {
			case domain.RouteDefer, domain.RouteDecline:
				if state != productionReviewPassed {
					t.Fatalf("state = %d, want passed", state)
				}
			case domain.RouteParkRevision, domain.RouteParkSeparateWork,
				domain.RouteAttentionHumanDecision, domain.RouteParkUnknown,
				domain.RouteAttentionUnclear, domain.RouteDispute:
				if state != productionReviewPending {
					t.Fatalf("state = %d, want pending", state)
				}
			case domain.RouteRemediate:
				t.Fatal("model route table unexpectedly included remediate")
			}
		})
	}
}

func TestFindingAdjudicationFastPathResolvesEitherTreeSide(t *testing.T) {
	for _, side := range []string{"candidate-added", "candidate-deleted"} {
		t.Run(side, func(t *testing.T) {
			location := &domain.FindingLocation{Path: "daemon/a.go"}
			f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "high", "high")
			if side == "candidate-added" {
				f.writePath(t, f.headRoot)
			} else {
				f.writePath(t, f.baseRoot)
			}
			fake := &scriptedFindingAdjudicator{err: errors.New("must not run")}
			f.workflow.findingAdjudicator = fake
			state, err := f.workflow.reconcileFindingAdjudication(
				f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
			if err != nil || state != productionReviewPending {
				t.Fatalf("fast path = %d, %v", state, err)
			}
			if fake.calls != 0 {
				t.Fatalf("adjudicator calls = %d, want 0", fake.calls)
			}
			if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
				artifact, err := tx.GetFindingAdjudicationForRound(f.ctx, f.record.RunID, 1)
				if err != nil {
					return err
				}
				if artifact.Entries[0].Route != domain.RouteRemediate ||
					artifact.Entries[0].Producer != domain.AdjudicationProducerEngine {
					t.Fatalf("fast-path entry = %#v", artifact.Entries[0])
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFindingAdjudicationFastPathRequiresRevalidatedClassification(t *testing.T) {
	for _, tc := range []struct {
		name          string
		note          string
		dropEvaluator bool
	}{
		{name: "malformed producer note", note: "unlabeled fixture"},
		{name: "evaluator unavailable", note: "producer=fake/test; fixture", dropEvaluator: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			location := &domain.FindingLocation{Path: "daemon/a.go"}
			f := newFindingAdjudicationFixtureWithNote(
				t, domain.FindingSeverityP2, location, "high", "high", tc.note)
			f.writePath(t, f.headRoot)
			if tc.dropEvaluator {
				f.workflow.inference = nil
			}
			f.workflow.findingAdjudicator = newDeterministicFindingAdjudicator(
				modelRouteEntry(t, f.finding.ID, domain.RouteDefer, domain.ConfidenceHigh))
			state, err := f.workflow.reconcileFindingAdjudication(
				f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
			if err != nil || state != productionReviewPassed {
				t.Fatalf("untrusted classification adjudication = %d, %v", state, err)
			}
			if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
				artifact, err := tx.GetFindingAdjudicationForRound(f.ctx, f.record.RunID, 1)
				if err != nil {
					return err
				}
				if artifact.Entries[0].Producer != domain.AdjudicationProducerModel ||
					artifact.Entries[0].Route != domain.RouteDefer {
					t.Fatalf("untrusted classification entry = %#v", artifact.Entries[0])
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFindingAdjudicationFastPathDoesNotFollowIntermediateSymlink(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go"}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "high", "high")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "a.go"), []byte("package outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.headRoot, "daemon")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.headRoot, "daemon")); err != nil {
		t.Fatal(err)
	}
	f.workflow.findingAdjudicator = newDeterministicFindingAdjudicator(
		modelRouteEntry(t, f.finding.ID, domain.RouteDefer, domain.ConfidenceHigh))
	state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPassed {
		t.Fatalf("symlink adjudication = %d, %v", state, err)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		artifact, err := tx.GetFindingAdjudicationForRound(f.ctx, f.record.RunID, 1)
		if err != nil {
			return err
		}
		if artifact.Entries[0].Producer != domain.AdjudicationProducerModel ||
			artifact.Entries[0].Route != domain.RouteDefer {
			t.Fatalf("symlink classification entry = %#v", artifact.Entries[0])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFindingAdjudicationRestartsAcrossArtifactBoundary(t *testing.T) {
	for _, side := range AllDurableTransitionSides {
		t.Run(string(side), func(t *testing.T) {
			location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
			f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "high", "high")
			f.writePath(t, f.headRoot)
			injected := false
			f.workflow.transitionHook = func(
				transition DurableTransition, observed DurableTransitionSide,
			) error {
				if !injected && transition == DurableTransitionFindingAdjudication && observed == side {
					injected = true
					return errors.New("injected process loss")
				}
				return nil
			}
			if _, err := f.workflow.reconcileFindingAdjudication(
				f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot,
			); err == nil || !injected {
				t.Fatalf("%s did not interrupt adjudication: %v", side, err)
			}
			var artifactPresent bool
			if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
				_, err := tx.GetFindingAdjudicationForRound(f.ctx, f.record.RunID, 1)
				artifactPresent = err == nil
				if errors.Is(err, store.ErrNotFound) {
					return nil
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if want := side == DurableTransitionAfter; artifactPresent != want {
				t.Fatalf("artifact present after %s crash = %t, want %t", side, artifactPresent, want)
			}
			f.workflow.transitionHook = nil
			state, err := f.workflow.reconcileFindingAdjudication(
				f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
			if err != nil || state != productionReviewPending {
				t.Fatalf("restart after %s = %d, %v", side, state, err)
			}
		})
	}
}

func TestFindingAdjudicationOutsideDeclaredPathsRequiresModelJudgment(t *testing.T) {
	location := &domain.FindingLocation{Path: "app/a.swift", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "high", "high")
	f.writePath(t, f.headRoot)
	f.workflow.findingAdjudicator = newDeterministicFindingAdjudicator(
		modelRouteEntry(t, f.finding.ID, domain.RouteDefer, domain.ConfidenceHigh))
	state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPassed {
		t.Fatalf("outside-scope adjudication = %d, %v", state, err)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		artifact, err := tx.GetFindingAdjudicationForRound(f.ctx, f.record.RunID, 1)
		if err != nil {
			return err
		}
		if artifact.Entries[0].Producer != domain.AdjudicationProducerModel {
			t.Fatalf("outside-scope producer = %q, want model", artifact.Entries[0].Producer)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFindingAdjudicationFailClosedAndNotAcceptedParkWithoutArtifact(t *testing.T) {
	for _, tc := range []struct {
		name        string
		location    *domain.FindingLocation
		materiality string
		adjudicator func(*findingAdjudicationFixture) findingAdjudicator
	}{
		{"missing location", nil, "high", func(*findingAdjudicationFixture) findingAdjudicator { return nil }},
		{"non-path location", &domain.FindingLocation{Path: "../outside.go"}, "high", func(*findingAdjudicationFixture) findingAdjudicator { return nil }},
		{"unresolvable location", &domain.FindingLocation{Path: "daemon/missing.go"}, "high", func(*findingAdjudicationFixture) findingAdjudicator { return nil }},
		{"missing output", &domain.FindingLocation{Path: "daemon/a.go"}, "low", func(*findingAdjudicationFixture) findingAdjudicator {
			return &scriptedFindingAdjudicator{}
		}},
		{"low-confidence output", &domain.FindingLocation{Path: "daemon/a.go"}, "low", func(f *findingAdjudicationFixture) findingAdjudicator {
			return &scriptedFindingAdjudicator{entries: []domain.FindingAdjudicationEntry{
				modelRouteEntry(t, f.finding.ID, domain.RouteDefer, domain.ConfidenceMedium),
			}}
		}},
		{"malformed output", &domain.FindingLocation{Path: "daemon/a.go"}, "low", func(f *findingAdjudicationFixture) findingAdjudicator {
			return &scriptedFindingAdjudicator{entries: []domain.FindingAdjudicationEntry{{
				FindingID: f.finding.ID, Producer: domain.AdjudicationProducerModel,
				GoalRelationship: domain.GoalAdjacent, Route: domain.RouteDefer,
				Rationale: "missing required confidence",
			}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, tc.location, tc.materiality, "high")
			if tc.location != nil && tc.location.Path == "daemon/a.go" {
				f.writePath(t, f.headRoot)
			}
			f.workflow.findingAdjudicator = tc.adjudicator(f)
			state, err := f.workflow.reconcileFindingAdjudication(
				f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
			if err != nil || state != productionReviewPending {
				t.Fatalf("park = %d, %v", state, err)
			}
			if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
				if _, err := tx.GetFindingAdjudicationForRound(f.ctx, f.record.RunID, 1); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("artifact after not-accepted batch = %v", err)
				}
				item, err := tx.GetAttentionItem(f.ctx, productionReviewItemID(f.task.RunID, 1))
				if err != nil {
					return err
				}
				if item.Type != domain.AttentionReviewDispute ||
					!item.Offers(domain.ActionDiscuss) || !item.Offers(domain.ActionStop) {
					t.Fatalf("fallback item = %#v", item)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFindingAdjudicationCriticalFinalDispositionRequiresAttention(t *testing.T) {
	for _, route := range []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer} {
		t.Run(string(route), func(t *testing.T) {
			location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
			f := newFindingAdjudicationFixture(t, domain.FindingSeverityP0, location, "low", "high")
			f.writePath(t, f.headRoot)
			f.workflow.findingAdjudicator = &scriptedFindingAdjudicator{entries: []domain.FindingAdjudicationEntry{
				modelRouteEntry(t, f.finding.ID, route, domain.ConfidenceHigh),
			}}
			state, err := f.workflow.reconcileFindingAdjudication(
				f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
			if err != nil || state != productionReviewPending {
				t.Fatalf("critical %s = %d, %v", route, state, err)
			}
			if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
				if _, err := tx.GetFindingDisposition(f.ctx, f.finding.ID, 1); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("critical %s disposition = %v", route, err)
				}
				item, err := tx.GetAttentionItem(f.ctx, productionReviewItemID(f.task.RunID, 1))
				if err != nil {
					return err
				}
				if item.Type != domain.AttentionReviewDispute {
					t.Fatalf("critical item type = %q", item.Type)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFindingAdjudicationDisputeAttentionRemainsParkedOnReplay(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP0, location, "low", "high")
	f.writePath(t, f.headRoot)
	f.workflow.findingAdjudicator = newDeterministicFindingAdjudicator(
		modelRouteEntry(t, f.finding.ID, domain.RouteDecline, domain.ConfidenceHigh))
	for pass := 1; pass <= 2; pass++ {
		state, err := f.workflow.reconcileFindingAdjudication(
			f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
		if err != nil || state != productionReviewPending {
			t.Fatalf("dispute replay pass %d = %d, %v", pass, state, err)
		}
	}
}

func TestFindingAdjudicationDispositionWriteConvergesAfterCrash(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
	f.writePath(t, f.headRoot)
	f.workflow.findingAdjudicator = newDeterministicFindingAdjudicator(
		modelRouteEntry(t, f.finding.ID, domain.RouteDefer, domain.ConfidenceHigh))
	afterCount := 0
	f.workflow.transitionHook = func(
		transition DurableTransition, side DurableTransitionSide,
	) error {
		if transition == DurableTransitionFindingAdjudication && side == DurableTransitionAfter {
			afterCount++
			if afterCount == 2 {
				return errors.New("injected process loss after disposition commit")
			}
		}
		return nil
	}
	if _, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot,
	); err == nil {
		t.Fatal("disposition after-hook did not interrupt reconciliation")
	}
	f.workflow.transitionHook = nil
	previousNow := f.workflow.now()
	f.workflow.now = func() time.Time { return previousNow.Add(time.Hour) }
	state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPassed {
		t.Fatalf("disposition restart = %d, %v", state, err)
	}
}

func TestDispositionAwareReviewSatisfiesReadinessWithoutRereview(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
	f.writePath(t, f.headRoot)
	f.workflow.findingAdjudicator = &scriptedFindingAdjudicator{entries: []domain.FindingAdjudicationEntry{
		modelRouteEntry(t, f.finding.ID, domain.RouteDefer, domain.ConfidenceHigh),
	}}
	f.workflow.reviewConfigurationDigest = f.record.ConfigurationDigest
	f.binding.profile.Review = domain.ReviewSettings{
		Mode: domain.ReviewFreesideInvoked, ConfigDigest: f.record.ConfigurationDigest,
	}
	f.binding.admission.Base = domain.BaseRevision{
		Repo: "owner/repo", RepositoryID: 1, BaseRef: "main", BaseSHA: f.record.BaseSHA,
	}
	f.binding.image.RecipeDigest = adjudicationDigest("7")
	checkpoint := productionVerificationCheckpoint{Authorization: domain.CandidateAuthorization{
		VerificationOutcome:      domain.VerificationPassed,
		VerificationRecipeDigest: adjudicationDigest("7"),
	}}
	reviewInstructions := exec.ReviewInstructionBinding{ResultDigest: f.record.InstructionDigest}
	if _, _, err := f.workflow.assertReviewedCandidate(
		f.ctx, f.task, f.binding, checkpoint, reviewInstructions,
	); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("undispositioned findings readiness = %v, want fail closed", err)
	}
	state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPassed {
		t.Fatalf("adjudication = %d, %v", state, err)
	}
	verdict, _, err := f.workflow.assertReviewedCandidate(
		f.ctx, f.task, f.binding,
		checkpoint, reviewInstructions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Class == domain.ReadinessBlocked {
		t.Fatalf("disposition-complete verdict = %#v", verdict)
	}
}

func TestFindingAdjudicationStructuredDissentValidation(t *testing.T) {
	for _, kind := range []findingAdjudicationDissentKind{
		findingDissentImportPathRejected, findingDissentRemediatorPushback,
	} {
		if err := validateFindingAdjudicationDissent(findingAdjudicationDissent{
			Kind: kind, FindingIDs: []domain.FindingID{"finding-a"}, Evidence: "boundary rejected daemon/x.go",
		}); err != nil {
			t.Fatalf("valid dissent %q: %v", kind, err)
		}
	}
	if err := validateFindingAdjudicationDissent(findingAdjudicationDissent{
		Kind: "invented", FindingIDs: []domain.FindingID{"finding-a"}, Evidence: "x",
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("invented dissent = %v", err)
	}
}

func TestFindingAdjudicationStructuredDissentForcesModelReentry(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "high", "high")
	f.writePath(t, f.headRoot)
	f.workflow.findingAdjudicator = newDeterministicFindingAdjudicator(
		modelRouteEntry(t, f.finding.ID, domain.RouteDefer, domain.ConfidenceHigh))
	state, err := f.workflow.reenterFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot,
		findingAdjudicationDissent{
			Kind: findingDissentImportPathRejected, FindingIDs: []domain.FindingID{f.finding.ID},
			Evidence: "the import boundary rejected the required fix path",
		},
	)
	if err != nil || state != productionReviewPassed {
		t.Fatalf("dissent reentry = %d, %v", state, err)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		artifact, err := tx.GetFindingAdjudicationForRound(f.ctx, f.record.RunID, 1)
		if err != nil {
			return err
		}
		if artifact.Entries[0].Producer != domain.AdjudicationProducerModel ||
			artifact.Entries[0].Route != domain.RouteDefer {
			t.Fatalf("dissent artifact entry = %#v", artifact.Entries[0])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFindingAdjudicationStageConsumesAuthenticatedCommand(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
	f.writePath(t, f.headRoot)
	f.workflow.findingAdjudicator = newDeterministicFindingAdjudicator(
		modelRouteEntry(t, f.finding.ID, domain.RouteParkRevision, domain.ConfidenceHigh))
	if state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot,
	); err != nil || state != productionReviewPending {
		t.Fatalf("initial park = %d, %v", state, err)
	}
	itemID := productionReviewItemID(f.task.RunID, 1)
	var item domain.AttentionItem
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(f.ctx, itemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "command-accept-park", DeviceID: "device-1",
		ItemID: item.ID, ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
		ArtifactDigests: item.ArtifactDigests,
		Action:          domain.ActionAcceptRecommendedRoute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		if err := tx.PutCommand(f.ctx, command); err != nil {
			return err
		}
		concluded, err := item.WithDecidedAt(f.workflow.now().UTC())
		if err != nil {
			return err
		}
		concluded.Status = domain.StatusResolved
		concluded.ItemVersion++
		return tx.PutAttentionItem(f.ctx, concluded)
	}); err != nil {
		t.Fatal(err)
	}
	hooks := 0
	f.workflow.transitionHook = func(
		transition DurableTransition, _ DurableTransitionSide,
	) error {
		if transition == DurableTransitionFindingAdjudication {
			hooks++
		}
		return nil
	}
	state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPending {
		t.Fatalf("consume command = %d, %v", state, err)
	}
	if hooks != 2 {
		t.Fatalf("route execution hooks = %d, want 2", hooks)
	}
}

func TestFindingAdjudicationDecisionRejectsPayloadOnlyRouteAuthority(t *testing.T) {
	entry := modelRouteEntry(t, "finding-a", domain.RouteDefer, domain.ConfidenceHigh)
	artifact, err := domain.NewFindingAdjudication(
		"run-a", 1, adjudicationDigest("1"), adjudicationDigest("2"),
		adjudicationDigest("3"), []domain.FindingAdjudicationEntry{entry},
		time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "command-forged-alternative", DeviceID: "device-1",
		ItemID: "item-a", ItemVersion: 1, PRHeadSHA: strings.Repeat("a", 40),
		ArtifactDigests: []domain.Digest{artifact.Digest},
		Action:          domain.ActionChooseAlternativeRoute,
		Message:         `[{"finding_id":"finding-a","route":"decline"}]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := findingRoutesFromDecision(artifact, &command); err == nil {
		t.Fatal("payload-only decline route was authorized against adjacent artifact axes")
	}
}

// TestRemediationOversizedInputParksRunWithoutStoppingLane proves the #911
// regression: a remediation candidate whose marshalled remediationInput exceeds
// exec.ProductionMaxInputBytes must not fail the production publication lane. It
// drives the finding-adjudication fast path to RouteRemediate over a git-backed
// candidate checkout whose head commit adds a ~4.5 MiB incompressible blob, so
// the git binary patch (and therefore the JSON input) overflows the 4 MiB
// deliverable limit. prepareRemediationIntent then returns
// ErrRemediationInputUndeliverable, executeFindingAdjudication terminalizes it as
// a durable AttentionExecutionFailure item and returns productionReviewPending
// (never a lane-fatal error). A second reconcile replays through the recorded
// escalation without re-diffing, dispatching a marker, or duplicating the item.
func TestRemediationOversizedInputParksRunWithoutStoppingLane(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go"}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "high", "high")

	// The marshalled input overflows only because the candidate patch carries
	// incompressible bytes; a fixed PRNG seed keeps the fixture deterministic and
	// avoids crypto/rand or wall-clock entropy.
	const oversizedBlobBytes = 4608 << 10 // 4.5 MiB > exec.ProductionMaxInputBytes.
	blob := make([]byte, oversizedBlobBytes)
	rng := mrand.New(mrand.NewSource(1)) //nolint:gosec // G404: deterministic incompressible test fixture, not security-sensitive
	if _, err := rng.Read(blob); err != nil {
		t.Fatal(err)
	}

	// A real git checkout is required: remediationCandidatePatch renders
	// git diff --binary base..head against it. daemon/a.go lands in the head tree
	// so the finding's location resolves as candidate-added within daemon/**, and
	// the oversized blob is the diff that overflows the limit.
	candidateRoot := t.TempDir()
	runRemediationGit(t, candidateRoot, nil, "init", "-q", "-b", "main", "--object-format=sha1")
	if err := os.MkdirAll(filepath.Join(candidateRoot, "daemon"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidateRoot, "daemon", "a.go"),
		[]byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRemediationGit(t, candidateRoot, nil, "add", "daemon/a.go")
	runRemediationGit(t, candidateRoot, nil, "commit", "-q", "-m", "base")
	baseSHA := strings.TrimSpace(string(runRemediationGit(t, candidateRoot, nil, "rev-parse", "HEAD")))
	if err := os.WriteFile(filepath.Join(candidateRoot, "daemon", "blob.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	runRemediationGit(t, candidateRoot, nil, "add", "daemon/blob.bin")
	runRemediationGit(t, candidateRoot, nil, "commit", "-q", "-m", "candidate")
	headSHA := strings.TrimSpace(string(runRemediationGit(t, candidateRoot, nil, "rev-parse", "HEAD")))

	// A non-nil artifact store plus a workDir arms the remediation-preparation
	// branch; the production invocation supplies the single input id
	// prepareRemediationIntent chains onto the remediation invocation.
	f.workflow.artifacts = remediationArtifactStore{}
	f.workflow.workDir = t.TempDir()
	productionInvocation, err := domain.NewAgentInvocation(
		productionInvocationID(f.task.RunID),
		[]domain.ArtifactID{domain.ArtifactID("production-input-" + string(f.task.RunID))}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		return tx.PutAgentInvocation(f.ctx, productionInvocation)
	}); err != nil {
		t.Fatal(err)
	}
	f.task.HeadSHA = headSHA
	f.task.Replay.ObservedBaseSHA = baseSHA

	itemID := productionRemediationUndeliverableItemID(f.task.RunID, f.record.Round)
	markerKey := string(remediationInvocationID(f.task.RunID, f.record.Round))

	countAttentionItems := func() int {
		var items []store.Snapshotted[domain.AttentionItem]
		if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
			var readErr error
			items, readErr = tx.ListAttentionItems(f.ctx)
			return readErr
		}); err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	assertNoDispatchedMarker := func() {
		var markerErr error
		if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
			_, markerErr = tx.GetOutbox(f.ctx, markerKey)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(markerErr, store.ErrNotFound) {
			t.Fatalf("remediation marker lookup = %v, want ErrNotFound (never dispatched)", markerErr)
		}
	}

	state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, candidateRoot)
	if err != nil {
		t.Fatalf("first reconcile err = %v, want nil (lane not fatal)", err)
	}
	if state != productionReviewPending {
		t.Fatalf("first reconcile state = %d, want pending", state)
	}

	var item domain.AttentionItem
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var readErr error
		item, readErr = tx.GetAttentionItem(f.ctx, itemID)
		return readErr
	}); err != nil {
		t.Fatalf("undeliverable attention item = %v, want present", err)
	}
	if item.Type != domain.AttentionExecutionFailure {
		t.Fatalf("attention item type = %q, want %q", item.Type, domain.AttentionExecutionFailure)
	}
	if item.Subject.RunID == nil || *item.Subject.RunID != f.task.RunID {
		t.Fatalf("attention item subject run = %v, want %q", item.Subject.RunID, f.task.RunID)
	}
	if got := countAttentionItems(); got != 1 {
		t.Fatalf("attention items after first reconcile = %d, want 1", got)
	}
	assertNoDispatchedMarker()

	// Replay must short-circuit through the recorded escalation: still pending,
	// still no error, no duplicate item, no dispatched marker.
	replayState, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, candidateRoot)
	if err != nil {
		t.Fatalf("replay reconcile err = %v, want nil", err)
	}
	if replayState != productionReviewPending {
		t.Fatalf("replay reconcile state = %d, want pending", replayState)
	}
	if got := countAttentionItems(); got != 1 {
		t.Fatalf("attention items after replay = %d, want 1 (no duplicate)", got)
	}
	assertNoDispatchedMarker()
}
