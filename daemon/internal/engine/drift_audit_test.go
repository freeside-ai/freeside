package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	driftAuditMediumConfidence = `{"verdict":"over_hardened","confidence":"medium","reversals":[{"finding_id":"finding-1","undo":"remove the retry wrapper around the config read","rationale":"the specification reads the config once at start"}],"explanation":"review added defensive work the specification does not need"}`
	// driftAuditBatchReversal cites the audited round's own finding, which the
	// site accepts (the round's adjudication names it) and the route gate
	// refuses.
	driftAuditBatchReversal = `{"verdict":"over_hardened","confidence":"high","reversals":[{"finding_id":"finding-b","undo":"drop the new guard","rationale":"the specification does not need it"}],"explanation":"review added defensive work the specification does not need"}`
)

type driftAuditFixtureOptions struct {
	// policy is merged over the default, which turns the audit on from round 2.
	policy map[string]string
	// earlierSeverity is the severity of the round-1 finding whose fix the
	// scripted audit reverses. The zero value is P2.
	earlierSeverity domain.FindingSeverity
	// batchRoute is the audited round's one route. The zero value is the
	// engine's remediate route; any other leaves the batch without a routed fix.
	batchRoute domain.AdjudicationRoute
	// recur makes the audited round's finding recur the fixed round-1 finding,
	// so the round stops on fixed_recurrence before any audit.
	recur bool
	// earlierOverHardened stores an over_hardened audit for round 1.
	earlierOverHardened bool
	// noRoundOneInput leaves out round 1's stored remediation input, the only
	// retained copy of the candidate round 1 reviewed.
	noRoundOneInput bool
}

// driftAuditFixture is a run at review round 2 over a git-backed candidate.
// Round 1 reviewed an earlier head and reported finding-1; its remediation
// produced the current head, which fixed it. Round 2 reports finding-b. The
// fake auditor's over_hardened body reverses finding-1.
type driftAuditFixture struct {
	*findingAdjudicationFixture
	current       domain.ReviewRecord
	adjudication  domain.FindingAdjudication
	candidateRoot string
	roundOneHead  string
}

func newDriftAuditFixture(t *testing.T, options driftAuditFixtureOptions) *driftAuditFixture {
	t.Helper()
	candidateRoot := t.TempDir()
	git := func(args ...string) string {
		return strings.TrimSpace(string(runRemediationGit(t, candidateRoot, nil, args...)))
	}
	write := func(name, content string) {
		path := filepath.Join(candidateRoot, "daemon", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main", "--object-format=sha1")
	write("a.go", "package fixture\n")
	git("add", "daemon/a.go")
	git("commit", "-q", "-m", "base")
	baseSHA := git("rev-parse", "HEAD")
	write("a.go", "package fixture\n\n// round one\n")
	git("commit", "-q", "-am", "round one")
	roundOneHead := git("rev-parse", "HEAD")
	write("b.go", "package fixture\n")
	git("add", "daemon/b.go")
	git("commit", "-q", "-m", "candidate")
	headSHA := git("rev-parse", "HEAD")

	policy := map[string]string{"review.drift_audit_after": "2"}
	for key, value := range options.policy {
		if value == "" {
			delete(policy, key)
			continue
		}
		policy[key] = value
	}
	severity := options.earlierSeverity
	if severity == "" {
		severity = domain.FindingSeverityP2
	}
	base := newFindingAdjudicationFixtureWithOptions(t, severity,
		&domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}, "high", "high",
		findingAdjudicationFixtureOptions{
			note: "producer=fake/test; fixture", policy: policy,
			baseSHA: baseSHA, headSHA: roundOneHead, findingID: fake.DriftAuditFindingID,
		})
	base.writePath(t, base.baseRoot)
	base.workflow.artifacts = base.artifacts
	base.workflow.workDir = t.TempDir()
	task := validPublicationTask(t, base.task.RunID, base.task.ProjectID)
	task.HeadSHA, task.Replay.HeadSHA = headSHA, headSHA
	task.Replay.ObservedBaseSHA, task.Replay.ImportOptions.BaseSHA = baseSHA, baseSHA
	base.task = task
	taskPayload, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	productionInvocation, err := domain.NewAgentInvocation(
		productionInvocationID(task.RunID),
		[]domain.ArtifactID{domain.ArtifactID("production-input-" + string(task.RunID))}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	finding := domain.Finding{
		ID: "finding-b", RunID: task.RunID, Source: base.finding.Source,
		Severity: domain.FindingSeverityP2,
		Location: &domain.FindingLocation{Path: "daemon/b.go", StartLine: 1, EndLine: 1},
		Message:  "second review finding", RawText: "second review finding",
		CreatedAt: base.finding.CreatedAt.Add(2 * time.Minute),
	}
	if options.recur {
		finding.Location, finding.Message, finding.RawText = base.finding.Location,
			base.finding.Message, base.finding.RawText
	}
	current, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: ProductionReviewInvocationID(task.RunID, 2), RunID: task.RunID, Round: 2,
		Provider: base.record.Provider, ModelConfiguration: base.record.ModelConfiguration,
		ConfigurationDigest: base.record.ConfigurationDigest,
		InstructionDigest:   base.record.InstructionDigest, CostOwner: base.record.CostOwner,
		BaseSHA: baseSHA, HeadSHA: headSHA,
		CompletedAt:        base.record.CompletedAt.Add(2 * time.Minute),
		CompletionEvidence: adjudicationDigest("8"),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	roundOne, err := domain.NewFindingAdjudication(
		task.RunID, 1, base.binding.run.SpecDigest, base.record.InstructionDigest,
		base.binding.resolvedPolicy.Digest,
		[]domain.FindingAdjudicationEntry{
			adjudicationRouteEntry(t, base.finding.ID, domain.RouteRemediate),
		}, "", base.record.CompletedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	route := options.batchRoute
	if route == "" {
		route = domain.RouteRemediate
	}
	adjudication, err := domain.NewFindingAdjudication(
		task.RunID, 2, base.binding.run.SpecDigest, current.InstructionDigest,
		base.binding.resolvedPolicy.Digest,
		[]domain.FindingAdjudicationEntry{adjudicationRouteEntry(t, finding.ID, route)},
		"", current.CompletedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := base.store.Write(base.ctx, func(tx *store.WriteTx) error {
		if err := tx.PutAgentInvocation(base.ctx, productionInvocation); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(base.ctx, roundOne); err != nil {
			return err
		}
		if options.earlierOverHardened {
			earlier, err := domain.NewDriftAudit(domain.DriftAuditInput{
				RunID: task.RunID, Round: 1, BaseSHA: baseSHA, HeadSHA: roundOneHead,
				ApprovedSpecDigest:   base.binding.run.SpecDigest,
				ResolvedPolicyDigest: base.binding.resolvedPolicy.Digest,
				Verdict:              domain.DriftVerdictOverHardened, Confidence: domain.ConfidenceHigh,
				Reversals: []domain.DriftReversal{{
					FindingID: base.finding.ID, Undo: "drop it", Rationale: "not needed",
				}},
				Explanation: "an earlier audit", CreatedAt: base.record.CompletedAt.Add(time.Minute),
			})
			if err != nil {
				return err
			}
			if err := tx.PutDriftAudit(base.ctx, earlier); err != nil {
				return err
			}
		}
		if err := tx.PutReviewRecord(base.ctx, current, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(base.ctx, adjudication); err != nil {
			return err
		}
		return tx.PutFindingDisposition(base.ctx, domain.ReviewDispositionRecord{
			FindingID: base.finding.ID, RunID: task.RunID, Round: 1,
			Disposition:             domain.ReviewDispositionFixed,
			Reason:                  "absent from the independent remediation review",
			RemediationInvocationID: current.InvocationID, CreatedAt: current.CompletedAt,
		})
	}); err != nil {
		t.Fatalf("seed drift round: %v", err)
	}
	if err := base.store.WriteInternal(base.ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(base.ctx, productionPublicationTaskKey(task.RunID),
			KindProductionPublicationRequested, taskPayload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f := &driftAuditFixture{
		findingAdjudicationFixture: base, current: current, adjudication: adjudication,
		candidateRoot: candidateRoot, roundOneHead: roundOneHead,
	}
	if !options.noRoundOneInput {
		body, err := json.Marshal(remediationInput{
			Version: remediationInputVersion, RunID: task.RunID, Round: 1,
			BaseSHA: baseSHA, HeadSHA: roundOneHead,
			Instruction: remediationInstruction, CandidatePatchBase64: f.roundOnePatch(t),
		})
		if err != nil {
			t.Fatal(err)
		}
		artifact := f.putEvidence(t, remediationInputArtifactID(task.RunID, 1), body)
		if err := base.store.Write(base.ctx, func(tx *store.WriteTx) error {
			return tx.PutArtifact(base.ctx, artifact)
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// roundOnePatch is the candidate round 1 reviewed, as its remediation input
// carried it.
func (f *driftAuditFixture) roundOnePatch(t *testing.T) []byte {
	t.Helper()
	return runRemediationGit(t, f.candidateRoot, nil,
		"diff", "--binary", "--full-index", f.current.BaseSHA, f.roundOneHead, "--")
}

func (f *driftAuditFixture) script(body string) {
	f.driver.Script(inference.DriftAuditorSiteID,
		fake.Script{Response: inference.Response{Output: []byte(body), ComputeUnits: 5}})
}

func (f *driftAuditFixture) reconcile(t *testing.T) productionReviewGateState {
	t.Helper()
	state, err := f.workflow.executeFindingAdjudication(
		f.ctx, f.task, f.current, f.adjudication, f.baseRoot, f.candidateRoot)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return state
}

func (f *driftAuditFixture) auditRequests() []inference.Request {
	var requests []inference.Request
	for _, request := range f.driver.Requests() {
		if request.SiteID == inference.DriftAuditorSiteID {
			requests = append(requests, request)
		}
	}
	return requests
}

// driftRoundState is everything a drift test asserts on, read in one pass.
type driftRoundState struct {
	item          *domain.AttentionItem
	itemSnapshot  store.Snapshot
	audit         *domain.DriftAudit
	failure       bool
	input         *remediationInput
	request       remediationInvocationRequest
	supersessions []domain.FindingDispositionSupersession
}

func (f *driftAuditFixture) state(t *testing.T) driftRoundState {
	t.Helper()
	var out driftRoundState
	optional := func(err error) error {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		item, snapshot, err := tx.GetAttentionItemSnapshot(
			f.ctx, productionReviewDiminishingItemID(f.task.RunID, f.current.Round))
		if err == nil {
			out.item, out.itemSnapshot = &item, snapshot
		} else if optional(err) != nil {
			return err
		}
		audit, err := tx.GetDriftAuditForRound(f.ctx, f.task.RunID, f.current.Round)
		if err == nil {
			out.audit = &audit
		} else if optional(err) != nil {
			return err
		}
		_, err = tx.GetArtifact(f.ctx, driftAuditFailureArtifactID(f.task.RunID, f.current.Round))
		if out.failure = err == nil; optional(err) != nil {
			return err
		}
		entry, err := tx.GetOutbox(f.ctx, string(remediationInvocationID(f.task.RunID, f.current.Round)))
		if err == nil {
			if out.request, err = decodeRemediationRequest(entry); err != nil {
				return err
			}
			input, _, err := loadRemediationInput(f.artifacts, out.request.InputArtifactDigest)
			if err != nil {
				return err
			}
			out.input = &input
		} else if optional(err) != nil {
			return err
		}
		out.supersessions, err = tx.ListFindingDispositionSupersessions(f.ctx, f.task.RunID)
		return err
	}); err != nil {
		t.Fatalf("read drift round state: %v", err)
	}
	return out
}

func (f *driftAuditFixture) decide(t *testing.T, action domain.Action) {
	t.Helper()
	state := f.state(t)
	if state.item == nil {
		t.Fatal("no diminishing item to decide")
	}
	if _, err := f.signet.Submit(f.ctx, signet.ClientCommand{
		CommandID: "command-" + string(action), DeviceID: "device-test",
		ExpectedEntityVersion: state.itemSnapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: state.item.ID, Action: action, ItemVersion: state.item.ItemVersion,
			PRHeadSHA: state.item.PRHeadSHA, ArtifactDigests: state.item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatalf("submit %s: %v", action, err)
	}
}

// requireOrdinaryRound asserts the round dispatched a remediation that carries
// no reversal and superseded nothing.
func requireOrdinaryRound(t *testing.T, state driftRoundState) {
	t.Helper()
	if state.input == nil {
		t.Fatal("no remediation dispatched")
	}
	if state.input.Instruction != remediationInstruction || len(state.input.Reversals) != 0 ||
		state.input.DriftAuditDigest != "" || len(state.supersessions) != 0 {
		t.Fatalf("ordinary round carries a simplification: instruction %q, reversals %+v, supersessions %+v",
			state.input.Instruction, state.input.Reversals, state.supersessions)
	}
}

// requireSimplificationRound asserts the round dispatched one remediation that
// carries the audit's reversal with its engine-derived path, routed only the
// batch's own fix, and superseded the reversed fix under the given authority.
func requireSimplificationRound(
	t *testing.T, f *driftAuditFixture, state driftRoundState,
	kind domain.DispositionSupersessionAuthorityKind,
) {
	t.Helper()
	if state.input == nil || state.audit == nil {
		t.Fatalf("simplification round: input %v, audit %v", state.input, state.audit)
	}
	want := []remediationReversal{{
		FindingID: fake.DriftAuditFindingID, Path: "daemon/a.go",
		Undo: state.audit.Reversals[0].Undo, Rationale: state.audit.Reversals[0].Rationale,
	}}
	if state.input.Instruction != simplificationInstruction ||
		state.input.DriftAuditDigest != state.audit.Digest ||
		!slices.Equal(state.input.Reversals, want) {
		t.Fatalf("simplification input = instruction %q, digest %q, reversals %+v",
			state.input.Instruction, state.input.DriftAuditDigest, state.input.Reversals)
	}
	if !slices.Equal(state.request.FindingIDs, []domain.FindingID{"finding-b"}) {
		t.Fatalf("routed findings = %v, want only the batch's fix", state.request.FindingIDs)
	}
	if len(state.supersessions) != 1 {
		t.Fatalf("supersessions = %+v, want one", state.supersessions)
	}
	got := state.supersessions[0]
	if got.FindingID != fake.DriftAuditFindingID || got.SupersededRound != 1 ||
		got.ReversingRound != f.current.Round || got.DriftAuditDigest != state.audit.Digest ||
		got.RemediationInvocationID != f.current.InvocationID || got.Authority.Kind != kind {
		t.Fatalf("supersession = %+v", got)
	}
	var effective []store.EffectiveFindingDisposition
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		effective, err = tx.EffectiveFindingDispositions(f.ctx, f.task.RunID, f.current.Round)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, disposition := range effective {
		if disposition.Stored.FindingID == fake.DriftAuditFindingID &&
			disposition.Effective != domain.ReviewDispositionDeclined {
			t.Fatalf("reversed fix reads %q, want declined", disposition.Effective)
		}
	}
}

// TestDriftAuditRunsOnlyWhenTriggered pins the trigger (plan §7 "The audit"):
// no audit with the knob unset, below the trigger round, on a reevaluation, or
// on a round a deterministic cause stopped.
func TestDriftAuditRunsOnlyWhenTriggered(t *testing.T) {
	for _, tc := range []struct {
		name         string
		options      driftAuditFixtureOptions
		reevaluation bool
	}{
		{"knob unset", driftAuditFixtureOptions{
			policy: map[string]string{"review.drift_audit_after": ""},
		}, false},
		{"below the trigger round", driftAuditFixtureOptions{
			policy: map[string]string{"review.drift_audit_after": "3"},
		}, false},
		{"reevaluation", driftAuditFixtureOptions{batchRoute: domain.RouteDefer}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDriftAuditFixture(t, tc.options)
			f.script(fake.DriftAuditStuck)
			if tc.reevaluation {
				f.task.reevaluation = &productionReevaluation{CommandID: "command-reevaluate"}
			}
			f.reconcile(t)
			state := f.state(t)
			if len(f.auditRequests()) != 0 || state.audit != nil || state.failure || state.item != nil {
				t.Fatalf("audit ran: %d requests, audit %v, failure %t, item %v",
					len(f.auditRequests()), state.audit, state.failure, state.item)
			}
			if !tc.reevaluation {
				requireOrdinaryRound(t, state)
			}
		})
	}
}

// TestDriftAuditSkipsADeterministicallyStoppedRound shows a round that
// fixed_recurrence stopped runs no audit, before or after the human decides,
// and that its item carries the typed cause.
func TestDriftAuditSkipsADeterministicallyStoppedRound(t *testing.T) {
	f := newDriftAuditFixture(t, driftAuditFixtureOptions{recur: true})
	f.script(fake.DriftAuditStuck)
	if state := f.reconcile(t); state != productionReviewPending {
		t.Fatalf("stopped round = %d, want pending", state)
	}
	state := f.state(t)
	if state.item == nil || state.item.ReviewDiminishing == nil ||
		state.item.ReviewDiminishing.Cause != domain.ReviewDiminishingFixedRecurrence ||
		state.item.ReviewDiminishing.DriftAudit != nil {
		t.Fatalf("stopped round item = %+v", state.item)
	}
	f.decide(t, domain.ActionContinueUnderPolicy)
	f.reconcile(t)
	state = f.state(t)
	requireOrdinaryRound(t, state)
	if len(f.auditRequests()) != 0 || state.audit != nil || state.failure {
		t.Fatalf("audit ran on a stopped round: %d requests", len(f.auditRequests()))
	}
}

// TestDriftAuditFailureIsRecordedOnce shows the fail-safe default: a failed
// audit records one fact, parks nothing, lets the round continue as it would
// with the audit off, and is not retried on a later reconcile of the round.
func TestDriftAuditFailureIsRecordedOnce(t *testing.T) {
	f := newDriftAuditFixture(t, driftAuditFixtureOptions{})
	f.script(fake.DriftAuditMalformed)
	interrupted := errors.New("interrupted before the write")
	f.workflow.transitionHook = func(_ DurableTransition, side DurableTransitionSide) error {
		if side == DurableTransitionBefore {
			return interrupted
		}
		return nil
	}
	if _, err := f.workflow.executeFindingAdjudication(
		f.ctx, f.task, f.current, f.adjudication, f.baseRoot, f.candidateRoot,
	); !errors.Is(err, interrupted) {
		t.Fatalf("first reconcile = %v, want the interruption", err)
	}
	state := f.state(t)
	if !state.failure || state.audit != nil || state.item != nil || state.input != nil {
		t.Fatalf("after a failed audit: failure %t, audit %v, item %v", state.failure, state.audit, state.item)
	}
	f.workflow.transitionHook = nil
	f.script(fake.DriftAuditStuck)
	f.reconcile(t)
	state = f.state(t)
	if got := len(f.auditRequests()); got != 1 {
		t.Fatalf("audit requests = %d, want the one failed call", got)
	}
	if state.audit != nil || state.item != nil {
		t.Fatalf("a retried audit produced a verdict: audit %v, item %v", state.audit, state.item)
	}
	requireOrdinaryRound(t, state)
}

// TestDriftAuditConvergedRecordsOnlyTheArtifact shows converged changes
// nothing about the round.
func TestDriftAuditConvergedRecordsOnlyTheArtifact(t *testing.T) {
	f := newDriftAuditFixture(t, driftAuditFixtureOptions{})
	f.script(fake.DriftAuditConverged)
	f.reconcile(t)
	state := f.state(t)
	if state.audit == nil || state.audit.Verdict != domain.DriftVerdictConverged ||
		state.item != nil || state.failure {
		t.Fatalf("converged round: audit %v, item %v, failure %t", state.audit, state.item, state.failure)
	}
	requireOrdinaryRound(t, state)
}

// TestDriftAuditInputCarriesBothDiffs shows the audit receives the round-1
// candidate from round 1's stored remediation input, and fails safe when
// nothing retained that candidate.
func TestDriftAuditInputCarriesBothDiffs(t *testing.T) {
	t.Run("stored round-1 input", func(t *testing.T) {
		f := newDriftAuditFixture(t, driftAuditFixtureOptions{})
		f.script(fake.DriftAuditConverged)
		f.reconcile(t)
		requests := f.auditRequests()
		if len(requests) != 1 {
			t.Fatalf("audit requests = %d", len(requests))
		}
		fields := requests[0].Fields
		if fields["round_one_diff"] != string(f.roundOnePatch(t)) ||
			!strings.Contains(fields["current_diff"], "daemon/b.go") ||
			strings.Contains(fields["round_one_diff"], "daemon/b.go") {
			t.Fatalf("diffs = round one %q, current %q", fields["round_one_diff"], fields["current_diff"])
		}
		if !strings.Contains(fields["disposition_history"], fake.DriftAuditFindingID) ||
			!strings.Contains(fields["adjudication_entries"], "finding-b") {
			t.Fatalf("history = %q, entries %q", fields["disposition_history"], fields["adjudication_entries"])
		}
	})
	t.Run("round-1 candidate not retained", func(t *testing.T) {
		f := newDriftAuditFixture(t, driftAuditFixtureOptions{noRoundOneInput: true})
		f.script(fake.DriftAuditStuck)
		f.reconcile(t)
		state := f.state(t)
		if len(f.auditRequests()) != 0 || !state.failure || state.item != nil {
			t.Fatalf("unbuildable input: %d requests, failure %t, item %v",
				len(f.auditRequests()), state.failure, state.item)
		}
		requireOrdinaryRound(t, state)
	})
}

func (f *driftAuditFixture) putEvidence(
	t *testing.T, id domain.ArtifactID, body []byte,
) domain.Artifact {
	t.Helper()
	digest := domain.Digest(contentaddr.Sum(body))
	if _, err := f.artifacts.Put(digest, strings.NewReader(string(body))); err != nil {
		t.Fatal(err)
	}
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: id, Type: domain.ArtifactKindEvidence, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: f.record.InvocationID,
			HeadBinding: domain.HeadBound, SourceHeadSHA: f.roundOneHead,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: int64(len(body)),
			CreatedAt: f.workflow.now().UTC(), Source: domain.EvidenceSourceRun,
			Availability: domain.EvidenceAvailable,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

// TestDriftAuditStuckParks shows stuck raises the round's one
// review_diminishing_returns item with the drift facts, and that a replay
// neither audits again nor parks twice nor starts a remediation.
func TestDriftAuditStuckParks(t *testing.T) {
	f := newDriftAuditFixture(t, driftAuditFixtureOptions{})
	f.script(fake.DriftAuditStuck)
	for pass := range 2 {
		if state := f.reconcile(t); state != productionReviewPending {
			t.Fatalf("pass %d = %d, want pending", pass, state)
		}
	}
	state := f.state(t)
	if state.item == nil || state.audit == nil {
		t.Fatalf("stuck round: item %v, audit %v", state.item, state.audit)
	}
	facts := state.item.ReviewDiminishing
	if facts == nil || facts.Cause != domain.ReviewDiminishingDriftAudit || facts.DriftAudit == nil ||
		facts.DriftAudit.AuditDigest != state.audit.Digest ||
		facts.DriftAudit.Verdict != domain.DriftVerdictStuck ||
		facts.DriftAudit.Confidence != domain.ConfidenceMedium ||
		facts.DriftAudit.Explanation != state.audit.Explanation ||
		len(facts.DriftAudit.Reversals) != 0 || facts.DriftAudit.SimplificationOnContinue {
		t.Fatalf("stuck facts = %+v", facts)
	}
	if got := len(f.auditRequests()); got != 1 {
		t.Fatalf("audit requests = %d, want 1", got)
	}
	if state.input != nil || len(state.supersessions) != 0 {
		t.Fatal("a parked round dispatched a remediation")
	}
}

// TestDriftAuditAutoRoutesOneSimplificationRound is the automatic route end to
// end: the first over_hardened verdict that passes the gate becomes the
// round's remediation, with the reversal list in its input and an auto_route
// supersession record in the same write. The re-emitted finding then reads as
// a known decline, not a recurrence, and the stored input re-authenticates
// against the records.
func TestDriftAuditAutoRoutesOneSimplificationRound(t *testing.T) {
	f := newDriftAuditFixture(t, driftAuditFixtureOptions{})
	f.script(fake.DriftAuditOverHardened)
	if state := f.reconcile(t); state != productionReviewPending {
		t.Fatalf("routed round = %d, want pending on its remediation", state)
	}
	state := f.state(t)
	if state.item != nil {
		t.Fatalf("an automatic route parked: %+v", state.item)
	}
	requireSimplificationRound(t, f, state, domain.DispositionSupersessionAutoRoute)

	// The input's reversal list is re-derived from the supersession records,
	// so a stored input that drops or adds one fails authentication.
	verified := authenticatedRemediationTransition{request: state.request}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		verified.adjudication = f.adjudication
		finding, err := tx.GetFinding(f.ctx, "finding-b")
		if err != nil {
			return err
		}
		verified.findings = []domain.Finding{finding}
		verified.driftAuditDigest, verified.reversals, err = remediationReversalsTx(
			f.ctx, tx, f.task.RunID, f.current.Round)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := authenticateRemediationInput(f.artifacts, verified); err != nil {
		t.Fatalf("authenticate the simplification input: %v", err)
	}
	verified.reversals = nil
	if err := authenticateRemediationInput(f.artifacts, verified); !errors.Is(err, errRemediationMarkerUnreadable) {
		t.Fatalf("an input with unrecorded reversals authenticated: %v", err)
	}

	// A later reconcile of the dispatched round changes nothing.
	f.reconcile(t)
	if got := len(f.auditRequests()); got != 1 {
		t.Fatalf("audit requests = %d, want 1", got)
	}
	if replay := f.state(t); len(replay.supersessions) != 1 || replay.item != nil {
		t.Fatalf("replay changed the round: %+v", replay)
	}

	// The next round re-emits the reversed finding.
	reemitted := f.finding
	reemitted.ID = "finding-c"
	reemitted.CreatedAt = f.current.CompletedAt.Add(time.Minute)
	next, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: ProductionReviewInvocationID(f.task.RunID, 3), RunID: f.task.RunID, Round: 3,
		Provider: f.current.Provider, ModelConfiguration: f.current.ModelConfiguration,
		ConfigurationDigest: f.current.ConfigurationDigest,
		InstructionDigest:   f.current.InstructionDigest, CostOwner: f.current.CostOwner,
		BaseSHA: f.current.BaseSHA, HeadSHA: f.current.HeadSHA,
		CompletedAt:        f.current.CompletedAt.Add(time.Minute),
		CompletionEvidence: adjudicationDigest("7"),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{reemitted.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	nextAdjudication, err := domain.NewFindingAdjudication(
		f.task.RunID, 3, f.binding.run.SpecDigest, next.InstructionDigest,
		f.binding.resolvedPolicy.Digest,
		[]domain.FindingAdjudicationEntry{
			adjudicationRouteEntry(t, reemitted.ID, domain.RouteRemediate),
		}, "", next.CompletedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(f.ctx, next, []domain.Finding{reemitted}); err != nil {
			return err
		}
		return tx.PutFindingAdjudication(f.ctx, nextAdjudication)
	}); err != nil {
		t.Fatal(err)
	}
	convergence, err := f.workflow.reviewConvergenceState(f.ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	cause, stop, err := store.EvaluateReviewConvergence(convergence, next)
	if err != nil {
		t.Fatal(err)
	}
	if stop && cause == domain.ReviewDiminishingFixedRecurrence {
		t.Fatal("a reversed fix's re-emission stopped the loop as a recurrence")
	}

	// Yield history shows the audited round's verdict once a later round
	// follows it, and never the last round's.
	history, err := f.workflow.reviewYieldHistory(f.ctx, f.task.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Rounds) != 3 {
		t.Fatalf("yield history rounds = %d, want 3", len(history.Rounds))
	}
	body, err := json.Marshal(history.Rounds)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(body), string(domain.DriftVerdictOverHardened)); got != 1 {
		t.Fatalf("yield history names over_hardened %d times, want once (round 2): %s", got, body)
	}
}

// TestDriftAuditOverHardenedParks covers every reason an over_hardened verdict
// parks instead of routing, with the promise the card then makes.
func TestDriftAuditOverHardenedParks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options driftAuditFixtureOptions
		body    string
		// promise is simplification_on_continue.
		promise bool
		actions []domain.Action
	}{
		{
			"policy routes to park",
			driftAuditFixtureOptions{policy: map[string]string{"review.drift_audit_route": "park"}},
			fake.DriftAuditOverHardened, true, nil,
		},
		{
			"confidence below the threshold",
			driftAuditFixtureOptions{},
			driftAuditMediumConfidence, true, nil,
		},
		{
			"reverses a high finding's fix",
			driftAuditFixtureOptions{earlierSeverity: domain.FindingSeverityP1},
			fake.DriftAuditOverHardened, true, nil,
		},
		{
			"second over_hardened of the run",
			driftAuditFixtureOptions{earlierOverHardened: true},
			fake.DriftAuditOverHardened, true, nil,
		},
		{
			"reverses a finding of the round's own batch",
			driftAuditFixtureOptions{},
			driftAuditBatchReversal, false, nil,
		},
		{
			"no review round left",
			driftAuditFixtureOptions{policy: map[string]string{"review.hard_round_limit": "2"}},
			fake.DriftAuditOverHardened, false,
			[]domain.Action{domain.ActionFinishNow},
		},
		{
			"batch routes no fix",
			driftAuditFixtureOptions{batchRoute: domain.RouteDefer},
			fake.DriftAuditOverHardened, false, nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDriftAuditFixture(t, tc.options)
			f.script(tc.body)
			if state := f.reconcile(t); state != productionReviewPending {
				t.Fatalf("parked round = %d, want pending", state)
			}
			state := f.state(t)
			if state.item == nil || state.audit == nil || state.input != nil || len(state.supersessions) != 0 {
				t.Fatalf("round did not park cleanly: item %v, audit %v, input %v, supersessions %v",
					state.item, state.audit, state.input, state.supersessions)
			}
			facts := state.item.ReviewDiminishing
			if facts == nil || facts.Cause != domain.ReviewDiminishingDriftAudit || facts.DriftAudit == nil {
				t.Fatalf("facts = %+v", facts)
			}
			drift := facts.DriftAudit
			if drift.AuditDigest != state.audit.Digest || drift.Verdict != domain.DriftVerdictOverHardened ||
				drift.Confidence != state.audit.Confidence || drift.Explanation != state.audit.Explanation ||
				!slices.Equal(drift.Reversals, state.audit.Reversals) {
				t.Fatalf("drift facts = %+v, audit %+v", drift, state.audit)
			}
			if drift.SimplificationOnContinue != tc.promise {
				t.Fatalf("simplification_on_continue = %t, want %t", drift.SimplificationOnContinue, tc.promise)
			}
			if tc.actions != nil && !slices.Equal(state.item.RequestedDecision, tc.actions) {
				t.Fatalf("requested decisions = %v, want %v", state.item.RequestedDecision, tc.actions)
			}
		})
	}
}

// TestDriftAuditContinueUnderPolicy shows what the human's decision on a
// drift_audit item runs: the simplification round under human authority only
// when the card promised one and the action is continue_under_policy, and the
// action's landed meaning otherwise.
func TestDriftAuditContinueUnderPolicy(t *testing.T) {
	for _, tc := range []struct {
		name           string
		options        driftAuditFixtureOptions
		body           string
		action         domain.Action
		simplification bool
	}{
		{
			"continue on a promised simplification",
			driftAuditFixtureOptions{policy: map[string]string{"review.drift_audit_route": "park"}},
			fake.DriftAuditOverHardened, domain.ActionContinueUnderPolicy, true,
		},
		{
			"continue on a high finding's reversal",
			driftAuditFixtureOptions{earlierSeverity: domain.FindingSeverityP1},
			fake.DriftAuditOverHardened, domain.ActionContinueUnderPolicy, true,
		},
		{
			"apply then finish on a promised simplification",
			driftAuditFixtureOptions{policy: map[string]string{"review.drift_audit_route": "park"}},
			fake.DriftAuditOverHardened, domain.ActionApplyThenFinish, false,
		},
		{
			"continue on an invalid list",
			driftAuditFixtureOptions{},
			driftAuditBatchReversal, domain.ActionContinueUnderPolicy, false,
		},
		{
			"continue on a stuck verdict",
			driftAuditFixtureOptions{},
			fake.DriftAuditStuck, domain.ActionContinueUnderPolicy, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDriftAuditFixture(t, tc.options)
			f.script(tc.body)
			f.reconcile(t)
			f.decide(t, tc.action)
			if state := f.reconcile(t); state != productionReviewPending {
				t.Fatalf("decided round = %d, want pending on its remediation", state)
			}
			state := f.state(t)
			if got := len(f.auditRequests()); got != 1 {
				t.Fatalf("audit requests = %d, want 1", got)
			}
			if !tc.simplification {
				requireOrdinaryRound(t, state)
				return
			}
			requireSimplificationRound(t, f, state, domain.DispositionSupersessionHumanCommand)
			command := state.supersessions[0].Authority.Command
			if command == nil || command.ItemID != state.item.ID ||
				command.CommandID != "command-"+string(tc.action) {
				t.Fatalf("human authority = %+v", command)
			}
		})
	}
}

// TestDriftAuditRouteSurvivesAnInterruptedWrite stops the transition on each
// side of the write that commits the simplification round and reconciles
// again: one audit call, one remediation, one supersession record, no item.
func TestDriftAuditRouteSurvivesAnInterruptedWrite(t *testing.T) {
	for _, side := range []DurableTransitionSide{DurableTransitionBefore, DurableTransitionAfter} {
		t.Run(fmt.Sprint(side), func(t *testing.T) {
			f := newDriftAuditFixture(t, driftAuditFixtureOptions{})
			f.script(fake.DriftAuditOverHardened)
			interrupted := errors.New("interrupted")
			f.workflow.transitionHook = func(_ DurableTransition, at DurableTransitionSide) error {
				if at == side {
					return interrupted
				}
				return nil
			}
			if _, err := f.workflow.executeFindingAdjudication(
				f.ctx, f.task, f.current, f.adjudication, f.baseRoot, f.candidateRoot,
			); !errors.Is(err, interrupted) {
				t.Fatalf("first reconcile = %v, want the interruption", err)
			}
			f.workflow.transitionHook = nil
			f.reconcile(t)
			state := f.state(t)
			if got := len(f.auditRequests()); got != 1 {
				t.Fatalf("audit requests = %d, want 1", got)
			}
			if state.item != nil {
				t.Fatalf("replay parked: %+v", state.item)
			}
			requireSimplificationRound(t, f, state, domain.DispositionSupersessionAutoRoute)
		})
	}
}
