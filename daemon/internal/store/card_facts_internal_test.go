package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func TestPutAttentionItemRegatesRestoredSpecRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/spec-revision.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	item, priorItem, command, artifacts := internalSpecRevisionFixture(t)
	if err := recordInternalSpecApprovalTerminal(ctx, st, priorItem, 1); err != nil {
		t.Fatalf("record initial specification terminal: %v", err)
	}
	if err := recordInternalSpecRevisionTerminal(ctx, st, item); err != nil {
		t.Fatalf("record spec revision terminal: %v", err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		for _, artifact := range artifacts {
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
		}
		if err := tx.PutAttentionItem(ctx, priorItem); err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `
		UPDATE attention_items
		SET body = json_set(body, '$.spec_revision.diff.lines_added', 9)
		WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}

	updated := item
	updated.ItemVersion++
	updated.Status = domain.StatusSuperseded
	updated.SpecRevision = nil
	err = st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, updated) })
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("update carrying restored forged revision = %v, want ErrParentKeyMismatch", err)
	}
}

func TestPutAttentionItemRestoresValidSpecRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/spec-revision.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	item, priorItem, command, artifacts := internalSpecRevisionFixture(t)
	if err := recordInternalSpecApprovalTerminal(ctx, st, priorItem, 1); err != nil {
		t.Fatal(err)
	}
	if err := recordInternalSpecRevisionTerminal(ctx, st, item); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		for _, artifact := range artifacts {
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
		}
		if err := tx.PutAttentionItem(ctx, priorItem); err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("seed revision: %v", err)
	}

	updated := item
	updated.ItemVersion++
	updated.Status = domain.StatusSuperseded
	updated.SpecRevision = nil
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, updated) }); err != nil {
		t.Fatalf("update carrying restored revision: %v", err)
	}
}

func internalSpecRevisionFixture(
	t *testing.T,
) (domain.AttentionItem, domain.AttentionItem, domain.Command, []domain.Artifact) {
	t.Helper()
	priorProvenance := domain.Provenance{
		ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-elaborate-run-1-1",
		HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
	}
	currentProvenance := priorProvenance
	currentProvenance.ProducerInvocationID = "inv-elaborate-run-1-2"
	priorBody := "# Specification\n\nKeep the request valid."
	currentBody := "# Specification\n\nKeep the request valid and bound it to 1 MiB."
	summaryText := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown, Content: "Bound the request body to 1 MiB.",
	}
	artifact := func(id domain.ArtifactID, kind domain.ArtifactKind, body string, p domain.Provenance) domain.Artifact {
		t.Helper()
		value, err := domain.NewArtifact(domain.ArtifactInput{
			ID: id, Type: kind, Digest: domain.Digest(contentaddr.Sum([]byte(body))), Provenance: p,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	prior := artifact("spec-implementation-run-1", domain.ArtifactKindSpecification, priorBody, priorProvenance)
	current := artifact("spec-implementation-run-2", domain.ArtifactKindSpecification, currentBody, currentProvenance)
	commentBody := "Bound the request body."
	feedbackProvenance := domain.Provenance{
		ProducerClass: domain.ProducerDaemon, ProducerInvocationID: "inv-elaborate-run-1-1",
		HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
	}
	feedback := artifact("spec-feedback-revise-spec", domain.ArtifactKindResearch, commentBody, feedbackProvenance)
	addressals := []domain.SpecAddressalClaim{{CommentID: "revise-spec", Response: "Added a 1 MiB bound."}}
	addressalsBody, err := json.Marshal(addressals)
	if err != nil {
		t.Fatal(err)
	}
	addressalsArtifact := artifact(
		"spec-addressals-implementation-run-2", domain.ArtifactKindSpecification,
		string(addressalsBody), currentProvenance,
	)
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
			Text: &domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: priorBody},
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
		Reason: "a revised specification is ready", RequestedDecision: []domain.Action{domain.ActionApprove},
		AgentClaims: []domain.AgentClaim{
			{
				Label: "Specification", Artifact: current.ID, Digest: current.Digest,
				Provenance: current.Provenance,
				Text:       &domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: currentBody},
			},
			{
				Label: summaryEvidenceLabel, Artifact: "spec-summary-implementation-run-2",
				Digest: summaryText.ComputeDigest(), Provenance: currentProvenance, Text: &summaryText,
			},
			{
				Label: "Addressals", Artifact: addressalsArtifact.ID,
				Digest: addressalsArtifact.Digest, Provenance: currentProvenance,
			},
		},
		SpecRevision: &domain.SpecRevisionFacts{
			Iteration: 2, PriorItemID: priorItem.ID,
			PriorSpecArtifactID: prior.ID, PriorSpecDigest: prior.Digest,
			Diff: domain.DeriveSpecDiff(priorBody, currentBody),
			PriorComments: []domain.SpecRevisionComment{{
				CommentID: "revise-spec", ArtifactID: feedback.ID, Digest: feedback.Digest,
				RaisedOnItemID: priorItem.ID, Iteration: 1, Body: commentBody,
			}},
			ClaimedAddressals: addressals, AddressalsDigest: addressalsArtifact.Digest,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return item, priorItem, command, []domain.Artifact{prior, current, feedback, addressalsArtifact}
}

func recordInternalSpecRevisionTerminal(
	ctx context.Context,
	st *Store,
	item domain.AttentionItem,
) error {
	if item.SpecRevision == nil {
		return domain.ErrParentKeyMismatch
	}
	return recordInternalSpecApprovalTerminal(ctx, st, item, item.SpecRevision.Iteration)
}

func recordInternalSpecApprovalTerminal(
	ctx context.Context,
	st *Store,
	item domain.AttentionItem,
	iteration int,
) error {
	var specification *domain.AgentClaim
	var summaryDigest *domain.Digest
	for index := range item.AgentClaims {
		switch item.AgentClaims[index].Label {
		case "Specification":
			specification = &item.AgentClaims[index]
		case summaryEvidenceLabel:
			summaryDigest = &item.AgentClaims[index].Digest
		}
	}
	if specification == nil {
		return domain.ErrParentKeyMismatch
	}
	terminal := blockedWaitElaborationTerminal{
		InvocationID: specification.Provenance.ProducerInvocationID,
		Iteration:    iteration, Status: "completed",
		ResearchArtifactIDs: []domain.ArtifactID{}, SpecArtifactID: &specification.Artifact,
		ApprovalItemID: &item.ID, SummaryDigest: summaryDigest,
	}
	body, err := json.Marshal(terminal)
	if err != nil {
		return err
	}
	return st.WriteInternal(ctx, func(tx *InternalTx) error {
		_, _, err := tx.RecordInbox(
			ctx, string(terminal.InvocationID), blockedWaitElaborationTerminalKind, body,
		)
		return err
	})
}

func TestAttentionItemReadAuthenticatesReviewDisputeBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/card-facts.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-card-facts", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	finding := domain.Finding{
		ID: "finding-1", RunID: run.ID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/review.go", StartLine: 1, EndLine: 1},
		Message:  "finding one", RawText: "finding one", CreatedAt: at,
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-card-facts", RunID: run.ID, Round: 2,
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
	runID := run.ID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-card-facts", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionReviewDispute, Priority: domain.PriorityHigh,
		Reason: "a review finding is disputed", RequestedDecision: []domain.Action{domain.ActionDiscuss},
		ReviewDispute: &domain.ReviewDisputeBinding{
			RunID: run.ID, Round: record.Round, FindingIDs: []domain.FindingID{finding.ID},
			CompletionEvidence: record.CompletionEvidence,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	var originalBody string
	if err := st.db.QueryRowContext(ctx,
		`SELECT body FROM attention_items WHERE id = ?`, item.ID).Scan(&originalBody); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		path  string
		value any
	}{
		{"invented round", "$.review_dispute.round", 3},
		{"invented finding set", "$.review_dispute.finding_ids[0]", "finding-invented"},
		{"invented completion evidence", "$.review_dispute.completion_evidence", "sha256:" + strings.Repeat("f", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx,
				`UPDATE attention_items SET body = json_set(?, ?, ?) WHERE id = ?`,
				originalBody, tc.path, tc.value, item.ID); err != nil {
				t.Fatal(err)
			}
			reads := []struct {
				name string
				read func(*ReadTx) error
			}{
				{"snapshot", func(tx *ReadTx) error {
					_, _, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
					return err
				}},
				{"history", func(tx *ReadTx) error {
					_, err := tx.GetAttentionItemRecord(ctx, item.ID)
					return err
				}},
			}
			for _, read := range reads {
				err := st.Read(ctx, read.read)
				if !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("%s read = %v, want ErrParentKeyMismatch", read.name, err)
				}
			}
		})
	}
}

func TestAttentionItemReadAuthenticatesBlockedWait(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/blocked-card-fact.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	invocationID := domain.InvocationID("inv-elaborate-run-blocked-card-fact-1")
	run := domain.Run{
		ID: "run-blocked-card-fact", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	runID := run.ID
	specification, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "spec-implementation-run-1", Type: domain.ArtifactKindSpecification,
		Digest: "sha256:specification",
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: invocationID,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "spec-approval-implementation-run-1", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason: "approve the specification", RequestedDecision: []domain.Action{domain.ActionApprove},
		AgentClaims: []domain.AgentClaim{{
			Label: "Specification", Artifact: specification.ID, Digest: specification.Digest,
			Provenance: specification.Provenance,
		}},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	approvalID := approval.ID
	blocked, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-blocked-card-fact", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionBlocked, Priority: domain.PriorityNormal,
		Reason: "waiting for specification approval", RequestedDecision: []domain.Action{},
		BlockedOn: &domain.BlockedWait{
			Kind: domain.BlockedWaitSpecApproval, Since: at, ItemID: &approvalID,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	terminalBody := []byte(`{"invocation_id":"inv-elaborate-run-blocked-card-fact-1","iteration":1,"status":"completed","research_artifact_ids":[],"spec_artifact_id":"spec-implementation-run-1","approval_item_id":"spec-approval-implementation-run-1"}`)
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		_, _, err := tx.RecordInbox(
			ctx, string(invocationID), blockedWaitElaborationTerminalKind, terminalBody,
		)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, specification); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, approval); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, blocked)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE inbox SET created_at = ? WHERE idempotency_key = ?`,
		formatTime(at), invocationID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE attention_items SET body = json_set(body, '$.created_at', NULL) WHERE id = ?`,
		approval.ID,
	); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetAttentionItem(ctx, blocked.ID)
		return err
	}); err != nil {
		t.Fatalf("legacy approval wait read = %v", err)
	}
	var legacyApprovalBody string
	if err := st.db.QueryRowContext(ctx,
		`SELECT body FROM attention_items WHERE id = ?`, approval.ID,
	).Scan(&legacyApprovalBody); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func() error
	}{
		{
			name: "wrong terminal kind",
			mutate: func() error {
				_, err := st.db.ExecContext(ctx,
					`UPDATE inbox SET kind = 'other_terminal' WHERE idempotency_key = ?`,
					invocationID,
				)
				return err
			},
		},
		{
			name: "wrong terminal payload",
			mutate: func() error {
				_, err := st.db.ExecContext(ctx,
					`UPDATE inbox SET payload = ? WHERE idempotency_key = ?`,
					[]byte("{}"), invocationID,
				)
				return err
			},
		},
		{
			name: "summary digest on legacy approval",
			mutate: func() error {
				payload := []byte(strings.TrimSuffix(string(terminalBody), "}") +
					`,"summary_digest":"sha256:` + strings.Repeat("a", 64) + `"}`)
				_, err := st.db.ExecContext(ctx,
					`UPDATE inbox SET payload = ? WHERE idempotency_key = ?`,
					payload, invocationID,
				)
				return err
			},
		},
		{
			name: "substituted specification artifact",
			mutate: func() error {
				substitute := specification
				substitute.ID = "spec-implementation-run-substitute-1"
				if err := st.Write(ctx, func(tx *WriteTx) error {
					return tx.PutArtifact(ctx, substitute)
				}); err != nil {
					return err
				}
				_, err := st.db.ExecContext(ctx,
					`UPDATE attention_items SET body = json_set(body, '$.agent_claims[0].artifact_id', ?) WHERE id = ?`,
					substitute.ID, approval.ID,
				)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx,
				`UPDATE inbox SET kind = ?, payload = ? WHERE idempotency_key = ?`,
				blockedWaitElaborationTerminalKind, terminalBody, invocationID,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx,
				`UPDATE attention_items SET body = ? WHERE id = ?`,
				legacyApprovalBody, approval.ID,
			); err != nil {
				t.Fatal(err)
			}
			if err := tc.mutate(); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetAttentionItem(ctx, blocked.ID)
				return err
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("read = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE inbox SET kind = ?, payload = ? WHERE idempotency_key = ?`,
		blockedWaitElaborationTerminalKind, terminalBody, invocationID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE attention_items SET body = ? WHERE id = ?`, legacyApprovalBody, approval.ID,
	); err != nil {
		t.Fatal(err)
	}
	assertAttentionItemFactReadGate(t, st, blocked.ID,
		attentionItemFactMutation{"$.blocked_on.item_id", string(blocked.ID)},
		attentionItemFactMutation{"$.blocked_on.since", at.Add(-time.Hour).Format(time.RFC3339Nano)},
	)
	successor := blocked
	successor.ItemVersion++
	successor.BlockedOn = nil
	err = st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, successor) })
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("restored blocked wait put = %v, want ErrParentKeyMismatch", err)
	}
}

func TestBlockedWaitTerminalSummaryMatchesApproval(t *testing.T) {
	invocationID := domain.InvocationID("inv-elaborate-elaboration-run-1")
	provenance := domain.Provenance{
		ProducerClass: domain.ProducerAgent, ProducerInvocationID: invocationID,
		HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
	}
	specification, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "spec-implementation-run-1", Type: domain.ArtifactKindSpecification,
		Digest: "sha256:specification", Provenance: provenance,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	summaryText := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown, Content: "Approved summary.",
	}
	summaryDigest := summaryText.ComputeDigest()
	approval := domain.AttentionItem{AgentClaims: []domain.AgentClaim{
		{
			Label: "Specification", Artifact: specification.ID,
			Digest: specification.Digest, Provenance: provenance,
		},
		{
			Label: export.SummaryEvidenceLabel, Artifact: "spec-summary-implementation-run-1",
			Digest: summaryDigest, Provenance: provenance, Text: &summaryText,
		},
	}}
	terminal := blockedWaitElaborationTerminal{SummaryDigest: &summaryDigest}
	if !blockedWaitTerminalSummaryMatches(terminal, approval, specification) {
		t.Fatal("canonical summary binding rejected")
	}
	approval.AgentClaims[1].Artifact = "spec-summary-foreign-1"
	if blockedWaitTerminalSummaryMatches(terminal, approval, specification) {
		t.Fatal("foreign summary artifact accepted")
	}
}

func TestBlockedWaitTerminalIdentityMatchesApproval(t *testing.T) {
	elaborationRun := domain.RunID("elaboration-run")
	approval := domain.AttentionItem{
		ID: "spec-approval-implementation-run-1",
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(elaborationRun),
			RunID: &elaborationRun,
		},
	}
	terminal := blockedWaitElaborationTerminal{
		InvocationID: "inv-elaborate-elaboration-run-1", Iteration: 1,
	}
	specification := domain.Artifact{ID: "spec-implementation-run-1"}
	if !blockedWaitTerminalIdentityMatches(terminal, approval, specification) {
		t.Fatal("canonical terminal identity rejected")
	}

	for _, tc := range []struct {
		name          string
		specification domain.ItemID
		approval      domain.ItemID
	}{
		{
			name:          "empty implementation run",
			specification: "spec--1", approval: "spec-approval--1",
		},
		{
			name:          "same elaboration and implementation run",
			specification: "spec-elaboration-run-1",
			approval:      "spec-approval-elaboration-run-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			approval := approval
			approval.ID = tc.approval
			specification := specification
			specification.ID = domain.ArtifactID(tc.specification)
			if blockedWaitTerminalIdentityMatches(terminal, approval, specification) {
				t.Fatal("inauthentic terminal identity accepted")
			}
		})
	}
}

func TestAttentionItemReadAuthenticatesExecutionFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, admission := seedAdmission(t, nil)
	if _, err := st.db.ExecContext(ctx,
		`UPDATE runs SET body = json_set(body, '$.stages[0].name', 'implement') WHERE id = ?`,
		admission.RunID); err != nil {
		t.Fatal(err)
	}
	outcome := domain.ExecutionOutcome{
		InvocationID: admission.InvocationID, AdmissionID: admission.ID,
		Status: domain.ExecutionOutcomeFailed, Summary: "implementation failed",
		RecordedAt: admission.AdmittedAt.Add(time.Minute),
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionOutcome(ctx, outcome)
	}); err != nil {
		t.Fatal(err)
	}
	runID := admission.RunID
	at := outcome.RecordedAt
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-execution-card-fact", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityUrgent,
		Reason: "implementation failed", RequestedDecision: []domain.Action{domain.ActionRetry},
		ExecutionFailure: &domain.ExecutionFailureFacts{
			Outcome: outcome.Status, Stage: domain.StageNameImplementation,
			InvocationID: admission.InvocationID,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	assertAttentionItemFactReadGate(t, st, item.ID,
		attentionItemFactMutation{"$.execution_failure.invocation_id", "inv-invented"})
}

func TestAttentionItemSuccessorReauthenticatesRestoredExecutionFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, admission := seedAdmission(t, nil)
	if _, err := st.db.ExecContext(ctx,
		`UPDATE runs SET body = json_set(body, '$.stages[0].name', 'implement') WHERE id = ?`,
		admission.RunID); err != nil {
		t.Fatal(err)
	}
	outcome := domain.ExecutionOutcome{
		InvocationID: admission.InvocationID, AdmissionID: admission.ID,
		Status: domain.ExecutionOutcomeFailed, Summary: "implementation failed",
		RecordedAt: admission.AdmittedAt.Add(time.Minute),
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionOutcome(ctx, outcome)
	}); err != nil {
		t.Fatal(err)
	}
	runID := admission.RunID
	at := outcome.RecordedAt
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-restored-execution-card-fact", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityUrgent,
		Reason: "implementation failed", RequestedDecision: []domain.Action{domain.ActionRetry},
		ExecutionFailure: &domain.ExecutionFailureFacts{
			Outcome: outcome.Status, Stage: domain.StageNameImplementation,
			InvocationID: admission.InvocationID,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE attention_items SET body = json_set(body, '$.execution_failure.invocation_id', 'inv-invented') WHERE id = ?`,
		item.ID); err != nil {
		t.Fatal(err)
	}
	successor := item
	successor.ItemVersion++
	successor.ExecutionFailure = nil
	err = st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, successor) })
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("restored execution failure put = %v, want ErrParentKeyMismatch", err)
	}
}

func TestCostCompletenessEnumerationsRejectCopiedRunIDTamper(t *testing.T) {
	t.Run("execution admission", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		st, admission := seedAdmission(t, nil)
		otherRun := domain.Run{
			ID: "run-invented", ProjectID: "proj-1",
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		}
		if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutRun(ctx, otherRun) }); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE execution_admissions SET run_id = 'run-invented' WHERE invocation_id = ?`,
			admission.InvocationID); err != nil {
			t.Fatal(err)
		}
		err := st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.ListRunExecutionAdmissionRecords(ctx, admission.RunID)
			return err
		})
		if !errors.Is(err, errRowInconsistent) {
			t.Fatalf("admission enumeration = %v, want errRowInconsistent", err)
		}
	})

	t.Run("review failure", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		st, err := Open(ctx, t.TempDir()+"/review-failure-card-fact.db", Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		run := domain.Run{
			ID: "run-review-failure-card-fact", ProjectID: "project-1",
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		}
		otherRun := domain.Run{
			ID: "run-invented", ProjectID: run.ProjectID,
			SpecDigest: run.SpecDigest, PolicyDigest: run.PolicyDigest,
		}
		failure := domain.ReviewFailure{
			InvocationID: "review-failure-card-fact", RunID: run.ID, Round: 1,
			BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureConfiguration,
			Reason:     "configuration refused",
			ObservedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		}
		if err := st.Write(ctx, func(tx *WriteTx) error {
			if err := tx.PutRun(ctx, run); err != nil {
				return err
			}
			if err := tx.PutRun(ctx, otherRun); err != nil {
				return err
			}
			return tx.PutReviewFailure(ctx, failure)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE review_failures SET run_id = 'run-invented' WHERE invocation_id = ?`,
			failure.InvocationID); err != nil {
			t.Fatal(err)
		}
		err = st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.ListReviewFailures(ctx, run.ID)
			return err
		})
		if !errors.Is(err, errRowInconsistent) {
			t.Fatalf("review-failure enumeration = %v, want errRowInconsistent", err)
		}
	})
}

type attentionItemFactMutation struct {
	path  string
	value any
}

func assertAttentionItemFactReadGate(
	t *testing.T, st *Store, itemID domain.ItemID, mutations ...attentionItemFactMutation,
) {
	t.Helper()
	ctx := context.Background()
	var originalBody string
	if err := st.db.QueryRowContext(ctx,
		`SELECT body FROM attention_items WHERE id = ?`, itemID).Scan(&originalBody); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range mutations {
		if _, err := st.db.ExecContext(ctx,
			`UPDATE attention_items SET body = json_set(?, ?, ?) WHERE id = ?`,
			originalBody, mutation.path, mutation.value, itemID); err != nil {
			t.Fatal(err)
		}
		reads := []struct {
			name string
			read func(*ReadTx) error
		}{
			{"snapshot", func(tx *ReadTx) error {
				_, _, err := tx.GetAttentionItemSnapshot(ctx, itemID)
				return err
			}},
			{"history", func(tx *ReadTx) error {
				_, err := tx.GetAttentionItemRecord(ctx, itemID)
				return err
			}},
		}
		for _, read := range reads {
			err := st.Read(ctx, read.read)
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("%s %s read = %v, want ErrParentKeyMismatch",
					mutation.path, read.name, err)
			}
		}
	}
}
