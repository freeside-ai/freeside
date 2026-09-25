package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestEffectProposalMigrationAppliesFromHead(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0040_")
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if got := rawVersion(t, db); got != 81 {
		t.Fatalf("schema version = %d, want 81", got)
	}
	for _, table := range []string{
		"effect_proposal_instances", "effect_proposal_items", "effect_proposal_revisions",
		"effect_proposal_decisions", "effect_proposal_snoozes", "effect_proposal_policy_approvals",
	} {
		assertTableExists(t, db, table, true)
	}
	// The 0077 rebuild widened the effect_kind CHECK to admit the second
	// registry member while still rejecting any unknown kind, and the batch
	// index and admission_key UNIQUE constraint survived the rebuild.
	var schema string
	if err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='effect_proposal_instances'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schema, "effect_kind IN ('run_proposal', 'source_issue_closure')") {
		t.Fatalf("rebuilt effect_kind CHECK missing both kinds: %s", schema)
	}
	if !strings.Contains(schema, "admission_key           TEXT NOT NULL UNIQUE") {
		t.Fatalf("rebuilt table lost admission_key UNIQUE: %s", schema)
	}
	var indexCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='effect_proposal_instances_batch'`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatalf("effect_proposal_instances_batch index count = %d, want 1", indexCount)
	}
}

func TestProposalInstanceReconstructionRejectsTampering(t *testing.T) {
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	policy, err := domain.NewResolvedPolicy("proposal-policy-run", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	handles := []domain.OpaqueSubjectHandle{domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID))}
	proposal, err := domain.NewEffectProposal(domain.EffectTaskProposal, domain.TaskProposalParameters{
		SubjectHandle: handles[0], Intent: domain.TaskProposalIntentImplement,
		ExpectedCostUnits: 10, Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	var instance domain.ProposalInstance
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, "project-1", time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "event-1"},
			"batch-1", proposal, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	mismatched, err := domain.NewEffectProposal(domain.EffectTaskProposal, domain.TaskProposalParameters{
		SubjectHandle: handles[0], Intent: domain.TaskProposalIntentImplement,
		ExpectedCostUnits: 10, Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 2},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		_, _, err := tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "event-scope-mismatch"},
			"batch-1", mismatched, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
		return err
	})
	if !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("direct allocation scope mismatch error = %v, want ErrEffectProposalInconsistent", err)
	}

	for _, tc := range []struct {
		name   string
		mutate string
		want   error
	}{
		{"column body mismatch", `UPDATE effect_proposal_instances SET content_digest = 'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' WHERE instance_id = ?`, errRowInconsistent},
		{"unknown body field", `UPDATE effect_proposal_instances SET body = json_set(body, '$.unknown', true) WHERE instance_id = ?`, nil},
		{"forged trusted digest", `UPDATE effect_proposal_instances SET body = json_set(body, '$.proposal.digest', 'sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc') WHERE instance_id = ?`, domain.ErrEffectProposalDigestMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx, tc.mutate, instance.ID); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetProposalInstance(ctx, instance.ID)
				return err
			})
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("read error = %v, want %v", err, tc.want)
			}
			// Restore the canonical row before the next independent mutation.
			key, _ := instance.Admission.String()
			body, _ := marshalProposalInstance(instance)
			if _, restoreErr := st.db.ExecContext(ctx, `UPDATE effect_proposal_instances SET
				admission_key = ?, proposal_batch_id = ?, effect_kind = ?, content_digest = ?,
				resolved_policy_run_id = ?, resolved_policy_digest = ?, subject_handle = ?,
				created_at = ?, body = ? WHERE instance_id = ?`,
				key, instance.ProposalBatchID, instance.Proposal.Kind, instance.Proposal.Digest,
				instance.Proposal.ResolvedPolicyRunID, instance.Proposal.ResolvedPolicyDigest,
				instance.Proposal.TaskProposal.SubjectHandle, formatTime(instance.CreatedAt), string(body), instance.ID); restoreErr != nil {
				t.Fatal(restoreErr)
			}
		})
	}

	t.Run("coordinated initial scope tamper", func(t *testing.T) {
		tamperedInstance := instance
		tamperedInstance.Proposal = mismatched
		body, err := marshalProposalInstance(tamperedInstance)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_instances SET
			content_digest = ?, body = ? WHERE instance_id = ?`, mismatched.Digest, string(body), instance.ID); err != nil {
			t.Fatal(err)
		}
		err = st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetProposalInstance(ctx, instance.ID)
			return err
		})
		if !errors.Is(err, domain.ErrEffectProposalInconsistent) {
			t.Fatalf("tampered initial proposal error = %v, want ErrEffectProposalInconsistent", err)
		}
		artifact, err := tamperedInstance.EvidenceArtifact()
		if err != nil {
			t.Fatal(err)
		}
		err = st.Read(ctx, func(tx *ReadTx) error {
			return tx.gateEffectProposalArtifact(ctx, artifact)
		})
		if !errors.Is(err, domain.ErrEffectProposalInconsistent) {
			t.Fatalf("tampered initial carrier error = %v, want ErrEffectProposalInconsistent", err)
		}
		body, err = marshalProposalInstance(instance)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_instances SET
			content_digest = ?, body = ? WHERE instance_id = ?`, proposal.Digest, string(body), instance.ID); err != nil {
			t.Fatal(err)
		}
	})

	historical, err := domain.NewResolvedPolicy("historical-policy-run", policy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: historical.RunID, ProjectID: "project-1",
			SpecDigest: "sha256:historical-spec", PolicyDigest: historical.Digest,
		}); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, historical)
	}); err != nil {
		t.Fatal(err)
	}
	staleProposal, err := domain.NewEffectProposal(domain.EffectTaskProposal, *instance.Proposal.TaskProposal, historical)
	if err != nil {
		t.Fatal(err)
	}
	staleInstance := instance
	staleInstance.Proposal = staleProposal
	body, err := marshalProposalInstance(staleInstance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_instances SET
		content_digest = ?, resolved_policy_run_id = ?, resolved_policy_digest = ?, body = ?
		WHERE instance_id = ?`, staleProposal.Digest, staleProposal.ResolvedPolicyRunID,
		staleProposal.ResolvedPolicyDigest, string(body), instance.ID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetProposalInstance(ctx, instance.ID)
		return err
	})
	if !errors.Is(err, domain.ErrProposalPolicyMismatch) {
		t.Fatalf("historical proposal policy error = %v, want ErrProposalPolicyMismatch", err)
	}
	staleArtifact, err := staleInstance.EvidenceArtifact()
	if err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		return tx.gateEffectProposalArtifact(ctx, staleArtifact)
	})
	if !errors.Is(err, domain.ErrProposalPolicyMismatch) {
		t.Fatalf("historical proposal carrier error = %v, want ErrProposalPolicyMismatch", err)
	}
}

func TestProposalLedgerRejectsMismatchedCommandAuthority(t *testing.T) {
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	policy, err := domain.NewResolvedPolicy("proposal-policy-run", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	handles := []domain.OpaqueSubjectHandle{domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID))}
	proposal, err := domain.NewEffectProposal(domain.EffectTaskProposal, domain.TaskProposalParameters{
		SubjectHandle: handles[0], Intent: domain.TaskProposalIntentImplement,
		ExpectedCostUnits: 10, Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	revised, err := domain.NewEffectProposal(domain.EffectTaskProposal, domain.TaskProposalParameters{
		SubjectHandle: handles[0], Intent: domain.TaskProposalIntentImplement,
		ExpectedCostUnits: 20, Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	var instance domain.ProposalInstance
	var item domain.AttentionItem
	err = st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, "project-1", at.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "event-1"},
			"batch-1", proposal, at)
		if err != nil {
			return err
		}
		recipe := domain.EffectProposalRecipeDigest
		forged, err := domain.NewArtifact(domain.ArtifactInput{
			ID: "effect-proposal/forged", Type: domain.ArtifactKindEvidence, Digest: proposal.Digest,
			Provenance: domain.Provenance{
				ProducerClass:        domain.ProducerDaemon,
				ProducerInvocationID: domain.InvocationID("effect-proposal/" + string(instance.ID)),
				HeadBinding:          domain.HeadIndependent, VerificationRecipeDigest: &recipe,
				SensitivityClass: domain.SensitivityNormal,
			},
			Metadata: runMeta(),
		}, map[domain.Digest]bool{recipe: true})
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, forged); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
			t.Fatalf("forged proposal carrier error = %v, want ErrEffectProposalInconsistent", err)
		}
		artifact, err := instance.EvidenceArtifact()
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		item, err = domain.NewAttentionItem(domain.AttentionItemInput{
			ID: domain.ItemID(instance.ID), ProjectID: "project-1",
			Subject: domain.Subject{Type: domain.SubjectProposalBatch, ID: "batch-1"},
			Type:    domain.AttentionTaskProposal, Priority: domain.PriorityNormal,
			Reason: "start the accepted work", RequestedDecision: []domain.Action{
				domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze,
			},
			EvidenceSnapshot: []domain.Artifact{artifact}, ItemVersion: 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
		}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		if err := tx.BindProposalItem(ctx, item.ID, instance.ID, proposal.Digest, nil); err != nil {
			return err
		}
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "command-start", DeviceID: "device-1", ItemID: item.ID,
			ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests, Action: domain.ActionStart,
		})
		if err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		wrongDigest := revised.Digest
		for name, attempt := range map[string]func() error{
			"decision action": func() error {
				return tx.RecordProposalDecision(ctx, instance.ID, command.CommandID, domain.ActionDecline, nil, at)
			},
			"decision digest": func() error {
				return tx.RecordProposalDecision(ctx, instance.ID, command.CommandID, domain.ActionStart, &wrongDigest, at)
			},
			"revision action": func() error {
				return tx.PutProposalRevision(ctx, instance, proposal, revised, command.CommandID, at)
			},
			"snooze action": func() error {
				return tx.RecordProposalSnooze(ctx, instance.ID, command.CommandID, at.Add(time.Hour), at)
			},
		} {
			if err := attempt(); !errors.Is(err, domain.ErrTransitionCommandMismatch) {
				t.Fatalf("%s error = %v, want ErrTransitionCommandMismatch", name, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	err = st.Write(ctx, func(tx *WriteTx) error {
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "command-revision", DeviceID: "device-1", ItemID: item.ID,
			ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests,
			Action: domain.ActionStartWithChanges,
			Message: `{"intent":"implement_subject","expected_cost_units":20,` +
				`"scope":{"component_count":1,"declared_path_count":1,"touches_control_plane":false}}`,
		})
		if err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		return tx.PutProposalRevision(ctx, instance, proposal, revised, command.CommandID, at)
	})
	if err != nil {
		t.Fatal(err)
	}
	revisionInstance := instance
	revisionInstance.Proposal = revised
	revisionArtifact, err := revisionInstance.EvidenceArtifact()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("revision authentication does not recurse through authoring item evidence", func(t *testing.T) {
		rebound := item
		rebound.EvidenceSnapshot = []domain.Artifact{revisionArtifact}
		rebound.ArtifactDigests = []domain.Digest{revisionArtifact.Digest}
		reboundBody, err := encode(rebound)
		if err != nil {
			t.Fatal(err)
		}
		originalBody, err := encode(item)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, reboundBody, item.ID); err != nil {
			t.Fatal(err)
		}
		err = st.Read(ctx, func(tx *ReadTx) error {
			return tx.gateEffectProposalArtifact(ctx, revisionArtifact)
		})
		if !errors.Is(err, errRowInconsistent) {
			t.Fatalf("revision rebound through authoring item error = %v, want row inconsistency", err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, originalBody, item.ID); err != nil {
			t.Fatal(err)
		}
	})
	substitutedPrior, err := domain.NewEffectProposal(domain.EffectTaskProposal, domain.TaskProposalParameters{
		SubjectHandle: handles[0], Intent: domain.TaskProposalIntentImplement,
		ExpectedCostUnits: 15, Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_items SET content_digest = ? WHERE item_id = ?`,
		substitutedPrior.Digest, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_revisions SET supersedes_digest = ?
		WHERE command_id = 'command-revision'`, substitutedPrior.Digest); err != nil {
		t.Fatal(err)
	}
	substitutedInstance := instance
	substitutedInstance.Proposal = substitutedPrior
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, _, err := tx.authenticatedProposalRevision(ctx, substitutedInstance, revised.Digest)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("revision with substituted prior error = %v, want row inconsistency", err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_items SET content_digest = ? WHERE item_id = ?`,
		proposal.Digest, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_revisions SET supersedes_digest = ?
		WHERE command_id = 'command-revision'`, proposal.Digest); err != nil {
		t.Fatal(err)
	}
	historical, err := domain.NewResolvedPolicy("historical-proposal-policy-run", policy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	historicalRevision, err := domain.NewEffectProposal(
		domain.EffectTaskProposal, *revised.TaskProposal, historical)
	if err != nil {
		t.Fatal(err)
	}
	historicalBody, err := historicalRevision.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_revisions
		SET content_digest = ?, body = ? WHERE command_id = 'command-revision'`,
		historicalRevision.Digest, string(historicalBody)); err != nil {
		t.Fatal(err)
	}
	historicalRevisionInstance := instance
	historicalRevisionInstance.Proposal = historicalRevision
	historicalRevisionArtifact, err := historicalRevisionInstance.EvidenceArtifact()
	if err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		return tx.gateEffectProposalArtifact(ctx, historicalRevisionArtifact)
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("historical revision carrier error = %v, want row inconsistency", err)
	}
	currentRevisionInstance := instance
	currentRevisionInstance.Proposal = revised
	currentRevisionArtifact, err := currentRevisionInstance.EvidenceArtifact()
	if err != nil {
		t.Fatal(err)
	}
	currentRevisionBody, err := revised.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_revisions
		SET content_digest = ?, body = ?, command_id = 'command-start'
		WHERE command_id = 'command-revision'`, revised.Digest, string(currentRevisionBody)); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		return tx.gateEffectProposalArtifact(ctx, currentRevisionArtifact)
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("unauthorized current-policy revision carrier error = %v, want row inconsistency", err)
	}

	until := at.Add(time.Hour)
	err = st.Write(ctx, func(tx *WriteTx) error {
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "command-snooze", DeviceID: "device-1", ItemID: item.ID,
			ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests,
			Action: domain.ActionSnooze, Message: formatTime(until),
		})
		if err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		if err := tx.RecordProposalSnooze(ctx, instance.ID, command.CommandID, until, at); err != nil {
			return err
		}
		next := item
		next.ItemVersion++
		return tx.PutAttentionItem(ctx, next)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate string
		args   []any
	}{
		{"command action binding", `UPDATE effect_proposal_snoozes SET command_id = 'command-start' WHERE command_id = 'command-snooze'`, nil},
		{"command message binding", `UPDATE effect_proposal_snoozes SET snooze_until = ? WHERE command_id = 'command-snooze'`, []any{formatTime(until.Add(time.Hour))}},
		{"timing order", `UPDATE effect_proposal_snoozes SET created_at = snooze_until WHERE command_id = 'command-snooze'`, nil},
		{"premature release", `UPDATE effect_proposal_snoozes SET released_at = ? WHERE command_id = 'command-snooze'`, []any{formatTime(at)}},
		{"future release", `UPDATE effect_proposal_snoozes SET released_at = ? WHERE command_id = 'command-snooze'`, []any{formatTime(until.Add(time.Hour))}},
	} {
		t.Run("snooze "+tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx, tc.mutate, tc.args...); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				if _, err := tx.ProposalSnoozed(ctx, item.ID, at.Add(time.Minute)); err != nil {
					return err
				}
				_, err := tx.ProposalSnoozeReleasePending(ctx, at.Add(time.Minute))
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("tampered snooze error = %v, want row inconsistency", err)
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_snoozes SET
				command_id = 'command-snooze', snooze_until = ?, created_at = ?, released_at = NULL`,
				formatTime(until), formatTime(at)); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("snooze item binding digest", func(t *testing.T) {
		if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_items SET content_digest = ? WHERE item_id = ?`,
			revised.Digest, item.ID); err != nil {
			t.Fatal(err)
		}
		err := st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.ProposalSnoozed(ctx, item.ID, at.Add(time.Minute))
			return err
		})
		if !errors.Is(err, errRowInconsistent) {
			t.Fatalf("tampered snooze item binding error = %v, want row inconsistency", err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_items SET content_digest = ? WHERE item_id = ?`,
			proposal.Digest, item.ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("snooze item version transition", func(t *testing.T) {
		if _, err := st.db.ExecContext(ctx, `UPDATE attention_items SET body = json_set(body, '$.item_version', 1) WHERE id = ?`,
			item.ID); err != nil {
			t.Fatal(err)
		}
		err := st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.ProposalSnoozed(ctx, item.ID, at.Add(time.Minute))
			return err
		})
		if !errors.Is(err, errRowInconsistent) {
			t.Fatalf("missing snooze item transition error = %v, want row inconsistency", err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE attention_items SET body = json_set(body, '$.item_version', 2) WHERE id = ?`,
			item.ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("snooze command item pr head", func(t *testing.T) {
		rollback := errors.New("rollback pr-head tamper")
		err := st.Write(ctx, func(tx *WriteTx) error {
			current, err := tx.GetAttentionItem(ctx, item.ID)
			if err != nil {
				return err
			}
			current.ItemVersion++
			current.PRHeadSHA = "forged"
			if err := tx.PutAttentionItem(ctx, current); err != nil {
				return err
			}
			if _, err := tx.ProposalSnoozed(ctx, item.ID, at.Add(time.Minute)); !errors.Is(err, errRowInconsistent) {
				t.Fatalf("tampered snooze command item error = %v, want row inconsistency", err)
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatalf("rollback error = %v", err)
		}
	})

	err = st.Write(ctx, func(tx *WriteTx) error {
		current, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "command-delayed-snooze", DeviceID: "device-1", ItemID: item.ID,
			ItemVersion: current.ItemVersion, PRHeadSHA: current.PRHeadSHA,
			ArtifactDigests: current.ArtifactDigests, Action: domain.ActionSnooze,
			Message: formatTime(until.Add(2 * time.Hour)),
		})
		if err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		next := current
		next.ItemVersion++
		if err := tx.PutAttentionItem(ctx, next); err != nil {
			return err
		}
		err = tx.RecordProposalSnooze(ctx, instance.ID, command.CommandID, until.Add(2*time.Hour), at)
		if !errors.Is(err, domain.ErrTransitionCommandMismatch) {
			t.Fatalf("delayed snooze error = %v, want ErrTransitionCommandMismatch", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestClosureApprovalRejectsCorruptedDeclineRow proves ClosureApprovalForInstance
// re-gates the authoring command's action against the ledger row. A durable
// decision row corrupted from decline to approve (with the item's bound digest)
// still names the original decline command; the read must refuse to mint an
// approval from it rather than trust the tampered action, matching the write
// path's command-authority check.
func TestClosureApprovalRejectsCorruptedDeclineRow(t *testing.T) {
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})

	policy, err := domain.NewResolvedPolicy("closure-approval-policy-run", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID))
	const repo, issue = "owner/repo", 7
	const repoID = int64(123)
	proposedTarget := domain.IssueSubjectRef{Repo: repo, RepositoryID: repoID, IssueNumber: issue}
	source := domain.SpecificationSource{
		Kind: domain.SpecificationSourceIssueSubject, IssueSubject: &proposedTarget,
	}
	proposal, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle:  handle,
		Source:         domain.ClosableSource{Present: true, Provenance: domain.ClosureProvenanceVerified, Repo: repo, RepositoryID: repoID, IssueNumber: issue},
		ProposedTarget: proposedTarget,
		Origin:         domain.ClosureFlagOriginProposeSite,
		Resolves:       true,
	}, policy)
	if err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	merge := domain.ProspectiveMerge{
		PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("b", 64)),
		CandidateHeadSHA:    "head-aaa",
		BaseRef:             "main",
		BaseSHA:             "base-000",
	}
	var instance domain.ProposalInstance
	var item domain.AttentionItem
	err = st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{ID: "proj-1", Repo: repo, RepositoryID: repoID}); err != nil {
			return err
		}
		task, err := tx.GetOrCreateTask(ctx, "proj-1", source)
		if err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: "proj-1", TaskID: task.ID,
			SpecDigest: "sha256:spec", PolicyDigest: policy.Digest, Stages: []domain.Stage{},
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, "proj-1", at.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "closure-event"},
			"batch-closure", proposal, at)
		if err != nil {
			return err
		}
		artifact, err := instance.EvidenceArtifact()
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		item, err = domain.NewAttentionItem(domain.AttentionItemInput{
			ID:        domain.ItemID(string(instance.ID) + "/effect"),
			ProjectID: "proj-1",
			Subject:   domain.Subject{Type: domain.SubjectProposalBatch, ID: domain.SubjectID(instance.ProposalBatchID)},
			Type:      domain.AttentionEffectProposal, Priority: domain.PriorityNormal,
			Reason: "Decide the proposed effect on the source issue",
			RequestedDecision: []domain.Action{
				domain.ActionApprove, domain.ActionApproveWithChanges,
				domain.ActionDecline, domain.ActionSnooze,
			},
			EvidenceSnapshot: []domain.Artifact{artifact}, ItemVersion: 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
			PRHeadSHA: merge.CandidateHeadSHA,
		}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		if err := tx.BindProposalItem(ctx, item.ID, instance.ID, proposal.Digest, &merge); err != nil {
			return err
		}
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "command-decline", DeviceID: "device-1", ItemID: item.ID,
			ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
			ArtifactDigests: item.ArtifactDigests, Action: domain.ActionDecline,
		})
		if err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		return tx.RecordProposalDecision(ctx, instance.ID, command.CommandID, domain.ActionDecline, nil, at)
	})
	if err != nil {
		t.Fatal(err)
	}

	// A genuine decline yields no approval.
	if approval := closureApproval(t, ctx, st, instance.ID); approval != nil {
		t.Fatalf("declined instance yielded an approval: %+v", approval)
	}

	// Corrupt the durable row to approve with the item's bound digest, leaving
	// the decline command in place. The action re-gate must reject it.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE effect_proposal_decisions SET action = 'approve', selected_digest = ? WHERE instance_id = ?`,
		proposal.Digest, instance.ID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ClosureApprovalForInstance(ctx, instance.ID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("corrupted decline-to-approve row error = %v, want row inconsistency", err)
	}
}

func closureApproval(t *testing.T, ctx context.Context, st *Store, instanceID domain.ProposalInstanceID) *domain.ClosureApproval {
	t.Helper()
	var approval *domain.ClosureApproval
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		approval, err = tx.ClosureApprovalForInstance(ctx, instanceID)
		return err
	}); err != nil {
		t.Fatalf("ClosureApprovalForInstance: %v", err)
	}
	return approval
}
