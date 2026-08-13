package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	intakeRepo   = "owner/repo"
	intakeRepoID = int64(84958515)
	intakeIssue  = 7
	intakeLabel  = "freeside"
)

var intakeStoreTS = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

const intakePolicyArtifactID = domain.ArtifactID("policy-art-1")

// putIntakePolicyArtifact seeds the policy artifact an intake subject binding
// names. Its digest is the resolved-policy digest, since the policy artifact is
// the resolved policy's content (the elaboration invariant the re-gate
// enforces), so both must equal the run's resolved-policy digest.
func putIntakePolicyArtifact(t *testing.T, ctx context.Context, tx *store.WriteTx, digest domain.Digest) {
	t.Helper()
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: intakePolicyArtifactID, Type: domain.ArtifactKindPolicy, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: "inv-intake-policy",
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
	}, nil)
	if err != nil {
		t.Fatalf("build policy artifact: %v", err)
	}
	if err := tx.PutArtifact(ctx, artifact); err != nil {
		t.Fatalf("put policy artifact: %v", err)
	}
}

// putIntakeProposalPolicy seeds the run, resolved policy, and policy artifact a
// label admission mints against. It deliberately records no work-unit
// declaration: BindIntakeAdmission mints the declaration for the occurrence's own
// issue, so the fixture that pre-recorded one would race the mint.
func putIntakeProposalPolicy(t *testing.T, ctx context.Context, tx *store.WriteTx, policy domain.ResolvedPolicy) {
	t.Helper()
	if err := tx.PutRun(ctx, domain.Run{
		ID: policy.RunID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
	}); err != nil {
		t.Fatalf("put run: %v", err)
	}
	if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
		t.Fatalf("put resolved policy: %v", err)
	}
	putIntakePolicyArtifact(t, ctx, tx, policy.Digest)
}

// TestIntakeOccurrenceLatchAndTransitions proves the allocation latch and the
// per-occurrence state machine across the store boundary: a present label is
// already active (no new ordinal), only an absence-then-presence allocates the
// next ordinal, same-state observation is idempotent, and an illegal transition
// is refused.
func TestIntakeOccurrenceLatchAndTransitions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})

	err := st.Write(ctx, func(tx *store.WriteTx) error {
		first, allocated, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS)
		if err != nil || !allocated || first.Ordinal != 1 || first.State != domain.IntakeOccurrencePresent {
			return errors.New("first allocation is not a present ordinal 1")
		}
		// A still-present label re-allocates to the same active occurrence.
		same, allocated, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS)
		if err != nil || allocated || same.Ordinal != 1 {
			return errors.New("present label allocated a new ordinal")
		}
		// Idempotent same-state observation.
		if _, err := tx.RecordIntakeObservation(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, domain.IntakeOccurrencePresent, intakeStoreTS); err != nil {
			return err
		}
		// Observe the label gone.
		absent, err := tx.RecordIntakeObservation(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, domain.IntakeOccurrenceAbsent, intakeStoreTS)
		if err != nil || absent.State != domain.IntakeOccurrenceAbsent {
			return errors.New("observation to absent failed")
		}
		// An absent occurrence never returns to present in place.
		if _, err := tx.RecordIntakeObservation(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, domain.IntakeOccurrencePresent, intakeStoreTS); !errors.Is(err, store.ErrImmutableConflict) {
			return errors.New("absent -> present was not refused")
		}
		// Label reappears: the next ordinal is allocated.
		next, allocated, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS)
		if err != nil || !allocated || next.Ordinal != 2 || next.State != domain.IntakeOccurrencePresent {
			return errors.New("reappearing label did not allocate ordinal 2")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// mintAndAllocateIntakeProposal mints the occurrence's work-unit declaration
// (the admission-transaction step that lets the proposal's subject handle
// resolve) and allocates the proposal instance under the occurrence's derived
// key, returning the instance id the label admission binds. policy/proposal must
// already be seeded via putIntakeProposalPolicy.
func mintAndAllocateIntakeProposal(
	t *testing.T, ctx context.Context, tx *store.WriteTx,
	occurrence domain.IntakeOccurrence, proposal domain.EffectProposal,
) domain.ProposalInstanceID {
	t.Helper()
	if _, err := tx.MintIntakeDeclaration(ctx, occurrence.RepositoryID, occurrence.IssueNumber,
		occurrence.Label, occurrence.Ordinal, proposal.ResolvedPolicyRunID); err != nil {
		t.Fatalf("mint intake declaration: %v", err)
	}
	instance, inserted, err := tx.AllocateProposalInstance(ctx, occurrence.ProposalAdmissionKey(), "batch-1", proposal, intakeStoreTS)
	if err != nil || !inserted {
		t.Fatalf("allocate proposal under occurrence key: %v inserted=%v", err, inserted)
	}
	return instance.ID
}

// TestIntakeAdmissionBindingAndReGate proves the minted admission binding
// round-trips through the returned-object re-gate, converges on replay, and
// carries the issue_subject source minted from the occurrence.
func TestIntakeAdmissionBindingAndReGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	policy, proposal, _ := proposalFixture(t)

	err := st.Write(ctx, func(tx *store.WriteTx) error {
		putIntakeProposalPolicy(t, ctx, tx, policy)
		occurrence, _, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS)
		if err != nil {
			return err
		}
		instanceID := mintAndAllocateIntakeProposal(t, ctx, tx, occurrence, proposal)
		if _, err := tx.BindIntakeAdmission(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, instanceID, intakePolicyArtifactID); err != nil {
			return err
		}
		// Replay converges.
		if _, err := tx.BindIntakeAdmission(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, instanceID, intakePolicyArtifactID); err != nil {
			return errors.New("admission replay did not converge")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The bound occurrence reconstructs through the re-gate with the minted
	// subject naming its own issue.
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetIntakeOccurrence(ctx, intakeRepoID, intakeIssue, intakeLabel, 1)
		if err != nil {
			return err
		}
		if got.Admission == nil || got.Admission.Subject.Source.Kind != domain.ElaborationSourceIssueSubject {
			return errors.New("reconstructed occurrence lost its admission")
		}
		ref := got.Admission.Subject.Source.IssueSubject
		if ref == nil || ref.RepositoryID != intakeRepoID || ref.IssueNumber != intakeIssue {
			return errors.New("minted subject does not name the occurrence's issue")
		}
		if got.Admission.Subject.WorkUnitID != domain.WorkUnitIDForRun(policy.RunID) {
			return errors.New("minted subject work unit is not the proposal's run")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestIntakeAdmissionRejectsForeignProposal proves the write re-gate refuses a
// binding whose proposal instance is real but was admitted under a different
// occurrence's key, failing atomically at the write instead of committing a row
// that every later read would reject as durably unreadable.
func TestIntakeAdmissionRejectsForeignProposal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	policy, proposal, _ := proposalFixture(t)

	err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := putProposalPolicy(t, ctx, tx, policy); err != nil {
			return err
		}
		occurrence, _, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS)
		if err != nil {
			return err
		}
		// A real proposal exists under the occurrence's own key (so the
		// admission_key FK passes), but the binding points its instance id at a
		// foreign proposal admitted under a different occurrence's key.
		if _, _, err := tx.AllocateProposalInstance(ctx, occurrence.ProposalAdmissionKey(), "batch-1", proposal, intakeStoreTS); err != nil {
			return err
		}
		foreign := occurrence
		foreign.Ordinal = 99
		foreignInstance, _, err := tx.AllocateProposalInstance(ctx, foreign.ProposalAdmissionKey(), "batch-1", proposal, intakeStoreTS)
		if err != nil {
			return err
		}
		if _, bindErr := tx.BindIntakeAdmission(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, foreignInstance.ID, intakePolicyArtifactID); !errors.Is(bindErr, store.ErrIntakeAdmissionInconsistent) {
			return fmt.Errorf("foreign binding not rejected at write, got %w", bindErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeAdmissionAuthenticatesPolicyArtifact proves BindIntakeAdmission
// authenticates the one named elaboration input it cannot derive — the policy
// artifact — rejecting a missing artifact and one whose digest is not the run's
// resolved policy. The subject's identity fields are minted, not accepted, so
// there is no forged work unit or digest for a caller to smuggle.
func TestIntakeAdmissionAuthenticatesPolicyArtifact(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		want             error
		policyArtifactID domain.ArtifactID
		// seedWrongArtifact records, under policyArtifactID, a policy artifact
		// whose digest is not the run's, so the derivation rejects it.
		seedWrongArtifact bool
	}{
		{"missing policy artifact", store.ErrNotFound, "art-missing", false},
		{"policy artifact of a foreign digest", store.ErrIntakeAdmissionInconsistent, "art-wrong-digest", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t, store.Options{})
			policy, proposal, _ := proposalFixture(t)
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				putIntakeProposalPolicy(t, ctx, tx, policy)
				if tc.seedWrongArtifact {
					wrong, err := domain.NewArtifact(domain.ArtifactInput{
						ID: tc.policyArtifactID, Type: domain.ArtifactKindPolicy,
						Digest: domain.Digest(contentaddr.Sum([]byte("foreign-policy"))),
						Provenance: domain.Provenance{
							ProducerClass: domain.ProducerDaemon, ProducerInvocationID: "inv-wrong",
							HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
						},
					}, nil)
					if err != nil {
						return err
					}
					if err := tx.PutArtifact(ctx, wrong); err != nil {
						return err
					}
				}
				occurrence, _, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS)
				if err != nil {
					return err
				}
				instanceID := mintAndAllocateIntakeProposal(t, ctx, tx, occurrence, proposal)
				if _, bindErr := tx.BindIntakeAdmission(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, instanceID, tc.policyArtifactID); !errors.Is(bindErr, tc.want) {
					return fmt.Errorf("bind not rejected as expected, got %w", bindErr)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestIntakeRefusalRequiresAdmission proves a refusal cannot be stamped on an
// occurrence that has admitted no proposal: the refusal semantics ("left as an
// ordinary proposal") presuppose one, and reconciliation must not consume a
// refusal for a nonexistent admission.
func TestIntakeRefusalRequiresAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		if _, _, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS); err != nil {
			return err
		}
		_, err := tx.RecordIntakeRefusal(ctx, intakeRepoID, intakeIssue, intakeLabel, 1,
			domain.IntakeRefusalWIPCapExhausted, intakeStoreTS)
		if !errors.Is(err, store.ErrImmutableConflict) {
			return fmt.Errorf("refusal on unadmitted occurrence: want ErrImmutableConflict, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// bindAdmittedOccurrence seeds an ordinal-1 occurrence with a bound admission
// and its proposal attention item, and returns the proposal instance id. When
// decided, the item carries a first-decision stamp (still open, but no longer
// withdrawable), modelling a started or declined card.
func bindAdmittedOccurrence(
	t *testing.T, ctx context.Context, tx *store.WriteTx,
	policy domain.ResolvedPolicy, proposal domain.EffectProposal, decided bool,
) domain.ProposalInstanceID {
	t.Helper()
	putIntakeProposalPolicy(t, ctx, tx, policy)
	occurrence, _, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS)
	if err != nil {
		t.Fatalf("allocate occurrence: %v", err)
	}
	if _, err := tx.MintIntakeDeclaration(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, proposal.ResolvedPolicyRunID); err != nil {
		t.Fatalf("mint intake declaration: %v", err)
	}
	instance, _, err := tx.AllocateProposalInstance(ctx, occurrence.ProposalAdmissionKey(), "batch-1", proposal, intakeStoreTS)
	if err != nil {
		t.Fatalf("allocate proposal instance: %v", err)
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
		t.Fatalf("build attention item: %v", err)
	}
	if decided {
		item, err = item.WithDecidedAt(intakeStoreTS)
		if err != nil {
			t.Fatalf("stamp decided: %v", err)
		}
	}
	if err := tx.PutAttentionItem(ctx, item); err != nil {
		t.Fatalf("put attention item: %v", err)
	}
	if err := tx.BindProposalItem(ctx, domain.ItemID(instance.ID), instance.ID, proposal.Digest); err != nil {
		t.Fatalf("bind proposal item: %v", err)
	}
	if _, err := tx.BindIntakeAdmission(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, instance.ID, intakePolicyArtifactID); err != nil {
		t.Fatalf("bind admission: %v", err)
	}
	return instance.ID
}

// TestIntakeSupersedeRecordsOnlyOpenProposal proves supersession records its
// fact only when a genuinely open proposal is withdrawn. An open card is
// superseded and recorded; an already-decided card (DecidedAt stamped) is left
// untouched and no supersession is recorded, though the occurrence still leaves
// present.
func TestIntakeSupersedeRecordsOnlyOpenProposal(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, decided bool, wantRecorded bool) {
		ctx := context.Background()
		st := openStore(t, store.Options{})
		policy, proposal, _ := proposalFixture(t)
		err := st.Write(ctx, func(tx *store.WriteTx) error {
			instanceID := bindAdmittedOccurrence(t, ctx, tx, policy, proposal, decided)
			got, err := tx.SupersedeIntakeProposal(ctx, intakeRepoID, intakeIssue, intakeLabel, 1,
				domain.IntakeSupersededLabelRemoved, domain.IntakeOccurrenceAbsent, intakeStoreTS)
			if err != nil {
				return err
			}
			if got.State != domain.IntakeOccurrenceAbsent {
				return errors.New("occurrence did not leave present")
			}
			if recorded := got.Supersession != nil; recorded != wantRecorded {
				return fmt.Errorf("supersession recorded = %v, want %v", recorded, wantRecorded)
			}
			gotItem, err := tx.GetAttentionItem(ctx, domain.ItemID(instanceID))
			if err != nil {
				return err
			}
			if wantRecorded && gotItem.Status != domain.StatusSuperseded {
				return fmt.Errorf("open item was not superseded: %s", gotItem.Status)
			}
			if !wantRecorded && gotItem.Status != domain.StatusOpen {
				return fmt.Errorf("decided item changed to %s", gotItem.Status)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Run("open proposal is superseded and recorded", func(t *testing.T) {
		run(t, false, true)
	})
	t.Run("decided proposal is left, no supersession", func(t *testing.T) {
		run(t, true, false)
	})
}

// TestIntakeSupersedeReasonMatchesState proves a supersession reason must name
// the departure it produced: label removal leaves the occurrence absent, issue
// closure leaves it closed. A mismatched pair is refused before the item is
// touched, so audit and reconciliation always read a consistent reason/state.
func TestIntakeSupersedeReasonMatchesState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		reason   domain.IntakeSupersessionReason
		newState domain.IntakeOccurrenceState
	}{
		{"label removed to closed", domain.IntakeSupersededLabelRemoved, domain.IntakeOccurrenceClosed},
		{"issue closed to absent", domain.IntakeSupersededIssueClosed, domain.IntakeOccurrenceAbsent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t, store.Options{})
			policy, proposal, _ := proposalFixture(t)
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				bindAdmittedOccurrence(t, ctx, tx, policy, proposal, false)
				_, supErr := tx.SupersedeIntakeProposal(ctx, intakeRepoID, intakeIssue, intakeLabel, 1,
					tc.reason, tc.newState, intakeStoreTS)
				if !errors.Is(supErr, store.ErrImmutableConflict) {
					return fmt.Errorf("mismatched reason/state not refused, got %w", supErr)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestIntakeObservationOpenProposalMustSupersede proves a bare observation
// cannot move an occurrence with an open admitted proposal off present: that
// departure must route through SupersedeIntakeProposal so a later reappearance
// cannot allocate a second live proposal. A decided proposal may be observed
// away freely, since it is terminal.
func TestIntakeObservationOpenProposalMustSupersede(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, decided bool, wantRefused bool) {
		ctx := context.Background()
		st := openStore(t, store.Options{})
		policy, proposal, _ := proposalFixture(t)
		err := st.Write(ctx, func(tx *store.WriteTx) error {
			bindAdmittedOccurrence(t, ctx, tx, policy, proposal, decided)
			_, obsErr := tx.RecordIntakeObservation(ctx, intakeRepoID, intakeIssue, intakeLabel, 1,
				domain.IntakeOccurrenceAbsent, intakeStoreTS)
			if wantRefused {
				if !errors.Is(obsErr, store.ErrImmutableConflict) {
					return fmt.Errorf("bare departure with an open proposal not refused, got %w", obsErr)
				}
				return nil
			}
			if obsErr != nil {
				return fmt.Errorf("departure with a decided proposal refused: %w", obsErr)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Run("open proposal blocks a bare departure", func(t *testing.T) {
		run(t, false, true)
	})
	t.Run("decided proposal allows departure", func(t *testing.T) {
		run(t, true, false)
	})
}

// TestIntakeRefusalRequiresOpenProposal proves a fresh refusal holds only while
// the proposal item is still open and undecided: a delayed gate call after a
// decision must not stamp a false "left as an ordinary proposal" record.
func TestIntakeRefusalRequiresOpenProposal(t *testing.T) {
	t.Parallel()
	t.Run("open proposal accepts the refusal", func(t *testing.T) {
		ctx := context.Background()
		st := openStore(t, store.Options{})
		policy, proposal, _ := proposalFixture(t)
		err := st.Write(ctx, func(tx *store.WriteTx) error {
			bindAdmittedOccurrence(t, ctx, tx, policy, proposal, false)
			got, err := tx.RecordIntakeRefusal(ctx, intakeRepoID, intakeIssue, intakeLabel, 1,
				domain.IntakeRefusalWIPCapExhausted, intakeStoreTS)
			if err != nil {
				return err
			}
			if got.Refusal == nil || got.Refusal.Reason != domain.IntakeRefusalWIPCapExhausted {
				return errors.New("refusal not recorded on an open proposal")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("decided proposal rejects the refusal", func(t *testing.T) {
		ctx := context.Background()
		st := openStore(t, store.Options{})
		policy, proposal, _ := proposalFixture(t)
		err := st.Write(ctx, func(tx *store.WriteTx) error {
			bindAdmittedOccurrence(t, ctx, tx, policy, proposal, true)
			_, refErr := tx.RecordIntakeRefusal(ctx, intakeRepoID, intakeIssue, intakeLabel, 1,
				domain.IntakeRefusalModeNotAuthorized, intakeStoreTS)
			if !errors.Is(refErr, store.ErrImmutableConflict) {
				return fmt.Errorf("refusal on a decided proposal not refused, got %w", refErr)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// TestIntakeLatchAllowsReallocationAfterSupersession proves the latch releases
// the next ordinal once an admitted occurrence has genuinely ended: after a
// supersession withdraws the open card, a reappearing label allocates ordinal 2
// (the item-consistency re-gate passes for a withdrawn card).
func TestIntakeLatchAllowsReallocationAfterSupersession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	policy, proposal, _ := proposalFixture(t)
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		bindAdmittedOccurrence(t, ctx, tx, policy, proposal, false)
		if _, err := tx.SupersedeIntakeProposal(ctx, intakeRepoID, intakeIssue, intakeLabel, 1,
			domain.IntakeSupersededLabelRemoved, domain.IntakeOccurrenceAbsent, intakeStoreTS); err != nil {
			return err
		}
		next, allocated, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS)
		if err != nil {
			return err
		}
		if !allocated || next.Ordinal != 2 || next.State != domain.IntakeOccurrencePresent {
			return fmt.Errorf("reallocation after supersession: allocated=%v ordinal=%d state=%s",
				allocated, next.Ordinal, next.State)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeTransitionRestampsRecordedAt proves a real state transition updates
// RecordedAt to the transition instant, for both a bare observation and a
// supersession, so the current-state record reports when it reached its state
// rather than when it was allocated.
func TestIntakeTransitionRestampsRecordedAt(t *testing.T) {
	t.Parallel()
	later := intakeStoreTS.Add(3 * time.Hour)

	t.Run("observation", func(t *testing.T) {
		ctx := context.Background()
		st := openStore(t, store.Options{})
		err := st.Write(ctx, func(tx *store.WriteTx) error {
			if _, _, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS); err != nil {
				return err
			}
			got, err := tx.RecordIntakeObservation(ctx, intakeRepoID, intakeIssue, intakeLabel, 1,
				domain.IntakeOccurrenceAbsent, later)
			if err != nil {
				return err
			}
			if !got.RecordedAt.Equal(later) {
				return fmt.Errorf("observation RecordedAt = %s, want %s", got.RecordedAt, later)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("supersession", func(t *testing.T) {
		ctx := context.Background()
		st := openStore(t, store.Options{})
		policy, proposal, _ := proposalFixture(t)
		err := st.Write(ctx, func(tx *store.WriteTx) error {
			bindAdmittedOccurrence(t, ctx, tx, policy, proposal, false)
			got, err := tx.SupersedeIntakeProposal(ctx, intakeRepoID, intakeIssue, intakeLabel, 1,
				domain.IntakeSupersededLabelRemoved, domain.IntakeOccurrenceAbsent, later)
			if err != nil {
				return err
			}
			if !got.RecordedAt.Equal(later) {
				return fmt.Errorf("supersession RecordedAt = %s, want %s", got.RecordedAt, later)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// TestIntakeAdmissionRefusesForeignIssueDeclaration proves the mint cannot
// rebind a run whose work-unit declaration already exists bound to a different
// issue: MintIntakeDeclaration mints for the occurrence's own issue, and the
// write-once declaration store refuses a conflicting one, so an occurrence can
// never be admitted onto a work unit declared for another issue.
func TestIntakeAdmissionRefusesForeignIssueDeclaration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	policy, _, _ := proposalFixture(t)
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		putIntakeProposalPolicy(t, ctx, tx, policy)
		// A declaration for this run already exists, bound to a foreign issue.
		foreignIssue := intakeIssue + 1
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			BoundIssue:          &foreignIssue,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, "project-1", intakeStoreTS)
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}

		if _, _, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS); err != nil {
			return err
		}
		if _, mintErr := tx.MintIntakeDeclaration(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, policy.RunID); !errors.Is(mintErr, store.ErrImmutableConflict) {
			return fmt.Errorf("foreign-issue declaration not refused, got %w", mintErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntakeDeclarationReplayConverges proves a crash-recovery replay of the
// mint converges rather than conflicting on the declaration instant. The
// declaration's DeclaredAt is the occurrence's own RecordedAt, not a caller
// clock, so a replay (here a second mint) reconstructs the byte-identical
// declaration and the write-once store accepts it instead of returning
// ErrImmutableConflict on a drifted timestamp.
func TestIntakeDeclarationReplayConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	policy, _, _ := proposalFixture(t)
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		putIntakeProposalPolicy(t, ctx, tx, policy)
		if _, _, err := tx.AllocateNextIntakeOccurrence(ctx, intakeRepo, intakeRepoID, intakeIssue, intakeLabel, intakeStoreTS); err != nil {
			return err
		}
		first, err := tx.MintIntakeDeclaration(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, policy.RunID)
		if err != nil {
			return err
		}
		// The replay arrives on a later wall-clock, modelling crash recovery. The
		// mint takes no clock, so it must still converge.
		second, err := tx.MintIntakeDeclaration(ctx, intakeRepoID, intakeIssue, intakeLabel, 1, policy.RunID)
		if err != nil {
			return fmt.Errorf("mint replay did not converge: %w", err)
		}
		if !first.DeclaredAt.Equal(second.DeclaredAt) || first.DeclaredAt.Equal(time.Time{}) {
			return fmt.Errorf("declaration instant not stable across replay: %s vs %s", first.DeclaredAt, second.DeclaredAt)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
