package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

type scriptedFindingAdjudicator struct {
	entries  []domain.FindingAdjudicationEntry
	err      error
	calls    int
	requests []findingAdjudicationRequest
}

func (f *scriptedFindingAdjudicator) Adjudicate(
	_ context.Context, request findingAdjudicationRequest,
) ([]domain.FindingAdjudicationEntry, error) {
	f.calls++
	f.requests = append(f.requests, request)
	return append([]domain.FindingAdjudicationEntry(nil), f.entries...), f.err
}

type findingAdjudicationFixture struct {
	ctx       context.Context
	store     *store.Store
	workflow  *productionPublicationWorkflow
	artifacts *findingAdjudicationArtifactStore
	blobs     *signet.BlobStore
	driver    *fake.Driver
	signet    *signet.Service
	task      productionPublicationTask
	binding   productionBinding
	record    domain.ReviewRecord
	finding   domain.Finding
	baseRoot  string
	headRoot  string
}

type findingAdjudicationArtifactStore struct {
	bodies map[domain.Digest][]byte
}

func (s *findingAdjudicationArtifactStore) Put(digest domain.Digest, reader io.Reader) (bool, error) {
	body, err := io.ReadAll(reader)
	if err != nil {
		return false, err
	}
	if domain.Digest(contentaddr.Sum(body)) != digest {
		return false, domain.ErrParentKeyMismatch
	}
	if _, ok := s.bodies[digest]; ok {
		return false, nil
	} else {
		s.bodies[digest] = append([]byte(nil), body...)
		return true, nil
	}
}

func (s *findingAdjudicationArtifactStore) Open(digest domain.Digest) (io.ReadCloser, error) {
	body, ok := s.bodies[digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(body)), nil
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
	st := storetest.Open(t, filepath.Join(dir, "freeside.db"), store.Options{})
	runID := domain.RunID("run-adjudication")
	specification := []byte("approved work-unit specification")
	instructions := []byte("trusted repository instructions")
	specDigest := domain.Digest(contentaddr.Sum(specification))
	instructionDigest := domain.Digest(contentaddr.Sum(instructions))
	artifacts := &findingAdjudicationArtifactStore{bodies: map[domain.Digest][]byte{
		specDigest: specification, instructionDigest: instructions,
	}}
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
		SpecDigest: specDigest, PolicyDigest: policy.Digest,
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
		InstructionDigest:   instructionDigest,
		CostOwner:           "test", BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
		CompletedAt: createdAt, CompletionEvidence: adjudicationDigest("9"),
		Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutDevice(ctx, domain.Device{
			ID: "device-test", DisplayName: "Test operator", Status: domain.DeviceActive, PairedAt: createdAt,
		}); err != nil {
			return err
		}
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
	driver := fake.New()
	budget := inference.Budget{
		Window: time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
	}
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(dir, "inference.json"),
		Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: driver},
		Sites: []inference.Site{
			inference.ClassifierSite(budget), inference.AdjudicatorSite(budget),
		},
		Advisory: advisoryStore, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := signet.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	signetService := signet.NewService(st, signet.WithBlobStore(blobs), signet.WithClock(func() time.Time { return now }))
	workflow := &productionPublicationWorkflow{
		store: st, attention: signetService, signet: signetService,
		now: func() time.Time { return now }, inference: client,
	}
	return &findingAdjudicationFixture{
		ctx: ctx, store: st, workflow: workflow, artifacts: artifacts, driver: driver,
		signet: signetService, blobs: blobs,
		task: productionPublicationTask{
			RunID: runID, ProjectID: run.ProjectID, HeadSHA: record.HeadSHA,
		},
		binding: productionBinding{run: run, resolvedPolicy: policy}, record: record,
		finding: finding, baseRoot: baseRoot, headRoot: headRoot,
	}
}

func putFindingAdjudicationRecommendationCase(
	t *testing.T,
	f *findingAdjudicationFixture,
	commitment func(domain.Digest) domain.Digest,
	mutateSource func(*domain.RecommendationSourceRecord),
) domain.AttentionItem {
	t.Helper()
	entries := []domain.FindingAdjudicationEntry{
		modelRouteEntry(t, f.finding.ID, domain.RouteParkRevision, domain.ConfidenceHigh),
	}
	surfaceDigest, err := prospectiveFindingAdjudicationSurfaceDigest(
		f.task, f.record.Round, 1, entries)
	if err != nil {
		t.Fatal(err)
	}
	artifactCommitment := surfaceDigest
	if commitment != nil {
		artifactCommitment = commitment(surfaceDigest)
	}
	artifact, err := domain.NewFindingAdjudication(
		f.record.RunID, f.record.Round, f.binding.run.SpecDigest, f.record.InstructionDigest,
		f.binding.resolvedPolicy.Digest, entries, artifactCommitment, f.workflow.now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	names, err := displayNames(t.Context(), f.workflow.store, f.task.ProjectID,
		findingAdjudicationSurfaceItem(f.task, artifact.Round, artifact.Revision, artifact.Entries).Subject)
	if err != nil {
		t.Fatal(err)
	}
	item, err := f.workflow.newFindingAdjudicationAttentionItem(
		f.task, artifact, map[domain.FindingID]domain.Finding{f.finding.ID: f.finding}, names)
	if err != nil {
		t.Fatal(err)
	}
	source, err := findingAdjudicationRecommendationSource(item, f.record)
	if err != nil {
		t.Fatal(err)
	}
	if mutateSource != nil {
		mutateSource(&source)
		source, err = domain.NewRecommendationSourceRecord(source)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingAdjudication(f.ctx, artifact); err != nil {
			return err
		}
		if err := tx.PutRecommendationSource(f.ctx, source); err != nil {
			return err
		}
		return tx.PutAttentionItem(f.ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func assertFindingAdjudicationRecommendationAbsent(
	t *testing.T, f *findingAdjudicationFixture, itemID domain.ItemID, wantSources int,
) {
	t.Helper()
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(f.ctx, itemID)
		if err != nil {
			return err
		}
		if item.Status != domain.StatusOpen || item.Recommendation != nil {
			t.Fatalf("rejected recommendation item = status %q, recommendation %#v",
				item.Status, item.Recommendation)
		}
		decidingItem, command, err := tx.FindingAdjudicationDecision(f.ctx, itemID)
		if err != nil {
			return err
		}
		if decidingItem.Status != domain.StatusOpen || command != nil {
			t.Fatalf("rejected recommendation decision = status %q, command %#v",
				decidingItem.Status, command)
		}
		sources, err := tx.ListRecommendationSources(f.ctx, itemID)
		if err != nil {
			return err
		}
		if len(sources) != wantSources {
			t.Fatalf("recommendation sources = %d, want %d", len(sources), wantSources)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionFindingAdjudicatorUsesBoundBodiesAndEngineFacts(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
	f.writePath(t, f.headRoot)
	f.driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output:       []byte(`{"entries":[{"finding_id":"finding-a","goal_relationship":"adjacent","compatibility":null,"route":"defer","confidence":"high","rationale":"outside the accepted outcome","evidence":["daemon/a.go:1"],"cited_rules":["declared work-unit scope"],"assumptions":[],"alternatives":["revise the work unit"],"open_questions":[]}]}`),
		ComputeUnits: 5,
	}})
	f.workflow.findingAdjudicator = &productionFindingAdjudicator{
		client: f.workflow.inference, store: f.store, artifacts: f.artifacts,
	}
	state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPassed {
		t.Fatalf("production inference adjudication = %d, %v", state, err)
	}
	requests := f.driver.Requests()
	if len(requests) != 1 || requests[0].SiteID != inference.AdjudicatorSiteID {
		t.Fatalf("requests = %#v", requests)
	}
	fields := requests[0].Fields
	if fields["approved_spec"] != "approved work-unit specification" ||
		fields["instruction_snapshot"] != "trusted repository instructions" ||
		fields["approved_spec_digest"] != string(f.binding.run.SpecDigest) ||
		fields["instruction_snapshot_digest"] != string(f.record.InstructionDigest) ||
		fields["resolved_policy_digest"] != string(f.binding.resolvedPolicy.Digest) {
		t.Fatalf("version-bound fields = %#v", fields)
	}
	if !strings.Contains(fields["findings"], `"remediation_surface":"daemon/a.go"`) ||
		!strings.Contains(fields["findings"], `"compatibility":"allowed"`) ||
		!strings.Contains(fields["findings"], `"version":1`) ||
		fields["prior_disposition_history"] != "[]" || fields["prior_adjudication"] != "null" ||
		fields["dissent"] != "null" {
		t.Fatalf("engine-derived fields = %#v", fields)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		artifact, err := tx.GetFindingAdjudicationForRound(f.ctx, f.record.RunID, f.record.Round)
		if err != nil {
			return err
		}
		if len(artifact.Entries) != 1 || artifact.Entries[0].Route != domain.RouteDefer ||
			artifact.Entries[0].Producer != domain.AdjudicationProducerModel {
			t.Fatalf("artifact entries = %#v", artifact.Entries)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionFindingAdjudicatorRetainsEngineAllowedRemediation(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
	f.writePath(t, f.headRoot)
	f.driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output:       []byte(`{"entries":[{"finding_id":"finding-a","goal_relationship":"required","compatibility":null,"route":null,"confidence":"high","rationale":"the approved outcome requires the contained fix","evidence":["daemon/a.go:1"],"cited_rules":["declared work-unit scope"],"assumptions":[],"alternatives":[],"open_questions":[]}]}`),
		ComputeUnits: 5,
	}})
	f.workflow.findingAdjudicator = &productionFindingAdjudicator{
		client: f.workflow.inference, store: f.store, artifacts: f.artifacts,
	}
	state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPending {
		t.Fatalf("production inference adjudication = %d, %v", state, err)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		artifact, err := tx.GetFindingAdjudicationForRound(f.ctx, f.record.RunID, f.record.Round)
		if err != nil {
			return err
		}
		entry := artifact.Entries[0]
		if entry.Producer != domain.AdjudicationProducerEngineModel ||
			entry.Compatibility == nil || *entry.Compatibility != domain.CompatibilityAllowed ||
			entry.Route != domain.RouteRemediate || entry.Confidence == nil ||
			*entry.Confidence != domain.ConfidenceHigh {
			t.Fatalf("artifact entry = %#v", entry)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFindingAdjudicationProducesAuthenticatedRecommendation(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
	f.writePath(t, f.headRoot)
	f.workflow.findingAdjudicator = &scriptedFindingAdjudicator{entries: []domain.FindingAdjudicationEntry{
		modelRouteEntry(t, f.finding.ID, domain.RouteParkRevision, domain.ConfidenceHigh),
	}}
	state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPending {
		t.Fatalf("finding adjudication = %d, %v", state, err)
	}
	itemID := productionFindingAdjudicationItemID(f.task.RunID, f.record.Round, 1)
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(f.ctx, itemID)
		if err != nil {
			return err
		}
		artifact, err := tx.GetFindingAdjudication(
			f.ctx, item.FindingAdjudication.AdjudicationDigest)
		if err != nil {
			return err
		}
		sources, err := tx.ListRecommendationSources(f.ctx, itemID)
		if err != nil {
			return err
		}
		if len(sources) != 1 {
			t.Fatalf("recommendation sources = %d, want 1", len(sources))
		}
		source := sources[0]
		recommendation := item.Recommendation
		if recommendation == nil || recommendation.Action != domain.ActionAcceptRecommendedRoute ||
			recommendation.Reason != domain.FindingAdjudicatorRecommendationReason ||
			recommendation.Source != domain.RecommendationAgentJudgment || recommendation.Confidence != nil ||
			recommendation.Provenance.AgentJudgment == nil ||
			recommendation.Provenance.AgentJudgment.JudgmentSite != domain.JudgmentSiteFindingAdjudicator ||
			recommendation.Provenance.AgentJudgment.InvocationID != f.record.InvocationID ||
			recommendation.Provenance.AgentJudgment.ArtifactDigest != artifact.Digest {
			t.Fatalf("stored recommendation = %#v", recommendation)
		}
		if source.ItemID != item.ID || source.Provenance.AgentJudgment == nil ||
			source.Provenance.AgentJudgment.InvocationID != f.record.InvocationID ||
			source.Provenance.AgentJudgment.ArtifactDigest != artifact.Digest ||
			source.DecisionSurfaceDigest != item.DecisionSurface.Digest ||
			artifact.DecisionSurfaceDigest != item.DecisionSurface.Digest {
			t.Fatalf("source = %#v, artifact commitment = %q, item surface = %#v",
				source, artifact.DecisionSurfaceDigest, item.DecisionSurface)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFindingAdjudicationRejectsInapplicableRecommendationSources(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	for _, tc := range []struct {
		name         string
		commitment   func(domain.Digest) domain.Digest
		mutateSource func(*domain.RecommendationSourceRecord)
	}{
		{
			name: "foreign artifact commitment",
			commitment: func(domain.Digest) domain.Digest {
				return adjudicationDigest("f")
			},
		},
		{
			name: "foreign review invocation",
			mutateSource: func(source *domain.RecommendationSourceRecord) {
				source.Provenance.AgentJudgment.InvocationID = "review-foreign"
			},
		},
		{
			name: "artifact absent from item binding",
			mutateSource: func(source *domain.RecommendationSourceRecord) {
				source.Provenance.AgentJudgment.ArtifactDigest = adjudicationDigest("e")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFindingAdjudicationFixture(
				t, domain.FindingSeverityP2, location, "low", "high")
			item := putFindingAdjudicationRecommendationCase(
				t, f, tc.commitment, tc.mutateSource)
			assertFindingAdjudicationRecommendationAbsent(t, f, item.ID, 1)
		})
	}

	t.Run("multiple sources", func(t *testing.T) {
		f := newFindingAdjudicationFixture(
			t, domain.FindingSeverityP2, location, "low", "high")
		item := putFindingAdjudicationRecommendationCase(t, f, nil, nil)
		second, err := findingAdjudicationRecommendationSource(item, f.record)
		if err != nil {
			t.Fatal(err)
		}
		second.Reason += " Duplicate record."
		second, err = domain.NewRecommendationSourceRecord(second)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
			return tx.PutRecommendationSource(f.ctx, second)
		}); err != nil {
			t.Fatal(err)
		}
		assertFindingAdjudicationRecommendationAbsent(t, f, item.ID, 2)
	})

	t.Run("older decision surface", func(t *testing.T) {
		f := newFindingAdjudicationFixture(
			t, domain.FindingSeverityP2, location, "low", "high")
		item := putFindingAdjudicationRecommendationCase(t, f, nil, nil)
		item.ItemVersion++
		item.RequestedDecision = append(item.RequestedDecision, domain.ActionDismiss)
		if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
			return tx.PutAttentionItem(f.ctx, item)
		}); err != nil {
			t.Fatal(err)
		}
		assertFindingAdjudicationRecommendationAbsent(t, f, item.ID, 1)
	})
}

func TestFindingAdjudicationDiscussCreatesAuthenticatedSuccessor(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
	f.writePath(t, f.headRoot)
	adjudicator := &scriptedFindingAdjudicator{entries: []domain.FindingAdjudicationEntry{
		modelRouteEntry(t, f.finding.ID, domain.RouteParkRevision, domain.ConfidenceHigh),
	}}
	f.workflow.findingAdjudicator = adjudicator
	dissent := findingAdjudicationDissent{
		Kind: findingDissentRemediatorPushback, FindingIDs: []domain.FindingID{f.finding.ID},
		Evidence: "The remediator reported that the requested change conflicts with the bound instructions.",
	}
	state, err := f.workflow.reenterFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot, dissent)
	if err != nil || state != productionReviewPending {
		t.Fatalf("initial adjudication = %d, %v", state, err)
	}
	f.workflow.artifacts = f.blobs
	attachmentBody := []byte("The cited repository rule is superseded by the bound specification.")
	attachmentDigest := domain.Digest(contentaddr.Sum(attachmentBody))
	if _, err := f.blobs.Put(attachmentDigest, bytes.NewReader(attachmentBody)); err != nil {
		t.Fatal(err)
	}
	initialItemID := productionFindingAdjudicationItemID(f.task.RunID, 1, 1)
	var initial domain.AttentionItem
	var initialSnapshot store.Snapshot
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		initial, initialSnapshot, err = tx.GetAttentionItemSnapshot(f.ctx, initialItemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(f.ctx, signet.ClientCommand{
		CommandID: "revise-adjudication", DeviceID: "device-test",
		ExpectedEntityVersion: initialSnapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: initial.ID, Action: domain.ActionDiscuss, ItemVersion: initial.ItemVersion,
			PRHeadSHA: initial.PRHeadSHA, ArtifactDigests: initial.ArtifactDigests,
			Message:     "The finding belongs in a separate follow-up unit.",
			Attachments: []domain.Digest{attachmentDigest},
		},
	}); err != nil {
		t.Fatal(err)
	}
	adjudicator.entries = []domain.FindingAdjudicationEntry{
		modelRouteEntry(t, f.finding.ID, domain.RouteParkSeparateWork, domain.ConfidenceHigh),
	}
	state, err = f.workflow.reenterFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot, dissent)
	if err != nil || state != productionReviewPending {
		t.Fatalf("revised adjudication = %d, %v", state, err)
	}
	if adjudicator.calls != 2 || len(adjudicator.requests) != 2 ||
		adjudicator.requests[1].Feedback == nil || len(adjudicator.requests[1].PriorEntries) != 1 ||
		adjudicator.requests[1].PriorEntries[0].Route != domain.RouteParkRevision {
		t.Fatalf("adjudicator requests = %#v", adjudicator.requests)
	}
	feedback := adjudicator.requests[1].Feedback
	if feedback.InvocationID != "inv-revise-adjudication" ||
		feedback.ConversationID != domain.ConversationID("conv-"+string(initialItemID)) ||
		feedback.ThroughSequence != 1 || len(feedback.ConversationPrefix) == 0 ||
		adjudicator.requests[1].ApprovedSpecDigest != f.binding.run.SpecDigest ||
		adjudicator.requests[1].InstructionSnapshotDigest != f.record.InstructionDigest ||
		adjudicator.requests[1].ResolvedPolicyDigest != f.binding.resolvedPolicy.Digest {
		t.Fatalf("feedback = %#v", feedback)
	}
	if len(feedback.Attachments) != 1 || feedback.Attachments[0].Digest != attachmentDigest ||
		feedback.Attachments[0].Content != string(attachmentBody) {
		t.Fatalf("materialized feedback attachments = %#v", feedback.Attachments)
	}
	replayedDissent := adjudicator.requests[1].Dissent
	if replayedDissent == nil || replayedDissent.Kind != dissent.Kind ||
		len(replayedDissent.FindingIDs) != 1 || replayedDissent.FindingIDs[0] != f.finding.ID ||
		replayedDissent.Evidence != dissent.Evidence {
		t.Fatalf("replayed dissent = %#v", replayedDissent)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		history, err := tx.ListFindingAdjudications(f.ctx, f.task.RunID)
		if err != nil {
			return err
		}
		if len(history) != 2 || history[1].Revision != 2 ||
			history[1].PredecessorDigest == nil || *history[1].PredecessorDigest != history[0].Digest ||
			history[1].Feedback == nil || history[1].Feedback.PrefixDigest != feedback.PrefixDigest ||
			history[1].Entries[0].Route != domain.RouteParkSeparateWork {
			t.Fatalf("adjudication history = %#v", history)
		}
		oldItem, err := tx.GetAttentionItem(f.ctx, initialItemID)
		if err != nil {
			return err
		}
		newItem, err := tx.GetAttentionItem(
			f.ctx, productionFindingAdjudicationItemID(f.task.RunID, 1, 2))
		if err != nil {
			return err
		}
		if oldItem.Status != domain.StatusSuperseded || newItem.Status != domain.StatusOpen ||
			newItem.ConversationID != nil || newItem.FindingAdjudication == nil ||
			newItem.FindingAdjudication.AdjudicationDigest != history[1].Digest {
			t.Fatalf("successor items = old %#v new %#v", oldItem, newItem)
		}
		if newItem.Recommendation == nil ||
			newItem.Recommendation.Action != domain.ActionAcceptRecommendedRoute ||
			newItem.Recommendation.Provenance.AgentJudgment == nil ||
			newItem.Recommendation.Provenance.AgentJudgment.InvocationID != f.record.InvocationID ||
			newItem.Recommendation.Provenance.AgentJudgment.ArtifactDigest != history[1].Digest ||
			newItem.Recommendation.Confidence != nil ||
			history[1].DecisionSurfaceDigest != newItem.DecisionSurface.Digest {
			t.Fatalf("successor recommendation = %#v, artifact commitment = %q, surface = %#v",
				newItem.Recommendation, history[1].DecisionSurfaceDigest, newItem.DecisionSurface)
		}
		oldSources, err := tx.ListRecommendationSources(f.ctx, oldItem.ID)
		if err != nil {
			return err
		}
		newSources, err := tx.ListRecommendationSources(f.ctx, newItem.ID)
		if err != nil {
			return err
		}
		if len(oldSources) != 1 || len(newSources) != 1 ||
			oldSources[0].ItemID != oldItem.ID || newSources[0].ItemID != newItem.ID ||
			newSources[0].Provenance.AgentJudgment == nil ||
			newSources[0].Provenance.AgentJudgment.ArtifactDigest != history[1].Digest {
			t.Fatalf("successor sources = old %#v, new %#v", oldSources, newSources)
		}
		if _, err := tx.GetFindingDisposition(f.ctx, f.finding.ID, 1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("discussion executed a route: %v", err)
		}
		dispatched, err := tx.ListDispatchedOutbox(f.ctx, string(domain.AgentInvocationRequestedKind))
		if err != nil || len(dispatched) != 1 {
			t.Fatalf("dispatched discussion = %#v, %v", dispatched, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	successorID := productionFindingAdjudicationItemID(f.task.RunID, 1, 2)
	var successor domain.AttentionItem
	var successorSnapshot store.Snapshot
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		successor, successorSnapshot, err = tx.GetAttentionItemSnapshot(f.ctx, successorID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	binaryBody := []byte{0xff, 0xfe}
	binaryDigest := domain.Digest(contentaddr.Sum(binaryBody))
	if _, err := f.blobs.Put(binaryDigest, bytes.NewReader(binaryBody)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(f.ctx, signet.ClientCommand{
		CommandID: "unsupported-adjudication-evidence", DeviceID: "device-test",
		ExpectedEntityVersion: successorSnapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: successor.ID, Action: domain.ActionDiscuss, ItemVersion: successor.ItemVersion,
			PRHeadSHA: successor.PRHeadSHA, ArtifactDigests: successor.ArtifactDigests,
			Message: "Review this opaque evidence.", Attachments: []domain.Digest{binaryDigest},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 2; pass++ {
		state, err = f.workflow.reenterFindingAdjudication(
			f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot, dissent)
		if err != nil || state != productionReviewPending {
			t.Fatalf("unsupported attachment pass %d = %d, %v", pass, state, err)
		}
	}
	if adjudicator.calls != 2 {
		t.Fatalf("unsupported attachment retried adjudicator: calls = %d, want 2", adjudicator.calls)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		pending, err := tx.ListPendingOutbox(f.ctx, string(domain.AgentInvocationRequestedKind))
		if err != nil {
			return err
		}
		for _, entry := range pending {
			if entry.IdempotencyKey == "inv-unsupported-adjudication-evidence" {
				t.Fatalf("unavailable discussion remained pending: %#v", entry)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		successor, successorSnapshot, err = tx.GetAttentionItemSnapshot(f.ctx, successorID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	retryBody := []byte("The relevant evidence is text-only on retry.")
	retryDigest := domain.Digest(contentaddr.Sum(retryBody))
	if _, err := f.blobs.Put(retryDigest, bytes.NewReader(retryBody)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(f.ctx, signet.ClientCommand{
		CommandID: "retry-adjudication-evidence", DeviceID: "device-test",
		ExpectedEntityVersion: successorSnapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: successor.ID, Action: domain.ActionDiscuss, ItemVersion: successor.ItemVersion,
			PRHeadSHA: successor.PRHeadSHA, ArtifactDigests: successor.ArtifactDigests,
			Message: "Use this bounded replacement evidence.", Attachments: []domain.Digest{retryDigest},
		},
	}); err != nil {
		t.Fatal(err)
	}
	state, err = f.workflow.reenterFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot, dissent)
	if err != nil || state != productionReviewPending {
		t.Fatalf("replacement attachment = %d, %v", state, err)
	}
	if adjudicator.calls != 3 || len(adjudicator.requests) != 3 {
		t.Fatalf("replacement attachment calls = %d, requests = %d", adjudicator.calls, len(adjudicator.requests))
	}
	retryFeedback := adjudicator.requests[2].Feedback
	if retryFeedback == nil || len(retryFeedback.Attachments) != 1 ||
		retryFeedback.Attachments[0].Digest != retryDigest ||
		retryFeedback.Attachments[0].Content != string(retryBody) {
		t.Fatalf("replacement feedback = %#v", retryFeedback)
	}
}

func TestFindingAdjudicationDiscussRecoversAcceptedCompletion(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
	f.writePath(t, f.headRoot)
	adjudicator := &scriptedFindingAdjudicator{entries: []domain.FindingAdjudicationEntry{
		modelRouteEntry(t, f.finding.ID, domain.RouteParkRevision, domain.ConfidenceHigh),
	}}
	f.workflow.findingAdjudicator = adjudicator
	if _, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot); err != nil {
		t.Fatal(err)
	}
	f.workflow.artifacts = f.blobs
	itemID := productionFindingAdjudicationItemID(f.task.RunID, 1, 1)
	var item domain.AttentionItem
	var itemSnapshot store.Snapshot
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		item, itemSnapshot, err = tx.GetAttentionItemSnapshot(f.ctx, itemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(f.ctx, signet.ClientCommand{
		CommandID: "recover-adjudication", DeviceID: "device-test", ExpectedEntityVersion: itemSnapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionDiscuss, ItemVersion: item.ItemVersion,
			PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
			Message: "Please reconsider this route.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	adjudicator.entries = []domain.FindingAdjudicationEntry{
		modelRouteEntry(t, f.finding.ID, domain.RouteAttentionHumanDecision, domain.ConfidenceHigh),
	}
	injected := false
	f.workflow.transitionHook = func(transition DurableTransition, side DurableTransitionSide) error {
		if !injected && transition == DurableTransitionFindingAdjudication && side == DurableTransitionBefore {
			injected = true
			return errors.New("injected process loss after accepted completion")
		}
		return nil
	}
	if _, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot); err == nil || !injected {
		t.Fatalf("revision did not interrupt after completion: %v", err)
	}
	if adjudicator.calls != 2 {
		t.Fatalf("adjudicator calls before recovery = %d, want 2", adjudicator.calls)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		item, itemSnapshot, err = tx.GetAttentionItemSnapshot(f.ctx, itemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.signet.Submit(f.ctx, signet.ClientCommand{
		CommandID: "skip-unconsumed-adjudication", DeviceID: "device-test",
		ExpectedEntityVersion: itemSnapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionDiscuss, ItemVersion: item.ItemVersion,
			PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
			Message: "Do not skip the accepted reply.",
		},
	})
	if !errors.Is(err, signet.ErrAgentReplyPending) {
		t.Fatalf("second Discuss before successor = %v, want ErrAgentReplyPending", err)
	}
	f.workflow.transitionHook = nil
	if _, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot); err != nil {
		t.Fatal(err)
	}
	if adjudicator.calls != 2 {
		t.Fatalf("recovery re-invoked model: calls = %d", adjudicator.calls)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		history, err := tx.ListFindingAdjudications(f.ctx, f.task.RunID)
		if err != nil {
			return err
		}
		if len(history) != 2 || history[1].Revision != 2 ||
			history[1].Entries[0].Route != domain.RouteAttentionHumanDecision {
			t.Fatalf("recovered ordered successor = %#v", history)
		}
		intent, err := tx.GetOutbox(f.ctx, "inv-recover-adjudication")
		if err != nil || !intent.Dispatched() {
			t.Fatalf("recovered invocation intent = %#v, %v", intent, err)
		}
		if _, err := tx.GetCommand(f.ctx, "skip-unconsumed-adjudication"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected second Discuss persisted: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFindingAdjudicationDiscussReconsidersEngineEntries(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "high", "high")
	f.writePath(t, f.headRoot)
	allowed := domain.CompatibilityAllowed
	engineEntry, err := domain.NewEngineAdjudicationEntry(
		f.finding.ID, domain.GoalRequired, &allowed, domain.RouteRemediate,
		"engine-derived containment", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := domain.NewFindingAdjudication(
		f.record.RunID, f.record.Round, f.binding.run.SpecDigest, f.record.InstructionDigest,
		f.binding.resolvedPolicy.Digest, []domain.FindingAdjudicationEntry{engineEntry}, "",

		time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	residue, err := f.workflow.findingAdjudicationRevisionInputs(
		f.ctx, f.binding, f.record, prior, f.baseRoot, f.headRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(residue) != 1 || residue[0].Finding.ID != f.finding.ID ||
		residue[0].Compatibility != domain.CompatibilityAllowed {
		t.Fatalf("engine challenge residue = %#v", residue)
	}
}

func TestFindingAdjudicationCompletionResultRevalidatesTrustBoundary(t *testing.T) {
	allowed := domain.CompatibilityAllowed
	engineEntry, err := domain.NewEngineAdjudicationEntry(
		"finding-engine", domain.GoalRequired, &allowed, domain.RouteRemediate,
		"engine-derived containment", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	priorModel := modelRouteEntry(
		t, "finding-model", domain.RouteParkRevision, domain.ConfidenceHigh)
	prior, err := domain.NewFindingAdjudication(
		"run-result", 1, adjudicationDigest("a"), adjudicationDigest("b"), adjudicationDigest("c"),
		[]domain.FindingAdjudicationEntry{engineEntry, priorModel}, "",

		time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	revisedModel := modelRouteEntry(
		t, "finding-model", domain.RouteParkSeparateWork, domain.ConfidenceHigh)
	revisedEngine, err := domain.NewEngineModelAdjudicationEntry(
		engineEntry.FindingID, domain.GoalRequired, domain.ConfidenceHigh,
		"the challenged finding still requires the engine-contained fix", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	valid := []domain.FindingAdjudicationEntry{revisedEngine, revisedModel}
	resultArtifacts := &findingAdjudicationArtifactStore{bodies: make(map[domain.Digest][]byte)}
	workflow := &productionPublicationWorkflow{artifacts: resultArtifacts}
	replyFor := func(result findingAdjudicationResult) domain.Message {
		body, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		digest := domain.Digest(contentaddr.Sum(body))
		if _, putErr := resultArtifacts.Put(digest, bytes.NewReader(body)); putErr != nil {
			t.Fatal(putErr)
		}
		return domain.Message{
			Body:        findingAdjudicationReplySummary(prior, result.Entries),
			Attachments: []domain.Digest{digest},
		}
	}
	validResult := findingAdjudicationResult{
		Version: findingAdjudicationResultVersion, PredecessorDigest: prior.Digest, Entries: valid,
	}
	residue := []findingAdjudicationInput{
		{Finding: domain.Finding{ID: engineEntry.FindingID}, Compatibility: domain.CompatibilityAllowed},
		{Finding: domain.Finding{ID: priorModel.FindingID}, Compatibility: domain.CompatibilityUnknown},
	}
	if got, err := workflow.loadFindingAdjudicationResult(
		prior, replyFor(validResult), residue, domain.DispatchThresholdHigh); err != nil || len(got) != 2 {
		t.Fatalf("valid result = %#v, %v", got, err)
	}
	unavailableResult := findingAdjudicationResult{
		Version: findingAdjudicationResultVersion, PredecessorDigest: prior.Digest,
		Unavailable: true, Entries: []domain.FindingAdjudicationEntry{},
	}
	if _, err := workflow.loadFindingAdjudicationResult(
		prior, replyFor(unavailableResult), residue, domain.DispatchThresholdHigh,
	); !errors.Is(err, inference.ErrAdjudicationNotAvailable) {
		t.Fatalf("unavailable result = %v", err)
	}
	for _, malformed := range []domain.Message{
		{},
		{Attachments: []domain.Digest{adjudicationDigest("d"), adjudicationDigest("e")}},
		{Attachments: []domain.Digest{adjudicationDigest("f")}},
	} {
		if _, err := workflow.loadFindingAdjudicationResult(
			prior, malformed, residue, domain.DispatchThresholdHigh); err == nil {
			t.Fatalf("malformed completion %#v was accepted", malformed)
		}
	}
	lowConfidence := modelRouteEntry(
		t, "finding-model", domain.RouteParkSeparateWork, domain.ConfidenceMedium)
	foreign := modelRouteEntry(
		t, "finding-foreign", domain.RouteParkSeparateWork, domain.ConfidenceHigh)
	for _, tc := range []struct {
		name   string
		result findingAdjudicationResult
	}{
		{name: "foreign predecessor", result: findingAdjudicationResult{
			Version: findingAdjudicationResultVersion, PredecessorDigest: adjudicationDigest("f"), Entries: valid,
		}},
		{name: "unreconsidered engine fact", result: findingAdjudicationResult{
			Version: findingAdjudicationResultVersion, PredecessorDigest: prior.Digest,
			Entries: []domain.FindingAdjudicationEntry{engineEntry, revisedModel},
		}},
		{name: "below threshold", result: findingAdjudicationResult{
			Version: findingAdjudicationResultVersion, PredecessorDigest: prior.Digest,
			Entries: []domain.FindingAdjudicationEntry{revisedEngine, lowConfidence},
		}},
		{name: "foreign finding", result: findingAdjudicationResult{
			Version: findingAdjudicationResultVersion, PredecessorDigest: prior.Digest,
			Entries: []domain.FindingAdjudicationEntry{revisedEngine, foreign},
		}},
		{name: "unavailable result with entries", result: findingAdjudicationResult{
			Version: findingAdjudicationResultVersion, PredecessorDigest: prior.Digest,
			Unavailable: true, Entries: valid,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := workflow.loadFindingAdjudicationResult(
				prior, replyFor(tc.result), residue, domain.DispatchThresholdHigh); err == nil {
				t.Fatal("tampered result was accepted")
			}
		})
	}
	notAllowed := append([]findingAdjudicationInput(nil), residue...)
	notAllowed[0].Compatibility = domain.CompatibilityUnknown
	if _, err := workflow.loadFindingAdjudicationResult(
		prior, replyFor(validResult), notAllowed, domain.DispatchThresholdHigh); err == nil {
		t.Fatal("mixed-origin result without fresh allowed compatibility was accepted")
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
				if item.Recommendation != nil {
					t.Fatalf("fallback recommendation = %#v, want absent", item.Recommendation)
				}
				sources, err := tx.ListRecommendationSources(f.ctx, item.ID)
				if err != nil {
					return err
				}
				if len(sources) != 0 {
					t.Fatalf("fallback recommendation sources = %#v, want none", sources)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFindingAdjudicationFailSafeParkingStopsInferenceRetry(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
	f.writePath(t, f.headRoot)
	adjudicator := &scriptedFindingAdjudicator{err: errors.New("driver unavailable")}
	f.workflow.findingAdjudicator = adjudicator
	for pass := 1; pass <= 2; pass++ {
		state, err := f.workflow.reconcileFindingAdjudication(
			f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
		if err != nil || state != productionReviewPending {
			t.Fatalf("fail-safe replay pass %d = %d, %v", pass, state, err)
		}
	}
	if adjudicator.calls != 1 {
		t.Fatalf("fail-safe parking invoked adjudicator %d times, want 1", adjudicator.calls)
	}
}

func TestFindingAdjudicationAttachmentMaterializationRejectsUntrustedBytes(t *testing.T) {
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, nil, "low", "high")
	body := []byte("trusted attachment")
	digest := domain.Digest(contentaddr.Sum(body))
	f.workflow.artifacts = &findingAdjudicationArtifactStore{bodies: map[domain.Digest][]byte{
		digest: []byte("different bytes"),
	}}
	message, err := domain.NewMessage(
		"msg-attachment", "conv-attachment", domain.AuthorUser, "consider this evidence",
		[]domain.Digest{digest}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	conversation, _ := (domain.Conversation{
		ID: "conv-attachment", Status: domain.ConversationAwaitingAgent,
	}).Append(message)
	if _, err := f.workflow.materializeFindingAdjudicationAttachments(
		conversation, 1); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("untrusted attachment bytes = %v", err)
	}
}

func TestFindingAdjudicationCriticalFinalDispositionRequiresAttention(t *testing.T) {
	for _, severity := range []domain.FindingSeverity{domain.FindingSeverityP0, domain.FindingSeverityP1} {
		for _, route := range []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer} {
			t.Run(string(severity)+"/"+string(route), func(t *testing.T) {
				location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
				f := newFindingAdjudicationFixture(t, severity, location, "low", "high")
				goal := domain.GoalContradictory
				if route == domain.RouteDefer {
					goal = domain.GoalAdjacent
				}
				f.driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
					Output: []byte(fmt.Sprintf(`{"entries":[{"finding_id":"finding-a","goal_relationship":%q,"compatibility":null,"route":%q,"confidence":"high","rationale":"model reduction requires the severity ceiling","evidence":[],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`,
						goal, route)),
					ComputeUnits: 1,
				}})
				f.workflow.findingAdjudicator = &productionFindingAdjudicator{
					client: f.workflow.inference, store: f.store, artifacts: f.artifacts,
				}
				f.writePath(t, f.headRoot)
				state, err := f.workflow.reconcileFindingAdjudication(
					f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
				if err != nil || state != productionReviewPending {
					t.Fatalf("critical %s = %d, %v", route, state, err)
				}
				requests := f.driver.Requests()
				if len(requests) != 1 || requests[0].SiteID != inference.AdjudicatorSiteID {
					t.Fatalf("critical requests = %#v", requests)
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
	readiness, _, err := f.workflow.assertReviewedCandidate(
		f.ctx, f.task, f.binding,
		checkpoint, reviewInstructions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if readiness.verdict.Class == domain.ReadinessBlocked {
		t.Fatalf("disposition-complete verdict = %#v", readiness.verdict)
	}
	// The card-facing detail (issue #982) comes from this same evaluation:
	// bound to the task head and admitted base, listing the production set's
	// two required checks as passed under their proof recipes, in key order.
	detail := readiness.detail
	if detail.EvaluationSetDigest != readiness.verdict.EvaluationSetDigest ||
		detail.CandidateHead != f.task.HeadSHA ||
		detail.Base != (domain.ReadinessBoundBase{
			BaseRef: f.binding.admission.Base.BaseRef, BaseSHA: f.binding.admission.Base.BaseSHA,
		}) || detail.Class() != readiness.verdict.Class {
		t.Fatalf("readiness detail = %#v against verdict %#v", detail, readiness.verdict)
	}
	if len(detail.Requirements) != 2 ||
		detail.Requirements[0].RequirementKey != "clean-verification" ||
		detail.Requirements[0].State != domain.ReadinessRequirementPassed ||
		detail.Requirements[0].ProofRecipeDigest == nil ||
		*detail.Requirements[0].ProofRecipeDigest != f.binding.image.RecipeDigest ||
		detail.Requirements[1].RequirementKey != "independent-review" ||
		detail.Requirements[1].State != domain.ReadinessRequirementPassed ||
		detail.Requirements[1].ProofRecipeDigest == nil ||
		detail.Requirements[0].Waiver != nil || detail.Requirements[1].Waiver != nil {
		t.Fatalf("readiness detail requirements = %#v", detail.Requirements)
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

func TestFindingAdjudicationProjectsAuthenticatedFindingContext(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 3, EndLine: 5}
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
	f.writePath(t, f.headRoot)
	revision := domain.ProposedWorkUnitRevision
	entry, err := domain.NewModelAdjudicationEntry(
		f.finding.ID, domain.GoalRequired, &revision, domain.RouteParkRevision, domain.ConfidenceHigh,
		"the finding requires a work-unit revision",
		[]string{"the changed lines fall outside the declared work-unit paths"},
		nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	f.workflow.findingAdjudicator = newDeterministicFindingAdjudicator(entry)
	if state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot,
	); err != nil || state != productionReviewPending {
		t.Fatalf("adjudication = %d, %v", state, err)
	}
	var item domain.AttentionItem
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var readErr error
		item, readErr = tx.GetAttentionItem(f.ctx, productionReviewItemID(f.task.RunID, 1))
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	// The producer projects the daemon-authenticated coordinates from the stored
	// Finding (message normalized, location as-is) and the evidence from the
	// artifact entry, so the operator sees the finding before the durable route.
	proposal := item.FindingAdjudication.Proposals[0]
	if proposal.FindingMessage != domain.NormalizeFindingMessage(f.finding.Message) {
		t.Fatalf("finding_message = %q, want %q", proposal.FindingMessage, f.finding.Message)
	}
	if proposal.FindingLocation == nil || *proposal.FindingLocation != *location {
		t.Fatalf("finding_location = %#v, want %#v", proposal.FindingLocation, location)
	}
	if !slices.Equal(proposal.Evidence, entry.Evidence) {
		t.Fatalf("evidence = %#v, want %#v", proposal.Evidence, entry.Evidence)
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

func TestFindingAdjudicationRoutesDiminishingReviewActions(t *testing.T) {
	for _, action := range []domain.Action{
		domain.ActionFinishNow, domain.ActionApplyThenFinish, domain.ActionContinueUnderPolicy,
	} {
		t.Run(string(action), func(t *testing.T) {
			location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
			f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, location, "low", "high")
			record, artifact := seedDiminishingReviewRound(t, f)
			producerID := remediationInvocationID(f.task.RunID, record.Round-1)
			summary := summaryClaimFixture(producerID, "One recurring finding remains open.")
			if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
				if err := tx.PutAgentInvocation(f.ctx, domain.AgentInvocation{
					ID: producerID, InputIDs: []domain.ArtifactID{"remediation-summary-input"},
				}); err != nil {
					return err
				}
				return tx.PutAgentClaims(f.ctx, producerID, []domain.AgentClaim{summary})
			}); err != nil {
				t.Fatal(err)
			}

			state, err := f.workflow.executeFindingAdjudication(
				f.ctx, f.task, record, artifact, f.headRoot)
			if err != nil || state != productionReviewPending {
				t.Fatalf("initial diminishing gate = %d, %v", state, err)
			}
			itemID := productionReviewDiminishingItemID(f.task.RunID, record.Round)
			var item domain.AttentionItem
			var itemSnapshot store.Snapshot
			if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
				var readErr error
				item, itemSnapshot, readErr = tx.GetAttentionItemSnapshot(f.ctx, itemID)
				return readErr
			}); err != nil {
				t.Fatal(err)
			}
			if item.YieldHistory == nil || len(item.YieldHistory.Rounds) != 3 {
				t.Fatalf("yield history = %#v", item.YieldHistory)
			}
			if len(item.AgentClaims) != 1 || item.AgentClaims[0].Text == nil ||
				item.AgentClaims[0].Provenance.ProducerInvocationID != producerID {
				t.Fatalf("diminishing summary claim = %+v", item.AgentClaims)
			}
			if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
				dispositions, readErr := tx.ListFindingDispositions(f.ctx, f.task.RunID)
				if readErr != nil {
					return readErr
				}
				if len(dispositions) != 0 {
					t.Fatalf("pre-decision dispositions = %#v", dispositions)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			command := signet.ClientCommand{
				CommandID: "command-" + string(action), DeviceID: "device-test",
				ExpectedEntityVersion: itemSnapshot.EntityVersion,
				Payload: signet.DecisionPayload{
					ItemID: item.ID, Action: action, ItemVersion: item.ItemVersion,
					PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
				},
			}
			if _, err := f.signet.Submit(f.ctx, command); err != nil {
				t.Fatal(err)
			}

			state, err = f.workflow.executeFindingAdjudication(
				f.ctx, f.task, record, artifact, f.headRoot)
			wantState := productionReviewContinue
			if action == domain.ActionFinishNow {
				wantState = productionReviewPassed
			}
			if err != nil || state != wantState {
				t.Fatalf("decided diminishing gate = %d, %v, want %d", state, err, wantState)
			}
			replayState, err := f.workflow.reconcileFindingAdjudication(
				f.ctx, f.task, f.binding, record, f.baseRoot, f.headRoot)
			if err != nil || replayState != wantState {
				t.Fatalf("reconstructed diminishing gate = %d, %v, want %d",
					replayState, err, wantState)
			}
			if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
				dispositions, readErr := tx.ListFindingDispositions(f.ctx, f.task.RunID)
				if readErr != nil {
					return readErr
				}
				if len(dispositions) != 1 || dispositions[0].FindingID != record.FindingIDs[0] ||
					dispositions[0].Disposition != domain.ReviewDispositionDeferred ||
					dispositions[0].AdjudicationDigest != artifact.Digest {
					t.Fatalf("post-decision dispositions = %#v", dispositions)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if action == domain.ActionApplyThenFinish {
				assertFinalReviewFindingsCanFinish(t, f, record)
			}
		})
	}
}

func assertFinalReviewFindingsCanFinish(
	t *testing.T, f *findingAdjudicationFixture, prior domain.ReviewRecord,
) {
	t.Helper()
	finding := f.finding
	finding.ID = "finding-final-review"
	finding.Message = "finding discovered by final review"
	finding.RawText = finding.Message
	finding.CreatedAt = prior.CompletedAt.Add(time.Minute)
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-final", RunID: f.task.RunID, Round: prior.Round + 1,
		Provider: prior.Provider, ModelConfiguration: prior.ModelConfiguration,
		ConfigurationDigest: prior.ConfigurationDigest,
		InstructionDigest:   prior.InstructionDigest,
		CostOwner:           prior.CostOwner,
		BaseSHA:             prior.BaseSHA,
		HeadSHA:             prior.HeadSHA,
		CompletedAt:         finding.CreatedAt,
		CompletionEvidence:  adjudicationDigest("f"),
		Outcome:             domain.ReviewFindings,
		FindingIDs:          []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := domain.NewFindingAdjudication(
		f.task.RunID, record.Round, f.binding.run.SpecDigest,
		record.InstructionDigest, f.binding.resolvedPolicy.Digest,
		[]domain.FindingAdjudicationEntry{
			modelRouteEntry(t, finding.ID, domain.RouteDefer, domain.ConfidenceHigh),
		}, "",

		record.CompletedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(f.ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		return tx.PutFindingAdjudication(f.ctx, artifact)
	}); err != nil {
		t.Fatal(err)
	}
	state, err := f.workflow.executeFindingAdjudication(
		f.ctx, f.task, record, artifact, f.headRoot)
	if err != nil || state != productionReviewPending {
		t.Fatalf("final findings gate = %d, %v", state, err)
	}
	itemID := productionReviewDiminishingItemID(f.task.RunID, record.Round)
	var item domain.AttentionItem
	var snapshot store.Snapshot
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var readErr error
		item, snapshot, readErr = tx.GetAttentionItemSnapshot(f.ctx, itemID)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(f.ctx, signet.ClientCommand{
		CommandID: "command-finish-final-review", DeviceID: "device-test",
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionFinishNow, ItemVersion: item.ItemVersion,
			PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatal(err)
	}
	state, err = f.workflow.executeFindingAdjudication(
		f.ctx, f.task, record, artifact, f.headRoot)
	if err != nil || state != productionReviewPassed {
		t.Fatalf("finish final findings = %d, %v", state, err)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		if _, err := tx.ReviewDiminishingDecision(
			f.ctx, productionReviewDiminishingItemID(f.task.RunID, prior.Round),
		); err != nil {
			return err
		}
		if _, err := tx.ReviewDiminishingDecision(f.ctx, itemID); err != nil {
			return err
		}
		dispositions, err := tx.ListFindingDispositions(f.ctx, f.task.RunID)
		if err != nil {
			return err
		}
		if len(dispositions) != 2 || dispositions[1].FindingID != finding.ID ||
			dispositions[1].Disposition != domain.ReviewDispositionDeferred {
			t.Fatalf("final dispositions = %#v", dispositions)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func seedDiminishingReviewRound(
	t *testing.T, f *findingAdjudicationFixture,
) (domain.ReviewRecord, domain.FindingAdjudication) {
	t.Helper()
	findings := make([]domain.Finding, 0, 2)
	records := make([]domain.ReviewRecord, 0, 2)
	for round := 2; round <= 3; round++ {
		finding := f.finding
		finding.ID = domain.FindingID(fmt.Sprintf("finding-recurring-%d", round))
		finding.CreatedAt = f.finding.CreatedAt.Add(time.Duration(round) * time.Minute)
		record, err := domain.NewReviewRecord(domain.ReviewRecord{
			InvocationID: domain.InvocationID(fmt.Sprintf("review-recurring-%d", round)),
			RunID:        f.task.RunID, Round: round, Provider: f.record.Provider,
			ModelConfiguration:  f.record.ModelConfiguration,
			ConfigurationDigest: f.record.ConfigurationDigest,
			InstructionDigest:   f.record.InstructionDigest, CostOwner: f.record.CostOwner,
			BaseSHA: f.record.BaseSHA, HeadSHA: f.record.HeadSHA,
			CompletedAt:        f.record.CompletedAt.Add(time.Duration(round) * time.Minute),
			CompletionEvidence: adjudicationDigest(fmt.Sprintf("%d", round)),
			Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		findings = append(findings, finding)
		records = append(records, record)
	}
	current := records[len(records)-1]
	entry := modelRouteEntry(t, current.FindingIDs[0], domain.RouteDefer, domain.ConfidenceHigh)
	artifact, err := domain.NewFindingAdjudication(
		f.task.RunID, current.Round, f.binding.run.SpecDigest,
		current.InstructionDigest, f.binding.resolvedPolicy.Digest,
		[]domain.FindingAdjudicationEntry{entry}, "",

		current.CompletedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		for index, record := range records {
			if err := tx.PutReviewRecord(f.ctx, record, []domain.Finding{findings[index]}); err != nil {
				return err
			}
		}
		return tx.PutFindingAdjudication(f.ctx, artifact)
	}); err != nil {
		t.Fatal(err)
	}
	return current, artifact
}

func TestFindingAdjudicationDecisionRejectsPayloadOnlyRouteAuthority(t *testing.T) {
	entry := modelRouteEntry(t, "finding-a", domain.RouteDefer, domain.ConfidenceHigh)
	artifact, err := domain.NewFindingAdjudication(
		"run-a", 1, adjudicationDigest("1"), adjudicationDigest("2"),
		adjudicationDigest("3"), []domain.FindingAdjudicationEntry{entry}, "",

		time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC))
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
