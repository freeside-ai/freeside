package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	intakeIntRepo   = "owner/repo"
	intakeIntRepoID = int64(84958515)
	intakeIntIssue  = 7
	intakeIntLabel  = "freeside"
)

var intakeIntTS = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// seedBoundIntakeOccurrence sets up an ordinal-1 present occurrence with a bound
// admission and an open proposal item, entirely through the store's real write
// path, and returns the bound occurrence. Internal (package store) so tests can
// then reach unexported helpers and write tampered shapes no public path emits.
func seedBoundIntakeOccurrence(t *testing.T, ctx context.Context, tx *WriteTx) domain.IntakeOccurrence {
	t.Helper()
	policy, err := domain.NewResolvedPolicy("intake-internal-run", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}})
	if err != nil {
		t.Fatalf("resolved policy: %v", err)
	}
	handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID))
	proposal, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: handle, Intent: domain.RunProposalIntentImplement,
		ExpectedCostUnits: 10, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	if err := tx.PutRun(ctx, domain.Run{
		ID: policy.RunID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
	}); err != nil {
		t.Fatalf("put run: %v", err)
	}
	if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
		t.Fatalf("put resolved policy: %v", err)
	}
	// The mint gate (#740) requires the run's project registered against the
	// occurrence's repository before MintIntakeDeclaration will bind it.
	project, err := domain.NewProject("project-1", intakeIntRepo, intakeIntRepoID)
	if err != nil {
		t.Fatalf("build project: %v", err)
	}
	if err := tx.RegisterProject(ctx, project); err != nil {
		t.Fatalf("register project: %v", err)
	}
	policyArtifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "policy-art-1", Type: domain.ArtifactKindPolicy, Digest: policy.Digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: "inv-policy",
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}, nil)
	if err != nil {
		t.Fatalf("policy artifact: %v", err)
	}
	if err := tx.PutArtifact(ctx, policyArtifact); err != nil {
		t.Fatalf("put policy artifact: %v", err)
	}

	occurrence, _, err := tx.AllocateNextIntakeOccurrence(ctx, intakeIntRepo, intakeIntRepoID, intakeIntIssue, intakeIntLabel, intakeIntTS)
	if err != nil {
		t.Fatalf("allocate occurrence: %v", err)
	}
	if _, err := tx.MintIntakeDeclaration(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1, policy.RunID); err != nil {
		t.Fatalf("mint intake declaration: %v", err)
	}
	instance, _, err := tx.AllocateProposalInstance(ctx, occurrence.ProposalAdmissionKey(), "batch-1", proposal, intakeIntTS)
	if err != nil {
		t.Fatalf("allocate instance: %v", err)
	}
	artifact, err := instance.EvidenceArtifact()
	if err != nil {
		t.Fatalf("evidence artifact: %v", err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID(instance.ID), ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectProposalBatch, ID: "batch-1"},
		Type:    domain.AttentionRunProposal, Priority: domain.PriorityNormal,
		Reason:            "start the accepted work",
		RequestedDecision: []domain.Action{domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze},
		EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
	if err != nil {
		t.Fatalf("attention item: %v", err)
	}
	if err := tx.PutAttentionItem(ctx, item); err != nil {
		t.Fatalf("put item: %v", err)
	}
	if err := tx.BindProposalItem(ctx, domain.ItemID(instance.ID), instance.ID, proposal.Digest); err != nil {
		t.Fatalf("bind proposal item: %v", err)
	}
	bound, err := tx.BindIntakeAdmission(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1, instance.ID, "policy-art-1")
	if err != nil {
		t.Fatalf("bind admission: %v", err)
	}
	return bound
}

// TestIntakeLatchRejectsTamperedNonPresentOpenItem proves the ordinal latch
// re-gates the decoded occurrence state against its admitted item: a row
// tampered to absent while its proposal item stays open cannot release the next
// ordinal, so reconciliation can never admit a second live proposal for one
// labeled issue. The write path forbids this shape (the observation guard), so
// the tampered row is written directly through putIntakeOccurrence — domain
// Validate accepts it because item openness is not visible to the domain, which
// is exactly why the latch must be the boundary that catches it.
func TestIntakeLatchRejectsTamperedNonPresentOpenItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		occurrence := seedBoundIntakeOccurrence(t, ctx, tx)
		// Tamper: persist the occurrence as absent while its item stays open.
		occurrence.State = domain.IntakeOccurrenceAbsent
		if err := tx.putIntakeOccurrence(ctx, occurrence); err != nil {
			return err
		}
		if _, _, err := tx.AllocateNextIntakeOccurrence(ctx, intakeIntRepo, intakeIntRepoID, intakeIntIssue, intakeIntLabel, intakeIntTS); !errors.Is(err, ErrIntakeAdmissionInconsistent) {
			return fmt.Errorf("tampered latch not rejected, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeReGateRejectsTamperedSubject proves the read re-gate re-derives the
// whole admission binding from the occurrence and its proposal and fails closed
// when a stored subject field disagrees. It tampers the project id — a value
// domain Validate accepts (it cross-checks nothing) but the store re-derives from
// the proposal's minted declaration — so a row that decodes cleanly is still
// rejected as inauthentic. The single re-derive-and-compare replaces the
// enumerable per-field re-gate whose gaps drove five review rounds.
func TestIntakeReGateRejectsTamperedSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		occurrence := seedBoundIntakeOccurrence(t, ctx, tx)
		// Tamper: a foreign project the proposal's declaration does not name.
		occurrence.Admission.Subject.ProjectID = "project-foreign"
		if err := tx.putIntakeOccurrence(ctx, occurrence); err != nil {
			return err
		}
		if _, err := tx.GetIntakeOccurrence(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1); !errors.Is(err, ErrIntakeAdmissionInconsistent) {
			return fmt.Errorf("tampered subject not rejected, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeReGateRejectsFalseSupersession proves the read re-gate re-checks a
// stored supersession fact against the authoritative proposal item: a row
// tampered to claim a withdrawal its still-open item never took fails closed, so
// audit and reconciliation can never read a supersession the decision ledger
// contradicts. The write path forbids this shape (it records the fact only on a
// genuine withdrawal), so the tampered row is written directly through
// putIntakeOccurrence.
func TestIntakeReGateRejectsFalseSupersession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		occurrence := seedBoundIntakeOccurrence(t, ctx, tx)
		// Tamper: leave present (item still open), then persist an absent row that
		// falsely claims a supersession. Domain Validate accepts it (a supersession
		// on a non-present admitted occurrence is well-formed); only the item's
		// actual status, invisible to the domain, contradicts it.
		occurrence.State = domain.IntakeOccurrenceAbsent
		occurrence.Supersession = &domain.IntakeSupersession{
			Reason: domain.IntakeSupersededLabelRemoved, RecordedAt: intakeIntTS,
		}
		if err := tx.putIntakeOccurrence(ctx, occurrence); err != nil {
			return err
		}
		if _, err := tx.GetIntakeOccurrence(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1); !errors.Is(err, ErrIntakeAdmissionInconsistent) {
			return fmt.Errorf("false supersession not rejected, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeProposalItemAuthenticatesAdmittedDigest proves the item boundary
// pins the rendered proposal to the digest the occurrence admitted: the
// authentic admitted digest resolves the item, but any other digest (a
// same-instance revision the item might render) fails closed, so a card whose
// content was never admitted cannot be inspected or withdrawn.
func TestIntakeProposalItemAuthenticatesAdmittedDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		occurrence := seedBoundIntakeOccurrence(t, ctx, tx)
		instanceID := occurrence.Admission.ProposalInstanceID
		admitted := occurrence.Admission.ProposalDigest
		if _, err := tx.authenticatedProposalItem(ctx, instanceID, admitted); err != nil {
			return fmt.Errorf("admitted digest rejected: %w", err)
		}
		wrong := domain.Digest(contentaddr.Sum([]byte("never-admitted")))
		if _, err := tx.authenticatedProposalItem(ctx, instanceID, wrong); !errors.Is(err, ErrIntakeAdmissionInconsistent) {
			return fmt.Errorf("non-admitted digest not rejected, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeReGateToleratesUnavailablePolicyArtifact proves the read re-gate
// authenticates the stored binding against durable parents only: when the bound
// policy artifact is removed after admission (an input that has become
// unavailable), GetIntakeOccurrence still returns the authenticated occurrence,
// so #659 can record a subject_input_missing/stale refusal instead of finding
// the occurrence unreadable. Current-input availability moved to the start-time
// refusal (owner-ratified after round 11 #1); admission-time presence is still
// enforced on the write path.
func TestIntakeReGateToleratesUnavailablePolicyArtifact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		seedBoundIntakeOccurrence(t, ctx, tx)
		if _, err := tx.tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, "policy-art-1"); err != nil {
			return err
		}
		got, err := tx.GetIntakeOccurrence(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1)
		if err != nil {
			return fmt.Errorf("occurrence unreadable after its policy artifact was removed: %w", err)
		}
		if got.Admission == nil {
			return errors.New("reconstructed occurrence lost its admission")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeReGateRejectsTamperedPolicyArtifactID proves the extracted
// policy_artifact_id column authenticates the body's recorded id: a body-only
// tamper that swaps the artifact id (to a foreign or missing artifact) no longer
// mirrors the column, so reconstruction fails closed instead of trusting the
// substituted id and later misreading it as a start-time unavailability. The
// policy artifact has no durable parent the read can re-derive it from (it may
// legitimately be gone), so the independent column is the check.
func TestIntakeReGateRejectsTamperedPolicyArtifactID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		occurrence := seedBoundIntakeOccurrence(t, ctx, tx)
		// Tamper the body's artifact id only; the policy_artifact_id column keeps
		// the admitted id. domain Validate accepts any non-empty id with matching
		// digests, so only the column mirror catches the swap.
		occurrence.Admission.Subject.PolicyArtifactID = "art-foreign"
		body, err := encode(occurrence)
		if err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx,
			`UPDATE intake_occurrences SET body = ? WHERE repository_id = ? AND issue_number = ? AND label = ? AND ordinal = ?`,
			body, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1); err != nil {
			return err
		}
		if _, err := tx.GetIntakeOccurrence(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1); !errors.Is(err, errRowInconsistent) {
			return fmt.Errorf("body-only tampered policy artifact id not rejected, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeReGateRejectsTamperedItemType proves the item authentication requires
// the bound card to remain a run proposal over a proposal batch: a tampered
// attention_items row that keeps the id, project, and digest binding but changes
// the subject type still resolves through ProposalForItem, so without the
// semantic-type guard a supersession could withdraw a card that is no longer a
// run proposal. Written directly (the transition gate forbids the shape through
// PutAttentionItem).
func TestIntakeReGateRejectsTamperedItemType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		occurrence := seedBoundIntakeOccurrence(t, ctx, tx)
		itemID := domain.ItemID(occurrence.Admission.ProposalInstanceID)
		item, err := tx.GetAttentionItem(ctx, itemID)
		if err != nil {
			return err
		}
		// Tamper: a structurally valid item whose subject is a run, not a proposal
		// batch (domain Validate accepts both; only intake requires the batch).
		item.Subject = domain.Subject{Type: domain.SubjectRun, ID: item.Subject.ID}
		body, err := encode(item)
		if err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, body, itemID); err != nil {
			return err
		}
		// Forge the decision surface to match, so the intake gate under test
		// is reached rather than the surface re-gate refusing the row first.
		seedDecisionSurface(t, ctx, tx.tx, item)
		if _, err := tx.SupersedeIntakeProposal(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1,
			domain.IntakeSupersededLabelRemoved, domain.IntakeOccurrenceAbsent, intakeIntTS); !errors.Is(err, ErrIntakeAdmissionInconsistent) {
			return fmt.Errorf("tampered item type not rejected, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeReGateRejectsFabricatedRefusal proves the extracted refusal_reason
// column authenticates the decoded refusal fact: a body-only tamper that
// fabricates a refusal (leaving the column null) fails reconstruction, so
// reconciliation cannot consume a forged wip_cap_exhausted / mode_not_authorized
// / input-unavailability result and suppress or misreport intake.
func TestIntakeReGateRejectsFabricatedRefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		occurrence := seedBoundIntakeOccurrence(t, ctx, tx)
		// Tamper the body only to add a refusal; the refusal_reason column stays
		// null (no refusal was recorded), so the mirror no longer matches.
		occurrence.Refusal = &domain.IntakeStartRefusal{
			Reason: domain.IntakeRefusalWIPCapExhausted, RecordedAt: intakeIntTS,
		}
		body, err := encode(occurrence)
		if err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx,
			`UPDATE intake_occurrences SET body = ? WHERE repository_id = ? AND issue_number = ? AND label = ? AND ordinal = ?`,
			body, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1); err != nil {
			return err
		}
		if _, err := tx.GetIntakeOccurrence(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1); !errors.Is(err, errRowInconsistent) {
			return fmt.Errorf("fabricated refusal not rejected, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeReGateRejectsSupersessionOfDecidedProposal proves the supersession
// re-gate authenticates against the decision ledger, not just the item status: a
// start_with_changes also supersedes the original item while recording a real
// decision, so a tampered occurrence that falsely claims an intake withdrawal on
// a decided proposal is rejected. The durable start_with_changes state is
// simulated (item superseded + a decision-ledger row); a genuine intake
// withdrawal records no decision.
func TestIntakeReGateRejectsSupersessionOfDecidedProposal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		occurrence := seedBoundIntakeOccurrence(t, ctx, tx)
		instanceID := occurrence.Admission.ProposalInstanceID
		itemID := domain.ItemID(instanceID)
		// The item is superseded (so the status check passes) AND the decision
		// ledger records the start — the start_with_changes shape the status-only
		// check could not distinguish from an intake withdrawal.
		if _, err := tx.supersedeOpenProposalItem(ctx, instanceID, occurrence.Admission.ProposalDigest); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx,
			`INSERT INTO commands (command_id, item_id, item_version, pr_head_sha, device_id, action, entity_version, as_of_revision, body)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"cmd-swc", string(itemID), 1, "sha", "dev", "start_with_changes", 1, 1, "{}"); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx,
			`INSERT INTO effect_proposal_decisions (instance_id, command_id, action, selected_digest, decided_at)
			 VALUES (?, ?, 'start_with_changes', ?, ?)`,
			string(instanceID), "cmd-swc", string(occurrence.Admission.ProposalDigest), "2026-08-11T12:00:00Z"); err != nil {
			return err
		}
		// Tamper the occurrence: falsely claim a label-removal supersession.
		occurrence.State = domain.IntakeOccurrenceAbsent
		occurrence.Supersession = &domain.IntakeSupersession{
			Reason: domain.IntakeSupersededLabelRemoved, RecordedAt: intakeIntTS,
		}
		if err := tx.putIntakeOccurrence(ctx, occurrence); err != nil {
			return err
		}
		if _, err := tx.GetIntakeOccurrence(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1); !errors.Is(err, ErrIntakeAdmissionInconsistent) {
			return fmt.Errorf("supersession of a decided proposal not rejected, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeReGateRejectsImpossibleSupersessionReason proves the read re-gate
// rejects the one supersession reason/state combination that cannot occur
// legitimately: issue_closed is stamped only when the occurrence becomes closed,
// and closed is terminal, so an issue_closed supersession on an absent occurrence
// is impossible. The reverse (a label_removed supersession on a closed
// occurrence) stays legal, since an absent occurrence may later be observed
// closed, so it is not exercised here.
func TestIntakeReGateRejectsImpossibleSupersessionReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		seedBoundIntakeOccurrence(t, ctx, tx)
		// Legitimately supersede the open card as a label removal (absent).
		occ, err := tx.SupersedeIntakeProposal(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1,
			domain.IntakeSupersededLabelRemoved, domain.IntakeOccurrenceAbsent, intakeIntTS)
		if err != nil {
			return err
		}
		// Tamper the reason to issue_closed while the occurrence stays absent.
		occ.Supersession.Reason = domain.IntakeSupersededIssueClosed
		if err := tx.putIntakeOccurrence(ctx, occ); err != nil {
			return err
		}
		if _, err := tx.GetIntakeOccurrence(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1); !errors.Is(err, ErrIntakeAdmissionInconsistent) {
			return fmt.Errorf("impossible issue_closed+absent supersession not rejected, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeReGateRejectsTamperedSupersessionReason proves the supersession_reason
// column authenticates the decoded cause in the direction the state cannot: a
// closed occurrence legitimately superseded as issue_closed, tampered in the body
// only to label_removed, passes the item, decision-ledger, and reason/state
// checks (label_removed on a closed occurrence is legal via absent->closed), so
// the extracted column is the sole authority that catches the swap.
func TestIntakeReGateRejectsTamperedSupersessionReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		seedBoundIntakeOccurrence(t, ctx, tx)
		occ, err := tx.SupersedeIntakeProposal(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1,
			domain.IntakeSupersededIssueClosed, domain.IntakeOccurrenceClosed, intakeIntTS)
		if err != nil {
			return err
		}
		// Tamper the body reason only; the supersession_reason column keeps
		// issue_closed. label_removed on a closed occurrence is otherwise legal.
		occ.Supersession.Reason = domain.IntakeSupersededLabelRemoved
		body, err := encode(occ)
		if err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx,
			`UPDATE intake_occurrences SET body = ? WHERE repository_id = ? AND issue_number = ? AND label = ? AND ordinal = ?`,
			body, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1); err != nil {
			return err
		}
		if _, err := tx.GetIntakeOccurrence(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1); !errors.Is(err, errRowInconsistent) {
			return fmt.Errorf("body-only tampered supersession reason not rejected, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeReGateRejectsCrossRepositoryProject proves the read re-gate closes
// the case #720's re-gate could not (issue #740): a stored admission whose
// durable declaration names a project registered against a different repository
// than the occurrence is rejected on reconstruction. The binding is otherwise
// authentic — its subject byte-equals what re-derivation produces — so only the
// project→repository check catches it. No write path can reach this state (the
// mint gate and the bind-time derive both refuse it), so the occurrence is
// assembled directly: the declaration is recorded cross-repo through the
// lower-level RecordWorkUnitDeclaration, and the admission is written through
// putIntakeOccurrence.
func TestIntakeReGateRejectsCrossRepositoryProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		policy, err := domain.NewResolvedPolicy("intake-cross-run", []domain.PolicyKey{{
			Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
				Source: domain.ProvenanceOverride,
				Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		}})
		if err != nil {
			return err
		}
		handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID))
		proposal, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
			SubjectHandle: handle, Intent: domain.RunProposalIntentImplement,
			ExpectedCostUnits: 10, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
		}, policy)
		if err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: "project-cross", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		// The declaration's project is registered against a different repository
		// than the occurrence (intakeIntRepoID).
		foreign, err := domain.NewProject("project-cross", "other/repo", intakeIntRepoID+1)
		if err != nil {
			return err
		}
		if err := tx.RegisterProject(ctx, foreign); err != nil {
			return err
		}

		occurrence, _, err := tx.AllocateNextIntakeOccurrence(ctx, intakeIntRepo, intakeIntRepoID, intakeIntIssue, intakeIntLabel, intakeIntTS)
		if err != nil {
			return err
		}
		// Record the cross-repo declaration directly, bypassing the mint gate that
		// would refuse it, so the durable parent the re-gate re-derives from exists.
		issue := occurrence.IssueNumber
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			BoundIssue:          &issue,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, "project-cross", occurrence.RecordedAt)
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		instance, _, err := tx.AllocateProposalInstance(ctx, occurrence.ProposalAdmissionKey(), "batch-1", proposal, intakeIntTS)
		if err != nil {
			return err
		}
		// Hand-build the domain-valid admission the mint would have produced, then
		// write it directly: the subject faithfully mirrors the cross-repo
		// declaration, so the re-gate's byte-equality would pass and only the
		// project→repository check fires.
		occurrence.Admission = &domain.IntakeAdmission{
			AdmissionKey:       occurrence.ProposalAdmissionKey(),
			ProposalInstanceID: instance.ID,
			ProposalDigest:     proposal.Digest,
			Subject: domain.IntakeSubjectBinding{
				ProjectID:            "project-cross",
				WorkUnitID:           declaration.ID,
				SpecificationRunID:   policy.RunID,
				PolicyArtifactID:     "policy-art-1",
				PolicyArtifactDigest: policy.Digest,
				ResolvedPolicyDigest: policy.Digest,
				Source: domain.SpecificationSource{
					Kind: domain.SpecificationSourceIssueSubject,
					IssueSubject: &domain.IssueSubjectRef{
						Repo: occurrence.Repo, RepositoryID: occurrence.RepositoryID, IssueNumber: occurrence.IssueNumber,
					},
				},
			},
		}
		if err := tx.putIntakeOccurrence(ctx, occurrence); err != nil {
			return err
		}
		_, readErr := tx.GetIntakeOccurrence(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1)
		if !errors.Is(readErr, ErrIntakeProjectRepositoryMismatch) {
			return fmt.Errorf("cross-repository binding not rejected for mismatch, got %w", readErr)
		}
		if !errors.Is(readErr, ErrIntakeAdmissionInconsistent) {
			return fmt.Errorf("cross-repository read rejection is not the held-binding class, got %w", readErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeReGateClassifiesCorruptProjectRowAsHold proves a GetProject failure
// that is not ErrNotFound — here a tampered projects row whose column disagrees
// with its body — still surfaces through the read re-gate as
// ErrIntakeAdmissionInconsistent, the durable "hold, don't act" signal. The
// projects row is write-once and undeletable, so any reconstruction failure of
// an authentic binding is authority corruption, not a transient store fault a
// consumer might retry past.
func TestIntakeReGateClassifiesCorruptProjectRowAsHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		seedBoundIntakeOccurrence(t, ctx, tx)
		// Tamper the authentic project's row so its repository_id column no longer
		// matches the body; GetProject then fails the cross-check with
		// errRowInconsistent rather than ErrNotFound.
		if _, err := tx.tx.ExecContext(ctx,
			`UPDATE projects SET repository_id = repository_id + 1 WHERE project_id = ?`,
			"project-1"); err != nil {
			return err
		}
		if _, err := tx.GetIntakeOccurrence(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, 1); !errors.Is(err, ErrIntakeAdmissionInconsistent) {
			return fmt.Errorf("corrupt project row not classified as a durable hold, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
