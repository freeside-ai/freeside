package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// cycleRemediationProducer is the remediator an external review cycle's first
// round dispatched, seeded as the engine records it: a dispatched intent
// bound to the cycle's authority, its admission, invocation, and input
// artifact, and the adjudication that routed the findings.
type cycleRemediationProducer struct {
	id      domain.InvocationID
	request domain.RemediationInvocationIntent
}

// seedCycleRemediationProducer records the producer's rows for the given
// review record and finding IDs. mutate edits the intent before it is
// enqueued, so a test can bind the producer to the wrong round or head.
func seedCycleRemediationProducer(
	t *testing.T, f externalReviewFixture, review domain.ReviewRecord,
	adjudication domain.FindingAdjudication, findingIDs []domain.FindingID,
	mutate func(*domain.RemediationInvocationIntent),
) cycleRemediationProducer {
	t.Helper()
	ctx := context.Background()
	round := review.Round
	producer := domain.RemediationInvocationID(f.run.ID, round)
	stageID := domain.RemediationStageID(f.run.ID, round)
	inputID := domain.ArtifactID(fmt.Sprintf("remediation-input-%d-%s", round, f.run.ID))
	input, err := domain.NewArtifact(domain.ArtifactInput{
		ID: inputID, Type: domain.ArtifactKindEvidence,
		Digest: domain.Digest("sha256:" + strings.Repeat("f", 64)),
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: review.InvocationID,
			HeadBinding: domain.HeadBound, SourceHeadSHA: review.HeadSHA,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 2,
			CreatedAt: reentryAt, Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := domain.NewAgentInvocation(
		producer, []domain.ArtifactID{"remediation-prompt", inputID}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	inputDigest, err := invocation.ComputeInputDigest()
	if err != nil {
		t.Fatal(err)
	}
	prior := f.admission
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: producer, RunID: f.run.ID, StageID: stageID,
		AttemptID: domain.AttemptID("attempt-" + string(producer)),
		Backend:   prior.Backend, Capabilities: prior.Capabilities,
		OperatingMode: prior.OperatingMode, CredentialMode: prior.CredentialMode,
		EgressProfile: prior.EgressProfile, ImageRef: prior.ImageRef,
		SpecDigest: prior.SpecDigest, PolicyDigest: prior.PolicyDigest, InputDigest: inputDigest,
		Base: prior.Base, Workspace: "workspace-remediate", AdmittedAt: prior.AdmittedAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := domain.RemediationInvocationIntent{
		Version: "freeside.remediation-request/v1", InvocationID: producer,
		RunID: f.run.ID, StageID: stageID, Round: round,
		ReviewInvocationID: review.InvocationID, AdjudicationDigest: adjudication.Digest,
		InputArtifactID: inputID, InputArtifactDigest: input.Digest,
		BaseSHA: review.BaseSHA, HeadSHA: review.HeadSHA, FindingIDs: findingIDs,
		SuccessorPublicationID: f.authority.PublicationID(),
	}
	if mutate != nil {
		mutate(&request)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	run := f.run
	run.Stages = append(slices.Clone(run.Stages), domain.Stage{
		ID: stageID, RunID: run.ID, Name: "implementation",
		Attempts: []domain.Attempt{{ID: admission.AttemptID, StageID: stageID, Number: 1, InvocationID: producer}},
	})
	if err := f.st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, input); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("seed remediation producer: %v", err)
	}
	if err := f.st.WriteInternal(ctx, func(tx *InternalTx) error {
		if _, _, err := tx.EnqueueOutbox(ctx, string(producer), "remediation_invocation_requested", payload); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(ctx, string(producer))
	}); err != nil {
		t.Fatalf("dispatch remediation producer: %v", err)
	}
	return cycleRemediationProducer{id: producer, request: request}
}

// authenticateProducer runs the producer gate for the cycle's authority.
func (f externalReviewFixture) authenticateProducer(t *testing.T, producer domain.InvocationID) error {
	t.Helper()
	ctx := context.Background()
	return f.st.Read(ctx, func(tx *ReadTx) error {
		return tx.AuthenticateSuccessorProducer(ctx, f.authority, producer)
	})
}

// cycleReview reads the record putCycleReview wrote for a round.
func cycleReview(t *testing.T, st *Store, round int) domain.ReviewRecord {
	t.Helper()
	ctx := context.Background()
	var review domain.ReviewRecord
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		review, err = tx.GetReviewRecord(ctx, domain.InvocationID("review-cycle-"+strconv.Itoa(round)))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return review
}

// seedCleanExternalCycle is an external review cycle whose first-round review
// record is clean: on the authority's base and head, listing no finding.
// Round 3 is a remediation round of the same cycle, listing a finding of
// its own.
func seedCleanExternalCycle(t *testing.T) (externalCycleFixture, domain.ReviewRecord) {
	t.Helper()
	f := externalCycleFixture{externalReviewFixture: seedExternalReview(t, externalReviewOptions{})}
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	review := putCycleReview(t, f.st, f.run.ID, externalCycleRound, reentryBase1, reentryHead1)
	f.laterInternal = cycleInternalFinding("finding-later-internal", f.run.ID)
	f.fixedBy = putCycleReview(t, f.st, f.run.ID, 3, reentryBase1, reentryHead2, f.laterInternal)
	return f, review
}

// TestSuccessorProducerAdmitsTheCycleFindingsItRemediates: the producer gate
// admits, under the cycle's own round, a remediation of the cycle's admitted
// external findings on a clean record, and a mixed one beside the record's
// own finding. Each refusal is one fact away from the admitted shape (issue
// #1767 decision 2).
func TestSuccessorProducerAdmitsTheCycleFindingsItRemediates(t *testing.T) {
	t.Parallel()
	t.Run("external findings on a clean record", func(t *testing.T) {
		t.Parallel()
		f, review := seedCleanExternalCycle(t)
		artifact := f.adjudication(t, externalCycleRound,
			map[domain.FindingID]domain.AdjudicationRoute{f.finding.ID: domain.RouteRemediate})
		if err := f.putAdjudication(t, artifact); err != nil {
			t.Fatal(err)
		}
		producer := seedCycleRemediationProducer(t, f.externalReviewFixture, review, artifact,
			[]domain.FindingID{f.finding.ID}, nil)
		if err := f.authenticateProducer(t, producer.id); err != nil {
			t.Fatalf("admitted producer = %v", err)
		}
	})
	t.Run("external finding beside the record's own", func(t *testing.T) {
		t.Parallel()
		f := seedExternalCycle(t)
		review := cycleReview(t, f.st, externalCycleRound)
		artifact := f.adjudication(t, externalCycleRound, map[domain.FindingID]domain.AdjudicationRoute{
			f.finding.ID: domain.RouteRemediate, f.internal.ID: domain.RouteRemediate,
		})
		if err := f.putAdjudication(t, artifact); err != nil {
			t.Fatal(err)
		}
		producer := seedCycleRemediationProducer(t, f.externalReviewFixture, review, artifact,
			[]domain.FindingID{f.internal.ID, f.finding.ID}, nil)
		if err := f.authenticateProducer(t, producer.id); err != nil {
			t.Fatalf("admitted mixed producer = %v", err)
		}
	})
}

// TestSuccessorProducerRefusesAFindingTheCycleDoesNotAnswer: on a clean
// record, every ID must be an external finding the cycle answers: of its run,
// on its head, by a reviewer its profile admits. A review-finding ID on a
// clean record is refused, and a round other than the cycle's own keeps the
// ordinary gate, which lists no external finding. The adjudication always
// routes the admitted finding: the store refuses an adjudication over the
// others before any producer exists, so the gate's own check is what each
// case reaches. A finding the cycle does answer is refused when the request's
// adjudication does not hold it on a remediate route.
func TestSuccessorProducerRefusesAFindingTheCycleDoesNotAnswer(t *testing.T) {
	t.Parallel()
	stranger := int64(externalReviewerID + 100)
	for _, tc := range []struct {
		name    string
		finding func(t *testing.T, f externalCycleFixture) domain.FindingID
	}{
		{"finding of another run", func(t *testing.T, f externalCycleFixture) domain.FindingID {
			other := f.run
			other.ID = "run-other"
			other.Stages = nil
			if err := f.st.Write(context.Background(), func(tx *WriteTx) error {
				return tx.PutRun(context.Background(), other)
			}); err != nil {
				t.Fatal(err)
			}
			finding := externalFindingBy(t, other.ID, reentryHead1, externalReviewerID, externalReviewerLogin,
				"PRRT_other_run", reentryAt.Add(time.Second))
			putExternalFinding(t, f.st, finding)
			return finding.ID
		}},
		{"finding on another head", func(t *testing.T, f externalCycleFixture) domain.FindingID {
			finding := externalFindingBy(t, f.run.ID, reentryHead2, externalReviewerID, externalReviewerLogin,
				"PRRT_other_head", reentryAt.Add(time.Second))
			putExternalFinding(t, f.st, finding)
			return finding.ID
		}},
		{"finding by an unlisted reviewer", func(t *testing.T, f externalCycleFixture) domain.FindingID {
			finding := externalFindingBy(t, f.run.ID, reentryHead1, stranger, "stranger",
				"PRRT_stranger", reentryAt.Add(time.Second))
			putExternalFinding(t, f.st, finding)
			return finding.ID
		}},
		{"review finding of another round on a clean record", func(t *testing.T, f externalCycleFixture) domain.FindingID {
			return f.laterInternal.ID
		}},
		{"finding no record holds", func(t *testing.T, f externalCycleFixture) domain.FindingID {
			return "finding-not-reported"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, review := seedCleanExternalCycle(t)
			artifact := f.adjudication(t, externalCycleRound,
				map[domain.FindingID]domain.AdjudicationRoute{f.finding.ID: domain.RouteRemediate})
			if err := f.putAdjudication(t, artifact); err != nil {
				t.Fatal(err)
			}
			producer := seedCycleRemediationProducer(t, f.externalReviewFixture, review, artifact,
				[]domain.FindingID{tc.finding(t, f)}, nil)
			if err := f.authenticateProducer(t, producer.id); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("producer = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
	// The cycle answers both findings. Only the request's adjudication says
	// which of them it sent to a fix.
	for _, tc := range []struct {
		name  string
		route domain.AdjudicationRoute
	}{
		{"admitted finding the adjudication does not hold", ""},
		{"admitted finding the adjudication declined", domain.RouteDecline},
		{"admitted finding the adjudication deferred", domain.RouteDefer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, review := seedCleanExternalCycle(t)
			second := externalFindingBy(t, f.run.ID, reentryHead1, externalReviewerID, externalReviewerLogin,
				"PRRT_second", reentryAt.Add(time.Second))
			putExternalFinding(t, f.st, second)
			routes := map[domain.FindingID]domain.AdjudicationRoute{f.finding.ID: domain.RouteRemediate}
			if tc.route != "" {
				routes[second.ID] = tc.route
			}
			artifact := f.adjudication(t, externalCycleRound, routes)
			if err := f.putAdjudication(t, artifact); err != nil {
				t.Fatal(err)
			}
			producer := seedCycleRemediationProducer(t, f.externalReviewFixture, review, artifact,
				[]domain.FindingID{f.finding.ID, second.ID}, nil)
			if err := f.authenticateProducer(t, producer.id); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("producer = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
	t.Run("round after the cycle's own", func(t *testing.T) {
		t.Parallel()
		f, _ := seedCleanExternalCycle(t)
		artifact := f.adjudication(t, 3,
			map[domain.FindingID]domain.AdjudicationRoute{f.laterInternal.ID: domain.RouteRemediate})
		if err := f.putAdjudication(t, artifact); err != nil {
			t.Fatal(err)
		}
		producer := seedCycleRemediationProducer(t, f.externalReviewFixture, f.fixedBy, artifact,
			[]domain.FindingID{f.laterInternal.ID, f.finding.ID}, nil)
		if err := f.authenticateProducer(t, producer.id); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("producer under round 3 = %v, want ErrParentKeyMismatch", err)
		}
	})
}
