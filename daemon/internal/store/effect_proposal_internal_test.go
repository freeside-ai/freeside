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
	if got := rawVersion(t, db); got != 66 {
		t.Fatalf("schema version = %d, want 66", got)
	}
	for _, table := range []string{
		"effect_proposal_instances", "effect_proposal_items", "effect_proposal_revisions",
		"effect_proposal_decisions", "effect_proposal_snoozes",
	} {
		assertTableExists(t, db, table, true)
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
	proposal, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: handles[0], Intent: domain.RunProposalIntentImplement,
		ExpectedCostUnits: 10, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
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
	mismatched, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: handles[0], Intent: domain.RunProposalIntentImplement,
		ExpectedCostUnits: 10, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 2},
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
				instance.Proposal.RunProposal.SubjectHandle, formatTime(instance.CreatedAt), string(body), instance.ID); restoreErr != nil {
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
	staleProposal, err := domain.NewEffectProposal(domain.EffectRunProposal, *instance.Proposal.RunProposal, historical)
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
	proposal, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: handles[0], Intent: domain.RunProposalIntentImplement,
		ExpectedCostUnits: 10, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	revised, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: handles[0], Intent: domain.RunProposalIntentImplement,
		ExpectedCostUnits: 20, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
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
			Type:    domain.AttentionRunProposal, Priority: domain.PriorityNormal,
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
		if err := tx.BindProposalItem(ctx, item.ID, instance.ID, proposal.Digest); err != nil {
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
	substitutedPrior, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: handles[0], Intent: domain.RunProposalIntentImplement,
		ExpectedCostUnits: 15, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
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
		domain.EffectRunProposal, *revised.RunProposal, historical)
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
