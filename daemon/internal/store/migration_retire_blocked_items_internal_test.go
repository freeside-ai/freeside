package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/domain/domaintest"
)

// plantClosedBlockedWait stores a run, its specification approval, and a
// superseded blocked item waiting on that approval, and returns the blocked
// item's ID. The name keeps one fixture's rows apart from another's.
func plantClosedBlockedWait(t *testing.T, ctx context.Context, st *Store, name string) domain.ItemID {
	t.Helper()
	at := time.Date(2026, 10, 10, 9, 26, 0, 0, time.UTC)
	run := domain.Run{
		ID: domain.RunID("run-retire-blocked-" + name), ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	runID := run.ID
	subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID}
	// The store's specification-approval gate binds these identities: the
	// artifact and the approval are named for the implementation run and
	// iteration, and the specification run's terminal record names them both.
	invocationID := domain.SpecificationInvocationID(run.ID, 1)
	implementation := "implementation-retire-blocked-" + name + "-1"
	specification, err := domain.NewArtifact(domain.ArtifactInput{
		ID: domain.ArtifactID("spec-" + implementation), Type: domain.ArtifactKindSpecification,
		Digest: domain.Digest("sha256:specification-" + name),
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: invocationID,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID("spec-approval-" + implementation), ProjectID: run.ProjectID,
		Subject: subject,
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason: "approve the specification", RequestedDecision: []domain.Action{domain.ActionApprove},
		AgentClaims: []domain.AgentClaim{{
			Label: "Specification", Artifact: specification.ID, Digest: specification.Digest,
			Provenance: specification.Provenance,
			Metadata:   claimMeta(domain.EvidenceMediaImagePNG),
		}},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	approvalID := approval.ID
	raised := at.Add(7 * time.Minute)
	blocked, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "blocked-" + approval.ID, ProjectID: run.ProjectID,
		Subject: subject,
		Type:    domain.AttentionBlocked, Priority: domain.PriorityNormal,
		// The characters Go's JSON encoder escapes prove json_set's
		// re-serialization keeps the body decodable.
		Reason: "Specification approval has been waiting & <still> is.", RequestedDecision: []domain.Action{},
		BlockedOn: &domain.BlockedWait{
			Kind: domain.BlockedWaitSpecApproval, Since: at, ItemID: &approvalID,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &raised, Status: domain.StatusSuperseded,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := json.Marshal(blockedWaitSpecificationTerminal{
		InvocationID: invocationID, Iteration: 1, Status: "completed",
		ResearchArtifactIDs: []domain.ArtifactID{},
		SpecArtifactID:      &specification.ID, ApprovalItemID: &approvalID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		_, _, err := tx.RecordInbox(ctx, string(invocationID), blockedWaitSpecificationTerminalKind, terminal)
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
		if err := putTestAttentionItem(ctx, tx, &approval); err != nil {
			return err
		}
		blocked = domaintest.MustConclude(blocked)
		return putTestAttentionItem(ctx, tx, &blocked)
	}); err != nil {
		t.Fatal(err)
	}
	return blocked.ID
}

type blockedRow struct {
	status, bodyStatus          string
	itemVersion                 int64
	entityVersion, asOfRevision int64
	body                        string
}

func readBlockedRow(t *testing.T, ctx context.Context, db *sql.DB, id domain.ItemID) blockedRow {
	t.Helper()
	var row blockedRow
	if err := db.QueryRowContext(ctx, `SELECT status, json_extract(body, '$.status'),
		json_extract(body, '$.item_version'), entity_version, as_of_revision, body
		FROM attention_items WHERE id = ?`, id).Scan(
		&row.status, &row.bodyStatus, &row.itemVersion, &row.entityVersion, &row.asOfRevision, &row.body,
	); err != nil {
		t.Fatal(err)
	}
	return row
}

func attentionItemCount(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
