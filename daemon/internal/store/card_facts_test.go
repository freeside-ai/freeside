package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestGoldenRoundTripCardFacts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{AdmissionFloors: attendedFloors()})
	execution := newAdmissionFixture(t, nil)
	execution.run.Stages[0].Name = "implement"
	outcome := cardFactExecutionOutcome(execution.admission)
	approval := cardFactApprovalItem(t)
	items := storeCardFactItems(t)
	revision := specRevisionCardFact(t)
	items["spec_revision"] = revision.item
	if err := recordSpecApprovalTerminal(ctx, s, revision.priorItem, 1); err != nil {
		t.Fatalf("record initial specification terminal: %v", err)
	}
	if err := recordSpecRevisionTerminal(ctx, s, revision.item); err != nil {
		t.Fatalf("record spec revision terminal: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, execution.run); err != nil {
			return err
		}
		if err := tx.RecordAuthIdentity(ctx, execution.identity, admissionEpoch); err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(ctx, execution.admission); err != nil {
			return err
		}
		if err := tx.RecordExecutionOutcome(ctx, outcome); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, approval); err != nil {
			return err
		}
		for _, artifact := range revision.artifacts {
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
		}
		if err := tx.PutAttentionItem(ctx, revision.priorItem); err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, revision.command); err != nil {
			return err
		}
		finding := adjudicationFinding("finding-1", execution.run.ID, "daemon/review.go", time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC))
		record := adjudicationReviewRecord(t, execution.run.ID, 2, []domain.FindingID{finding.ID}, finding.CreatedAt)
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		for _, item := range items {
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed card facts: %v", err)
	}

	for name, item := range items {
		t.Run(name, func(t *testing.T) {
			var got domain.AttentionItem
			if err := s.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				got, err = tx.GetAttentionItem(ctx, item.ID)
				return err
			}); err != nil {
				t.Fatalf("get: %v", err)
			}
			want := projectedAttentionItem(t, item)
			gotJSON := marshalIndent(t, got)
			if string(gotJSON) != string(marshalIndent(t, want)) {
				t.Fatalf("round trip mismatch for %s", name)
			}
			golden.Assert(t, "attention_item_card_"+name, gotJSON)
		})
	}
}

func TestPutAttentionItemAuthenticatesReviewDisputeBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	finding := adjudicationFinding("finding-1", f.run.ID, "daemon/review.go", at)
	record := adjudicationReviewRecord(t, f.run.ID, 2, []domain.FindingID{finding.ID}, at)
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, []domain.Finding{finding})
	}); err != nil {
		t.Fatalf("seed review: %v", err)
	}

	base := storeCardFactItems(t)["review_dispute"]
	cases := []struct {
		name   string
		mutate func(*domain.ReviewDisputeBinding)
	}{
		{"invented round", func(binding *domain.ReviewDisputeBinding) { binding.Round++ }},
		{"invented finding set", func(binding *domain.ReviewDisputeBinding) {
			binding.FindingIDs = []domain.FindingID{"finding-invented"}
		}},
		{"invented completion evidence", func(binding *domain.ReviewDisputeBinding) {
			binding.CompletionEvidence = adjudicationDigest("f")
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			candidate.ID = domain.ItemID(fmt.Sprintf("item-dispute-forged-%d", i))
			binding := *candidate.ReviewDispute
			binding.FindingIDs = slices.Clone(binding.FindingIDs)
			tc.mutate(&binding)
			candidate.ReviewDispute = &binding
			err := s.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, candidate)
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("put = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

func TestAttentionItemAuthenticatesShadowReviewDisputeBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-shadow-card-fact", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	routed := routedCandidate(t, run.ID, "routed-card-fact", 1, nil, at)
	finding := domain.Finding{
		ID: "shadow-finding-card-fact", RunID: run.ID,
		Source: string(domain.ShadowReviewClaudeLocal), Severity: domain.FindingSeverityP1,
		Location: &domain.FindingLocation{Path: "daemon/shadow.go", StartLine: 1, EndLine: 1},
		Message:  "shadow finding", RawText: "shadow finding", CreatedAt: at,
	}
	shadow := shadowRecord(t, run.ID, "shadow-card-fact", 1,
		[]domain.FindingID{finding.ID}, at)
	shadow.CompletionEvidence = adjudicationDigest("f")
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{finding})
	}); err != nil {
		t.Fatalf("seed shadow review: %v", err)
	}

	runID := run.ID
	input := domain.AttentionItemInput{
		ID: "item-shadow-card-fact", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionReviewDispute, Priority: domain.PriorityHigh,
		Reason: "a shadow finding is disputed", RequestedDecision: []domain.Action{domain.ActionDiscuss},
		ReviewDispute: &domain.ReviewDisputeBinding{
			RunID: run.ID, Round: shadow.ShadowedRound,
			FindingIDs: []domain.FindingID{finding.ID}, CompletionEvidence: shadow.CompletionEvidence,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &at, Status: domain.StatusOpen,
	}
	item, err := domain.NewAttentionItem(input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("put shadow dispute item: %v", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatalf("get shadow dispute item: %v", err)
	}

	forged := item
	forged.ID = "item-shadow-card-fact-forged"
	binding := *forged.ReviewDispute
	binding.CompletionEvidence = routed.CompletionEvidence
	forged.ReviewDispute = &binding
	err = s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, forged)
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("forged cross-lane binding put = %v, want ErrParentKeyMismatch", err)
	}
}

func TestPutAttentionItemRejectsChangedCardFact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{AdmissionFloors: attendedFloors()})
	execution := newAdmissionFixture(t, nil)
	execution.run.Stages[0].Name = "implement"
	item := storeCardFactItems(t)["execution_failure"]
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, execution.run); err != nil {
			return err
		}
		if err := tx.RecordAuthIdentity(ctx, execution.identity, admissionEpoch); err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(ctx, execution.admission); err != nil {
			return err
		}
		if err := tx.RecordExecutionOutcome(ctx, cardFactExecutionOutcome(execution.admission)); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changed := item
	changed.ItemVersion = 2
	fact := *changed.ExecutionFailure
	fact.InvocationID = "inv-other"
	changed.ExecutionFailure = &fact
	err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, changed) })
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("changed fact put = %v, want ErrImmutableConflict", err)
	}
}

func TestPutAttentionItemAuthenticatesInitialSpecification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	revision := specRevisionCardFact(t)
	item := revision.priorItem

	err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, revision.artifacts[0]); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put without completed specification terminal = %v, want ErrParentKeyMismatch", err)
	}
	if err := recordSpecApprovalTerminal(ctx, s, item, 1); err != nil {
		t.Fatalf("record initial specification terminal: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, revision.artifacts[0]); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("put authenticated initial specification: %v", err)
	}
}

func TestPutAttentionItemAuthenticatesSpecRevisionArtifacts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	revision := specRevisionCardFact(t)
	item := revision.item

	missingErr := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, item) })
	if !errors.Is(missingErr, store.ErrNotFound) {
		t.Fatalf("put without artifacts = %v, want ErrNotFound", missingErr)
	}
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		for _, artifact := range revision.artifacts {
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
		}
		if err := tx.PutAttentionItem(ctx, revision.priorItem); err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, revision.command); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put without specification terminal = %v, want ErrParentKeyMismatch", err)
	}
	if err := recordSpecApprovalTerminal(ctx, s, revision.priorItem, 1); err != nil {
		t.Fatalf("record initial specification terminal: %v", err)
	}
	if err := recordSpecRevisionTerminal(ctx, s, item); err != nil {
		t.Fatalf("record spec revision terminal: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		for _, artifact := range revision.artifacts {
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
		}
		if err := tx.PutAttentionItem(ctx, revision.priorItem); err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, revision.command); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("put valid revision: %v", err)
	}

	missingSummary := item
	missingSummary.ItemVersion++
	missingSummary.AgentClaims = []domain.AgentClaim{item.AgentClaims[0], item.AgentClaims[2]}
	missingSummary.ArtifactDigests = []domain.Digest{
		missingSummary.AgentClaims[0].Digest, missingSummary.AgentClaims[1].Digest,
	}
	slices.Sort(missingSummary.ArtifactDigests)
	err = s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, missingSummary) })
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put revision without summary = %v, want ErrParentKeyMismatch", err)
	}

	forgedSummary := item
	forgedSummary.ItemVersion++
	forgedSummary.AgentClaims = slices.Clone(item.AgentClaims)
	forgedText := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown, Content: "A caller-supplied summary.",
	}
	forgedSummary.AgentClaims[1].Text = &forgedText
	forgedSummary.AgentClaims[1].Digest = forgedText.ComputeDigest()
	forgedSummary.AgentClaims[1].Metadata.SizeBytes = int64(len(forgedText.Content))
	forgedSummary.ArtifactDigests = []domain.Digest{
		forgedSummary.AgentClaims[0].Digest,
		forgedSummary.AgentClaims[1].Digest,
		forgedSummary.AgentClaims[2].Digest,
	}
	slices.Sort(forgedSummary.ArtifactDigests)
	err = s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, forgedSummary) })
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put revision with forged summary = %v, want ErrParentKeyMismatch", err)
	}

	foreign := item
	foreign.ID = "item-card-spec-revision-foreign"
	facts := *foreign.SpecRevision
	facts.PriorSpecDigest = "sha256:foreign"
	foreign.SpecRevision = &facts
	err = s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, foreign) })
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put foreign prior digest = %v, want ErrParentKeyMismatch", err)
	}

	forgedDiff := item
	forgedDiff.ID = "spec-approval-implementation-run-forged-2"
	forgedFacts := *forgedDiff.SpecRevision
	forgedFacts.Diff.LinesAdded++
	forgedDiff.SpecRevision = &forgedFacts
	err = s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, forgedDiff) })
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put caller-supplied diff = %v, want ErrParentKeyMismatch", err)
	}

	changed := item
	changed.ItemVersion++
	changedFacts := *changed.SpecRevision
	changedFacts.Diff.LinesAdded++
	changed.SpecRevision = &changedFacts
	err = s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, changed) })
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put changed revision facts = %v, want ErrParentKeyMismatch", err)
	}
}

func TestPutAttentionItemBindsSpecRevisionAddressalsToCurrentInvocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*specRevisionCardFixture)
	}{
		{"foreign artifact identity", func(fixture *specRevisionCardFixture) {
			fixture.item.AgentClaims[2].Artifact = "spec-addressals-foreign-run-2"
			fixture.artifacts[3].ID = "spec-addressals-foreign-run-2"
		}},
		{"foreign invocation provenance", func(fixture *specRevisionCardFixture) {
			provenance := fixture.item.AgentClaims[2].Provenance
			provenance.ProducerInvocationID = "inv-specify-run-1-99"
			fixture.item.AgentClaims[2].Provenance = provenance
			fixture.artifacts[3].Provenance = provenance
		}},
		{"reordered claims", func(fixture *specRevisionCardFixture) {
			claims := fixture.item.AgentClaims
			claims[0], claims[1] = claims[1], claims[0]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openStore(t, store.Options{})
			revision := specRevisionCardFact(t)
			tc.mutate(&revision)
			if err := recordSpecApprovalTerminal(ctx, s, revision.priorItem, 1); err != nil {
				t.Fatal(err)
			}
			if err := recordSpecRevisionTerminal(ctx, s, revision.item); err != nil {
				t.Fatal(err)
			}
			err := s.Write(ctx, func(tx *store.WriteTx) error {
				for _, artifact := range revision.artifacts {
					if err := tx.PutArtifact(ctx, artifact); err != nil {
						return err
					}
				}
				if err := tx.PutAttentionItem(ctx, revision.priorItem); err != nil {
					return err
				}
				if err := tx.PutCommand(ctx, revision.command); err != nil {
					return err
				}
				return tx.PutAttentionItem(ctx, revision.item)
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("put = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

func TestPutAttentionItemAuthenticatesSpecRevisionCommands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		mutate      func(*specRevisionCardFixture)
		omitCommand bool
	}{
		{"missing command", func(*specRevisionCardFixture) {}, true},
		{"wrong action", func(fixture *specRevisionCardFixture) {
			fixture.command.Action = domain.ActionDiscuss
		}, false},
		{"wrong message", func(fixture *specRevisionCardFixture) {
			fixture.command.Message = "Different feedback."
		}, false},
		{"wrong iteration", func(fixture *specRevisionCardFixture) {
			fixture.item.ID = "spec-approval-implementation-run-3"
			currentProvenance := fixture.item.AgentClaims[0].Provenance
			currentProvenance.ProducerInvocationID = "inv-specify-run-1-3"
			fixture.item.AgentClaims[0].Artifact = "spec-implementation-run-3"
			fixture.item.AgentClaims[0].Provenance = currentProvenance
			fixture.item.AgentClaims[1].Provenance = currentProvenance
			fixture.item.AgentClaims[2].Provenance = currentProvenance
			fixture.artifacts[1].ID = "spec-implementation-run-3"
			fixture.artifacts[1].Provenance = currentProvenance
			fixture.artifacts[3].Provenance = currentProvenance
			facts := *fixture.item.SpecRevision
			facts.Iteration = 3
			facts.PriorComments[0].Iteration = 2
			fixture.item.SpecRevision = &facts
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openStore(t, store.Options{})
			revision := specRevisionCardFact(t)
			tc.mutate(&revision)
			if err := recordSpecApprovalTerminal(ctx, s, revision.priorItem, 1); err != nil {
				t.Fatal(err)
			}
			if err := recordSpecRevisionTerminal(ctx, s, revision.item); err != nil {
				t.Fatal(err)
			}
			err := s.Write(ctx, func(tx *store.WriteTx) error {
				for _, artifact := range revision.artifacts {
					if err := tx.PutArtifact(ctx, artifact); err != nil {
						return err
					}
				}
				if err := tx.PutAttentionItem(ctx, revision.priorItem); err != nil {
					return err
				}
				if !tc.omitCommand {
					if err := tx.PutCommand(ctx, revision.command); err != nil {
						return err
					}
				}
				return tx.PutAttentionItem(ctx, revision.item)
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("put = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

func TestPutAttentionItemRejectsOmittedSpecRevisionHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	revision := specRevisionCardFact(t)
	commentBody := "Also require an authenticated source."
	commentDigest := domain.Digest(contentaddr.Sum([]byte(commentBody)))
	feedback, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "spec-feedback-revise-again", Type: domain.ArtifactKindResearch, Digest: commentDigest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: "inv-specify-run-1-2",
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "revise-again", DeviceID: "device-1", ItemID: revision.item.ID,
		ItemVersion: revision.item.ItemVersion, ArtifactDigests: revision.item.ArtifactDigests,
		Action: domain.ActionRequestChanges, Message: commentBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	currentBody := "# Specification\n\nKeep the request valid, bound it to 1 MiB, and cite the source."
	summaryText := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown, Content: "Added the authenticated source.",
	}
	currentProvenance := revision.item.AgentClaims[0].Provenance
	currentProvenance.ProducerInvocationID = "inv-specify-run-1-3"
	current, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "spec-implementation-run-3", Type: domain.ArtifactKindSpecification,
		Digest: domain.Digest(contentaddr.Sum([]byte(currentBody))), Provenance: currentProvenance,
		Metadata: runMeta(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	addressals := []domain.SpecAddressalClaim{{CommentID: "revise-again", Response: "Added the source."}}
	addressalsBody, err := json.Marshal(addressals)
	if err != nil {
		t.Fatal(err)
	}
	addressalsDigest := domain.Digest(contentaddr.Sum(addressalsBody))
	addressalsArtifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "spec-addressals-implementation-run-3", Type: domain.ArtifactKindSpecification,
		Digest: addressalsDigest, Provenance: currentProvenance,
		Metadata: runMeta(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("run-1")
	at := time.Date(2026, 8, 29, 21, 0, 0, 0, time.UTC)
	forged, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "spec-approval-implementation-run-3", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason: "a third specification is ready",
		RequestedDecision: []domain.Action{
			domain.ActionApprove, domain.ActionRequestChanges, domain.ActionDiscuss, domain.ActionStop,
		},
		AgentClaims: []domain.AgentClaim{
			{
				Label: "Specification", Artifact: current.ID, Digest: current.Digest,
				Provenance: current.Provenance,
				Text:       &domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: currentBody},
				Metadata:   claimTextMeta(domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: currentBody}),
			},
			{
				Label: "freeside.summary", Artifact: "spec-summary-implementation-run-3",
				Digest: summaryText.ComputeDigest(), Provenance: currentProvenance, Text: &summaryText,
				Metadata: claimTextMeta(summaryText),
			},
			{
				Label: "Addressals", Artifact: addressalsArtifact.ID,
				Digest: addressalsDigest, Provenance: currentProvenance,
				Metadata: claimMeta(domain.EvidenceMediaImagePNG),
			},
		},
		SpecRevision: &domain.SpecRevisionFacts{
			Iteration: 3, PriorItemID: revision.item.ID,
			PriorSpecArtifactID: revision.artifacts[1].ID,
			PriorSpecDigest:     revision.artifacts[1].Digest,
			Diff:                domain.DeriveSpecDiff(revision.item.AgentClaims[0].Text.Content, currentBody),
			PriorComments: []domain.SpecRevisionComment{{
				CommentID: "revise-again", ArtifactID: feedback.ID, Digest: feedback.Digest,
				RaisedOnItemID: revision.item.ID, Iteration: 2, Body: commentBody,
			}},
			ClaimedAddressals: addressals, AddressalsDigest: addressalsDigest,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordSpecApprovalTerminal(ctx, s, revision.priorItem, 1); err != nil {
		t.Fatal(err)
	}
	if err := recordSpecRevisionTerminal(ctx, s, revision.item); err != nil {
		t.Fatal(err)
	}
	if err := recordSpecRevisionTerminal(ctx, s, forged); err != nil {
		t.Fatal(err)
	}
	err = s.Write(ctx, func(tx *store.WriteTx) error {
		for _, artifact := range append(revision.artifacts, current, feedback, addressalsArtifact) {
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
		}
		if err := tx.PutAttentionItem(ctx, revision.priorItem); err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, revision.command); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, revision.item); err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, forged)
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put omitted history = %v, want ErrParentKeyMismatch", err)
	}
}

func cardFactExecutionOutcome(admission domain.ExecutionAdmission) domain.ExecutionOutcome {
	return domain.ExecutionOutcome{
		InvocationID: admission.InvocationID, AdmissionID: admission.ID,
		Status: domain.ExecutionOutcomeFailed, Summary: "implementation failed",
		RecordedAt: admissionEpoch.Add(time.Minute),
	}
}

func cardFactApprovalItem(t *testing.T) domain.AttentionItem {
	t.Helper()
	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	runID := domain.RunID("run-1")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-spec", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason: "approve the specification", RequestedDecision: []domain.Action{domain.ActionApprove},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func storeCardFactItems(t *testing.T) map[string]domain.AttentionItem {
	t.Helper()
	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	runID := domain.RunID("run-1")
	subject := domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID}
	itemID := domain.ItemID("item-spec")
	rule := domain.TrustRuleTrustProfileDrift
	posture := domain.HealthPostureAdvisory
	runNames := &domain.DisplayNames{
		Project:  domain.DisplayName{Text: "owner/repo", Source: domain.DisplayNameSourceName},
		WorkUnit: domain.DisplayName{Text: "#1003", Source: domain.DisplayNameSourceName},
	}
	systemNames := &domain.DisplayNames{
		Project: domain.DisplayName{Text: "owner/repo", Source: domain.DisplayNameSourceName},
		WorkUnit: domain.DisplayName{
			Text: "daemon", Source: domain.DisplayNameSourceIdentifier,
		},
	}

	inputs := map[string]domain.AttentionItemInput{
		"cost": {
			ID: "item-card-cost", ProjectID: "proj-1", Subject: subject,
			Type: domain.AttentionReviewDiminishing, Priority: domain.PriorityNormal,
			Reason: "review yield is diminishing", RequestedDecision: []domain.Action{domain.ActionFinishNow},
			BillableCostSoFar: &domain.CostSoFar{Currency: "USD", Amount: "17.50", Invocations: 4},
			DisplayNames:      runNames,
			ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate, CreatedAt: &at, Status: domain.StatusOpen,
		},
		"execution_failure": {
			ID: "item-card-execution", ProjectID: "proj-1", Subject: subject,
			Type: domain.AttentionExecutionFailure, Priority: domain.PriorityUrgent,
			Reason: "implementation failed", RequestedDecision: []domain.Action{domain.ActionRetry},
			ExecutionFailure: &domain.ExecutionFailureFacts{
				Outcome: domain.ExecutionOutcomeFailed, Stage: domain.StageNameImplementation,
				InvocationID: "inv-1",
			},
			DisplayNames: runNames,
			ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional, CreatedAt: &at, Status: domain.StatusOpen,
		},
		"publish_block": {
			ID: "item-card-publish", ProjectID: "proj-1", Subject: subject,
			Type: domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
			Reason: "publication is blocked", RequestedDecision: []domain.Action{domain.ActionInspectTrustFailure},
			PublishBlock: &domain.PublishBlockFacts{TrustRule: &rule},
			DisplayNames: runNames,
			ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional, CreatedAt: &at, Status: domain.StatusOpen,
		},
		"diff_stats": {
			ID: "item-card-diff", ProjectID: "proj-1", Subject: subject,
			Type: domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
			Reason: "the pull request is ready for final review",
			RequestedDecision: []domain.Action{
				domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionDismiss,
			},
			PRHeadSHA: "cafebabe",
			PRReference: &domain.PRReference{
				Repo: "owner/repo", Number: 42,
			},
			DiffStats: &domain.DiffStats{
				FilesChanged: 3, Additions: 17, Deletions: 5,
				BaseSHA: "deadbeef", HeadSHA: "cafebabe",
			},
			DisplayNames: runNames,
			ItemVersion:  1, InterruptionClass: domain.InterruptionPlannedGate, CreatedAt: &at, Status: domain.StatusOpen,
		},
		"blocked_on": {
			ID: "item-card-blocked", ProjectID: "proj-1", Subject: subject,
			Type: domain.AttentionBlocked, Priority: domain.PriorityNormal,
			Reason: "waiting for specification approval", RequestedDecision: []domain.Action{},
			BlockedOn:    &domain.BlockedWait{Kind: domain.BlockedWaitSpecApproval, Since: at, ItemID: &itemID},
			DisplayNames: runNames,
			ItemVersion:  1, InterruptionClass: domain.InterruptionPlannedGate, CreatedAt: &at, Status: domain.StatusOpen,
		},
		"health_diagnostic": {
			ID: "item-card-health", ProjectID: "proj-1",
			Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
			Type:    domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
			Reason: "run projection is unavailable", RequestedDecision: []domain.Action{domain.ActionRunDoctor},
			HealthDiagnostic: &domain.HealthDiagnostic{
				Code: "run_projection.unavailable", Impairs: domain.ImpairedCapabilityRunVisibility,
			},
			DisplayNames: systemNames,
			Posture:      &posture, ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
			CreatedAt: &at, Status: domain.StatusOpen,
		},
		"review_dispute": {
			ID: "item-card-dispute", ProjectID: "proj-1", Subject: subject,
			Type: domain.AttentionReviewDispute, Priority: domain.PriorityHigh,
			Reason: "a review finding is disputed", RequestedDecision: []domain.Action{domain.ActionDiscuss},
			ReviewDispute: &domain.ReviewDisputeBinding{
				RunID: runID, Round: 2, FindingIDs: []domain.FindingID{"finding-1"},
				CompletionEvidence: adjudicationDigest("e"),
			},
			DisplayNames: runNames,
			ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional, CreatedAt: &at, Status: domain.StatusOpen,
		},
	}
	items := make(map[string]domain.AttentionItem, len(inputs))
	for name, input := range inputs {
		item, err := domain.NewAttentionItem(input, nil)
		if err != nil {
			t.Fatalf("NewAttentionItem %s: %v", name, err)
		}
		items[name] = item
	}
	return items
}

type specRevisionCardFixture struct {
	item      domain.AttentionItem
	priorItem domain.AttentionItem
	command   domain.Command
	artifacts []domain.Artifact
}

func specRevisionCardFact(t *testing.T) specRevisionCardFixture {
	t.Helper()
	priorProvenance := domain.Provenance{
		ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-specify-run-1-1",
		HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
	}
	currentProvenance := priorProvenance
	currentProvenance.ProducerInvocationID = "inv-specify-run-1-2"
	priorBody := "# Specification\n\nKeep the request valid."
	currentBody := "# Specification\n\nKeep the request valid and bound it to 1 MiB."
	summaryText := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown, Content: "Bound the request body to 1 MiB.",
	}
	prior, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "spec-implementation-run-1", Type: domain.ArtifactKindSpecification,
		Digest:     domain.Digest(contentaddr.Sum([]byte(priorBody))),
		Provenance: priorProvenance,
		Metadata:   runMeta(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "spec-implementation-run-2", Type: domain.ArtifactKindSpecification,
		Digest: domain.Digest(contentaddr.Sum([]byte(currentBody))), Provenance: currentProvenance,
		Metadata: runMeta(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	commentBody := "Bound the request body."
	commentDigest := domain.Digest(contentaddr.Sum([]byte(commentBody)))
	feedback, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "spec-feedback-revise-spec", Type: domain.ArtifactKindResearch, Digest: commentDigest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: "inv-specify-run-1-1",
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	addressals := []domain.SpecAddressalClaim{{CommentID: "revise-spec", Response: "Added a 1 MiB bound."}}
	addressalsBody, err := json.Marshal(addressals)
	if err != nil {
		t.Fatal(err)
	}
	addressalsDigest := domain.Digest(contentaddr.Sum(addressalsBody))
	addressalsArtifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "spec-addressals-implementation-run-2", Type: domain.ArtifactKindSpecification, Digest: addressalsDigest,
		Provenance: currentProvenance,
		Metadata:   runMeta(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("run-1")
	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	priorItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "spec-approval-implementation-run-1", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason: "the first specification is ready",
		RequestedDecision: []domain.Action{
			domain.ActionApprove, domain.ActionRequestChanges, domain.ActionDiscuss, domain.ActionStop,
		},
		AgentClaims: []domain.AgentClaim{{
			Label: "Specification", Artifact: prior.ID, Digest: prior.Digest, Provenance: prior.Provenance,
			Text:     &domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: priorBody},
			Metadata: claimTextMeta(domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: priorBody}),
		}},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "revise-spec", DeviceID: "device-1", ItemID: priorItem.ID,
		ItemVersion: priorItem.ItemVersion, ArtifactDigests: priorItem.ArtifactDigests,
		Action: domain.ActionRequestChanges, Message: commentBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "spec-approval-implementation-run-2", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason: "a revised specification is ready",
		RequestedDecision: []domain.Action{
			domain.ActionApprove, domain.ActionRequestChanges, domain.ActionDiscuss, domain.ActionStop,
		},
		AgentClaims: []domain.AgentClaim{
			{
				Label: "Specification", Artifact: current.ID, Digest: current.Digest,
				Provenance: current.Provenance,
				Text:       &domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: currentBody},
				Metadata:   claimTextMeta(domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: currentBody}),
			},
			{
				Label: "freeside.summary", Artifact: "spec-summary-implementation-run-2",
				Digest: summaryText.ComputeDigest(), Provenance: currentProvenance, Text: &summaryText,
				Metadata: claimTextMeta(summaryText),
			},
			{Label: "Addressals", Artifact: addressalsArtifact.ID, Digest: addressalsDigest, Provenance: currentProvenance, Metadata: claimMeta(domain.EvidenceMediaImagePNG)},
		},
		SpecRevision: &domain.SpecRevisionFacts{
			Iteration: 2, PriorItemID: priorItem.ID,
			PriorSpecArtifactID: prior.ID, PriorSpecDigest: prior.Digest,
			Diff: domain.DeriveSpecDiff(priorBody, currentBody),
			PriorComments: []domain.SpecRevisionComment{{
				CommentID: "revise-spec", ArtifactID: feedback.ID, Digest: feedback.Digest,
				RaisedOnItemID: priorItem.ID, Iteration: 1, Body: commentBody,
			}},
			ClaimedAddressals: addressals, AddressalsDigest: addressalsDigest,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return specRevisionCardFixture{
		item: item, priorItem: priorItem, command: command,
		artifacts: []domain.Artifact{prior, current, feedback, addressalsArtifact},
	}
}

func recordSpecRevisionTerminal(
	ctx context.Context,
	s *store.Store,
	item domain.AttentionItem,
) error {
	if item.SpecRevision == nil {
		return domain.ErrParentKeyMismatch
	}
	return recordSpecApprovalTerminal(ctx, s, item, item.SpecRevision.Iteration)
}

func recordSpecApprovalTerminal(
	ctx context.Context,
	s *store.Store,
	item domain.AttentionItem,
	iteration int,
) error {
	var specification *domain.AgentClaim
	var summaryDigest *domain.Digest
	for index := range item.AgentClaims {
		switch item.AgentClaims[index].Label {
		case "Specification":
			specification = &item.AgentClaims[index]
		case "freeside.summary":
			summaryDigest = &item.AgentClaims[index].Digest
		}
	}
	if specification == nil {
		return domain.ErrParentKeyMismatch
	}
	terminal := struct {
		InvocationID        domain.InvocationID `json:"invocation_id"`
		Iteration           int                 `json:"iteration"`
		Status              string              `json:"status"`
		ResearchArtifactIDs []domain.ArtifactID `json:"research_artifact_ids"`
		SpecArtifactID      *domain.ArtifactID  `json:"spec_artifact_id,omitempty"`
		ApprovalItemID      *domain.ItemID      `json:"approval_item_id,omitempty"`
		SummaryDigest       *domain.Digest      `json:"summary_digest,omitempty"`
	}{
		InvocationID: specification.Provenance.ProducerInvocationID,
		Iteration:    iteration, Status: "completed",
		ResearchArtifactIDs: []domain.ArtifactID{}, SpecArtifactID: &specification.Artifact,
		ApprovalItemID: &item.ID, SummaryDigest: summaryDigest,
	}
	body, err := json.Marshal(terminal)
	if err != nil {
		return err
	}
	return s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.RecordInbox(
			ctx, string(terminal.InvocationID), "specification_stage_terminal", body,
		)
		return err
	})
}
