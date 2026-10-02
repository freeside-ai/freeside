package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

var (
	reentryBase1 = strings.Repeat("1", 40)
	reentryBase2 = strings.Repeat("2", 40)
	reentryBase3 = strings.Repeat("3", 40)
	reentryHead1 = strings.Repeat("a", 40)
	reentryHead2 = strings.Repeat("b", 40)
	reentryAt    = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
)

// reentryFixture is a published ready item the daemon invalidated, with the
// clean review pass that earned it and the authority current state proves.
type reentryFixture struct {
	readyBindingFixture
	authority domain.PublicationSuccessor
}

type reentryOptions struct {
	// skipInvalidation leaves the predecessor open.
	skipInvalidation bool
	// reviewHead overrides the prior clean pass's head.
	reviewHead string
}

func putReentryReview(t *testing.T, st *Store, runID domain.RunID, round int, base, head string) domain.ReviewRecord {
	t.Helper()
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: domain.InvocationID("review-reentry-" + strings.Repeat("r", round)), RunID: runID, Round: round,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: base, HeadSHA: head,
		CompletedAt:        reentryAt.Add(time.Duration(round) * time.Minute),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(context.Background(), func(tx *WriteTx) error {
		return tx.PutReviewRecord(context.Background(), record, nil)
	}); err != nil {
		t.Fatal(err)
	}
	return record
}

// invalidateReadyItem supersedes an open ready item the way the daemon's
// observers do: the fact and the terminal status land in one item version.
func invalidateReadyItem(t *testing.T, st *Store, id domain.ItemID, fact domain.ReadinessInvalidation) {
	t.Helper()
	ctx := context.Background()
	if err := st.Write(ctx, func(tx *WriteTx) error {
		item, err := tx.GetAttentionItem(ctx, id)
		if err != nil {
			return err
		}
		item.Status, item.ReadinessInvalidation = domain.StatusSuperseded, &fact
		item.ItemVersion++
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
}

func reentryFact(reason domain.ReadinessInvalidationReason, bound, observed string) domain.ReadinessInvalidation {
	return domain.ReadinessInvalidation{Reason: reason, Bound: bound, Observed: observed, ObservedAt: reentryAt}
}

func seedReentry(t *testing.T, reason domain.ReadinessInvalidationReason, opts reentryOptions) reentryFixture {
	t.Helper()
	f := reentryFixture{readyBindingFixture: seedReadyItemBinding(t, "feat/meaningful-task", reentryBase1, reentryHead1)}
	reviewHead := reentryHead1
	if opts.reviewHead != "" {
		reviewHead = opts.reviewHead
	}
	prior := putReentryReview(t, f.st, f.run.ID, 1, reentryBase1, reviewHead)
	f.authority = domain.PublicationSuccessor{
		Version: domain.PublicationReentryVersion, Origin: domain.PublicationSuccessorReadinessInvalidation,
		RunID: f.run.ID, PredecessorItemID: f.item.ID, PriorReviewInvocationID: prior.InvocationID, ReviewRound: 2,
		Reentry: &domain.PublicationSuccessorReentry{Reason: reason},
	}
	fact := reentryFact(reason, reentryBase1, reentryBase2)
	f.authority.Reentry.BaseSHA, f.authority.Reentry.HeadSHA = reentryBase2, reentryHead1
	if reason == domain.ReadinessInvalidationHeadChanged {
		fact = reentryFact(reason, reentryHead1, reentryHead2)
		f.authority.Reentry.BaseSHA, f.authority.Reentry.HeadSHA = reentryBase1, reentryHead2
	}
	if !opts.skipInvalidation {
		invalidateReadyItem(t, f.st, f.item.ID, fact)
	}
	return f
}

func (f reentryFixture) record(t *testing.T, authority domain.PublicationSuccessor) error {
	t.Helper()
	ctx := context.Background()
	return f.st.Write(ctx, func(tx *WriteTx) error { return tx.RecordPublicationSuccessor(ctx, authority) })
}

func (f reentryFixture) read(t *testing.T, authority domain.PublicationSuccessor) error {
	t.Helper()
	ctx := context.Background()
	return f.st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetPublicationSuccessor(ctx, authority.RunID, authority.PublicationID())
		return err
	})
}

// reenteredItemAndBinding builds the ready item and binding a re-entered cycle
// records once it re-earns readiness: the authority's head, every other
// coordinate the predecessor's.
func (f reentryFixture) reenteredItemAndBinding(
	t *testing.T, authority domain.PublicationSuccessor, published domain.ReadyItemPRBinding,
) (domain.AttentionItem, domain.ReadyItemPRBinding) {
	t.Helper()
	runID := f.run.ID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: authority.ReadyItemID(), ProjectID: f.run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "re-earned", RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA:   authority.Reentry.HeadSHA,
		PRReference: &domain.PRReference{Repo: published.Repo, Number: published.PRNumber},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	binding := published
	binding.ItemID, binding.PublicationInvocationID = item.ID, authority.PublicationID()
	binding.HeadSHA, binding.RecordedAt = authority.Reentry.HeadSHA, reentryAt.Add(time.Hour)
	return item, binding
}

func (f reentryFixture) putItem(t *testing.T, item domain.AttentionItem) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.Write(ctx, func(tx *WriteTx) error { return putTestAttentionItem(ctx, tx, &item) }); err != nil {
		t.Fatal(err)
	}
}

func (f reentryFixture) recordBinding(binding domain.ReadyItemPRBinding) error {
	ctx := context.Background()
	return f.st.Write(ctx, func(tx *WriteTx) error { return tx.RecordReadyItemPRBinding(ctx, binding) })
}

func TestReentryAuthorityAcceptedForEachReason(t *testing.T) {
	t.Parallel()
	for _, reason := range []domain.ReadinessInvalidationReason{
		domain.ReadinessInvalidationBaseAdvanced, domain.ReadinessInvalidationHeadChanged,
	} {
		t.Run(string(reason), func(t *testing.T) {
			ctx := context.Background()
			f := seedReentry(t, reason, reentryOptions{})
			if err := f.record(t, f.authority); err != nil {
				t.Fatal(err)
			}
			// A byte-identical replay converges.
			if err := f.record(t, f.authority); err != nil {
				t.Fatalf("replay: %v", err)
			}
			if err := f.st.Read(ctx, func(tx *ReadTx) error {
				got, err := tx.PublicationSuccessorForReadyItem(ctx, f.run.ID, f.authority.ReadyItemID())
				if err != nil {
					return err
				}
				if got.PublicationID() != f.authority.PublicationID() || *got.Reentry != *f.authority.Reentry {
					t.Fatalf("resolved %#v", got)
				}
				current, err := tx.CurrentProductionReadyItemID(ctx, f.run.ID)
				if err != nil {
					return err
				}
				if current != f.authority.ReadyItemID() {
					t.Fatalf("current ready item = %q, want the re-entered cycle's %q", current, f.authority.ReadyItemID())
				}
				// The cycle has re-earned nothing yet: the published head stays
				// with the superseded item.
				published, err := tx.PublishedProductionReadyItemID(ctx, f.run.ID)
				if err != nil {
					return err
				}
				if published != f.item.ID {
					t.Fatalf("published ready item = %q, want %q", published, f.item.ID)
				}
				if err := tx.AuthenticateSuccessorProducer(ctx, f.authority, ""); !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("empty producer error = %v, want ErrParentKeyMismatch", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Current state fixes every field of the record (a run has one
			// review per round), so no differing second authority for the item
			// passes the gate to reach its shared key.
			second := f.authority
			second.ReviewRound = 3
			if err := f.record(t, second); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("second re-entry authority for the same item: %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

// TestReentryAuthorityRejectsEachMismatch offers a caller-supplied record that
// disagrees with current state on one fact at a time.
func TestReentryAuthorityRejectsEachMismatch(t *testing.T) {
	t.Parallel()
	type mutation struct {
		name   string
		reason domain.ReadinessInvalidationReason
		mutate func(*domain.PublicationSuccessor)
	}
	advanced, changed := domain.ReadinessInvalidationBaseAdvanced, domain.ReadinessInvalidationHeadChanged
	cases := []mutation{
		{"base_advanced as head_changed", advanced, func(s *domain.PublicationSuccessor) {
			s.Reentry.Reason, s.Reentry.BaseSHA, s.Reentry.HeadSHA = changed, reentryBase1, reentryBase2
		}},
		{"head_changed as base_advanced", changed, func(s *domain.PublicationSuccessor) {
			s.Reentry.Reason, s.Reentry.BaseSHA, s.Reentry.HeadSHA = advanced, reentryHead2, reentryHead1
		}},
		{"base_advanced base not observed", advanced, func(s *domain.PublicationSuccessor) { s.Reentry.BaseSHA = reentryBase3 }},
		{"base_advanced base still bound", advanced, func(s *domain.PublicationSuccessor) { s.Reentry.BaseSHA = reentryBase1 }},
		{"base_advanced foreign head", advanced, func(s *domain.PublicationSuccessor) { s.Reentry.HeadSHA = reentryHead2 }},
		{"head_changed head not observed", changed, func(s *domain.PublicationSuccessor) { s.Reentry.HeadSHA = reentryBase3 }},
		{"head_changed head still bound", changed, func(s *domain.PublicationSuccessor) { s.Reentry.HeadSHA = reentryHead1 }},
		{"head_changed base not reviewed", changed, func(s *domain.PublicationSuccessor) { s.Reentry.BaseSHA = reentryBase2 }},
		{"round not one above prior", advanced, func(s *domain.PublicationSuccessor) { s.ReviewRound = 3 }},
		{"unknown prior review", advanced, func(s *domain.PublicationSuccessor) { s.PriorReviewInvocationID = "review-unknown" }},
		{"other run", advanced, func(s *domain.PublicationSuccessor) { s.RunID = "run-other" }},
		{"unknown predecessor", advanced, func(s *domain.PublicationSuccessor) { s.PredecessorItemID = "production-ready-unknown" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := seedReentry(t, tc.reason, reentryOptions{})
			forged := f.authority
			reentry := *f.authority.Reentry
			forged.Reentry = &reentry
			tc.mutate(&forged)
			if err := forged.Validate(); err != nil {
				t.Fatalf("mutation is not a store-gate case: %v", err)
			}
			if err := f.record(t, forged); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("forged authority error = %v, want ErrParentKeyMismatch", err)
			}
			if err := f.record(t, f.authority); err != nil {
				t.Fatalf("true authority refused after the forgery: %v", err)
			}
		})
	}
}

// TestReentryAuthorityRegatesCurrentState proves the gate reads state, not the
// record: a predecessor that does not carry the invalidation, binding and
// review the record claims is refused at record time and again on every read.
func TestReentryAuthorityRegatesCurrentState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	advanced, changed := domain.ReadinessInvalidationBaseAdvanced, domain.ReadinessInvalidationHeadChanged
	t.Run("predecessor still open", func(t *testing.T) {
		f := seedReentry(t, advanced, reentryOptions{skipInvalidation: true})
		if err := f.record(t, f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("predecessor returned, not invalidated", func(t *testing.T) {
		f := seedReentry(t, advanced, reentryOptions{skipInvalidation: true})
		if err := f.st.Write(ctx, func(tx *WriteTx) error {
			item, err := tx.GetAttentionItem(ctx, f.item.ID)
			if err != nil {
				return err
			}
			item.Status = domain.StatusSuperseded
			item.ItemVersion++
			return tx.PutAttentionItem(ctx, item)
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.record(t, f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("retargeted predecessor", func(t *testing.T) {
		f := seedReentry(t, advanced, reentryOptions{skipInvalidation: true})
		invalidateReadyItem(t, f.st, f.item.ID, reentryFact(domain.ReadinessInvalidationRetargeted, "main", "release"))
		if err := f.record(t, f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("prior review of another head", func(t *testing.T) {
		f := seedReentry(t, advanced, reentryOptions{reviewHead: reentryHead2})
		if err := f.record(t, f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("prior review of another run", func(t *testing.T) {
		f := seedReentry(t, advanced, reentryOptions{})
		other := f.run
		other.ID, other.Stages = "run-other", nil
		if err := f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutRun(ctx, other) }); err != nil {
			t.Fatal(err)
		}
		// The same clean pass on the same head, recorded for the other run.
		if _, err := f.st.db.ExecContext(ctx, `DELETE FROM review_records`); err != nil {
			t.Fatal(err)
		}
		putReentryReview(t, f.st, other.ID, 1, reentryBase1, reentryHead1)
		if err := f.record(t, f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("stored record rewritten to other coordinates", func(t *testing.T) {
		f := seedReentry(t, advanced, reentryOptions{})
		if err := f.record(t, f.authority); err != nil {
			t.Fatal(err)
		}
		// The rewrite keeps the payload canonical, so only the gate can refuse it.
		if _, err := f.st.db.ExecContext(ctx, `UPDATE outbox
			SET payload = CAST(replace(CAST(payload AS TEXT), ?, ?) AS BLOB) WHERE idempotency_key = ?`,
			reentryBase2, reentryBase3, f.authority.Key()); err != nil {
			t.Fatal(err)
		}
		if err := f.read(t, f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("read error = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("head_changed fact bound to another head", func(t *testing.T) {
		f := seedReentry(t, changed, reentryOptions{skipInvalidation: true})
		invalidateReadyItem(t, f.st, f.item.ID, reentryFact(changed, reentryBase3, reentryHead2))
		if err := f.record(t, f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	for _, tc := range []struct {
		name        string
		reason      domain.ReadinessInvalidationReason
		sql         string
		itemInvalid bool
	}{
		{"observed coordinate rewritten", advanced, `UPDATE attention_items SET body = json_set(body, '$.readiness_invalidation.observed', 'elsewhere') WHERE id = ?`, false},
		{"reason rewritten", advanced, `UPDATE attention_items SET body = json_set(body, '$.readiness_invalidation.reason', 'head_changed') WHERE id = ?`, false},
		{"bound head rewritten", changed, `UPDATE attention_items SET body = json_set(body, '$.readiness_invalidation.bound', 'elsewhere') WHERE id = ?`, false},
		{"invalidation removed", changed, `UPDATE attention_items SET body = json_remove(body, '$.readiness_invalidation') WHERE id = ?`, false},
		{"binding removed", advanced, `DELETE FROM ready_item_pr_bindings WHERE item_id = ?`, false},
		{"status reopened", advanced, `UPDATE attention_items SET status = 'open', body = json_set(body, '$.status', 'open') WHERE id = ?`, true},
		{"type rewritten", advanced, `UPDATE attention_items SET item_type = 'review_dispute', body = json_set(body, '$.type', 'review_dispute') WHERE id = ?`, true},
	} {
		t.Run("read after "+tc.name, func(t *testing.T) {
			f := seedReentry(t, tc.reason, reentryOptions{})
			if err := f.record(t, f.authority); err != nil {
				t.Fatal(err)
			}
			if err := f.read(t, f.authority); err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.db.ExecContext(ctx, tc.sql, f.item.ID); err != nil {
				t.Fatal(err)
			}
			// A reopened or retyped item fails its own reconstruction first, so
			// those two report the item's error rather than the gate's.
			err := f.read(t, f.authority)
			if err == nil || (!tc.itemInvalid && !errors.Is(err, domain.ErrParentKeyMismatch)) {
				t.Fatalf("read error = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

// TestOnlyReentryNamesAnInvalidatedPredecessor pins that a superseded item
// never counts as the current ready item for another authority.
func TestOnlyReentryNamesAnInvalidatedPredecessor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedReentry(t, domain.ReadinessInvalidationBaseAdvanced, reentryOptions{})
	feedback := domain.PublicationSuccessor{
		Version: domain.PublicationSuccessorVersion, RunID: f.run.ID, CommandID: "return-1",
		FeedbackInvocationID: "inv-operator-feedback-1", PredecessorItemID: f.item.ID,
		PriorReviewInvocationID: f.authority.PriorReviewInvocationID, ReviewRound: 2,
	}
	continuation := domain.PublicationSuccessor{
		Version: domain.PublicationContinuationVersion, Origin: domain.PublicationSuccessorRemediation,
		RunID: f.run.ID, CommandID: "approve-1", ReevaluationCommandID: "rerun-1", PredecessorItemID: f.item.ID,
		PriorReviewInvocationID: f.authority.PriorReviewInvocationID, ReviewRound: 2,
	}
	for name, successor := range map[string]domain.PublicationSuccessor{"feedback": feedback, "remediation_continuation": continuation} {
		// Without the refusal these fail later on their missing command records,
		// which reports ErrNotFound; the refusal itself is the bare mismatch.
		if err := f.record(t, successor); !errors.Is(err, domain.ErrParentKeyMismatch) || errors.Is(err, ErrNotFound) {
			t.Fatalf("%s on an invalidated predecessor: %v", name, err)
		}
	}
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		if err := tx.requireUninvalidatedPredecessor(ctx, feedback); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("invalidated predecessor error = %v", err)
		}
		// A cycle that stopped before publication has no ready item row.
		unpublished := continuation
		unpublished.PredecessorItemID = "production-ready-feedback-never-published"
		return tx.requireUninvalidatedPredecessor(ctx, unpublished)
	}); err != nil {
		t.Fatal(err)
	}
	open := seedReentry(t, domain.ReadinessInvalidationBaseAdvanced, reentryOptions{skipInvalidation: true})
	if err := open.st.Read(ctx, func(tx *ReadTx) error {
		return tx.requireUninvalidatedPredecessor(ctx, feedback)
	}); err != nil {
		t.Fatalf("uninvalidated predecessor refused: %v", err)
	}
}

func TestReenteredReadyBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, reason := range []domain.ReadinessInvalidationReason{
		domain.ReadinessInvalidationBaseAdvanced, domain.ReadinessInvalidationHeadChanged,
	} {
		t.Run(string(reason), func(t *testing.T) {
			f := seedReentry(t, reason, reentryOptions{})
			if err := f.record(t, f.authority); err != nil {
				t.Fatal(err)
			}
			item, binding := f.reenteredItemAndBinding(t, f.authority, f.binding)
			f.putItem(t, item)
			for name, mutate := range map[string]func(*domain.ReadyItemPRBinding){
				"repo":                 func(b *domain.ReadyItemPRBinding) { b.Repo = "other/repo" },
				"repository id":        func(b *domain.ReadyItemPRBinding) { b.RepositoryID = 515151 },
				"pr number":            func(b *domain.ReadyItemPRBinding) { b.PRNumber = 451 },
				"base ref":             func(b *domain.ReadyItemPRBinding) { b.BaseRef = "release" },
				"producing invocation": func(b *domain.ReadyItemPRBinding) { b.ProducingInvocationID = "inv-ready-foreign" },
				"publication identity": func(b *domain.ReadyItemPRBinding) {
					b.PublicationIdentity = domain.Digest("sha256:" + strings.Repeat("b", 64))
				},
			} {
				forged := binding
				mutate(&forged)
				if err := f.recordBinding(forged); !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("re-entered binding with a foreign %s: %v, want ErrParentKeyMismatch", name, err)
				}
			}
			otherRun := binding
			otherRun.RunID = "run-other"
			if err := f.recordBinding(otherRun); !errors.Is(err, errRowInconsistent) {
				t.Fatalf("re-entered binding of another run: %v, want errRowInconsistent", err)
			}
			if err := f.recordBinding(binding); err != nil {
				t.Fatal(err)
			}
			if err := f.st.Read(ctx, func(tx *ReadTx) error {
				got, err := tx.GetReadyItemPRBinding(ctx, item.ID)
				if err != nil {
					return err
				}
				if got.HeadSHA != f.authority.Reentry.HeadSHA || got.PublicationIdentity != f.binding.PublicationIdentity {
					t.Fatalf("binding = %#v", got)
				}
				if _, err := tx.GetAttentionItem(ctx, item.ID); err != nil {
					return err
				}
				published, err := tx.PublishedProductionReadyItemID(ctx, f.run.ID)
				if err != nil {
					return err
				}
				if published != item.ID {
					t.Fatalf("published ready item = %q, want the re-entered %q", published, item.ID)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// The stored row is re-gated on read: retargeting it at another head
			// leaves both the item and the authority disagreeing with it.
			if _, err := f.st.db.ExecContext(ctx, `UPDATE ready_item_pr_bindings
				SET body = json_set(body, '$.head_sha', ?) WHERE item_id = ?`, reentryBase3, item.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetReadyItemPRBinding(ctx, item.ID)
				return err
			}); err == nil {
				t.Fatal("re-entered binding retargeted at another head was read back")
			}
		})
	}
}

// TestReenteredReadyBindingNeedsItsAuthority covers the forgeries the
// re-entry path must not open: a head the authority did not re-enter for, a
// re-entry claim with no authority behind it, and an ordinary binding naming a
// head no export produced.
func TestReenteredReadyBindingNeedsItsAuthority(t *testing.T) {
	t.Parallel()
	f := seedReentry(t, domain.ReadinessInvalidationHeadChanged, reentryOptions{})
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	t.Run("item and binding agree on a head the authority does not name", func(t *testing.T) {
		wrong := f.authority
		wrong.Reentry = &domain.PublicationSuccessorReentry{
			Reason: domain.ReadinessInvalidationHeadChanged, BaseSHA: reentryBase1, HeadSHA: reentryBase3,
		}
		item, binding := f.reenteredItemAndBinding(t, wrong, f.binding)
		f.putItem(t, item)
		if err := f.recordBinding(binding); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	foreign := func(t *testing.T, id domain.ItemID, publication domain.InvocationID) domain.ReadyItemPRBinding {
		t.Helper()
		claim := f.authority
		item, binding := f.reenteredItemAndBinding(t, claim, f.binding)
		item.ID, binding.ItemID, binding.PublicationInvocationID = id, id, publication
		f.putItem(t, item)
		return binding
	}
	t.Run("re-entry claim without an authority", func(t *testing.T) {
		binding := foreign(t, "production-ready-reentry-"+domain.ItemID(strings.Repeat("0", 64)),
			domain.InvocationID(reentryPublicationPrefix+strings.Repeat("0", 64)))
		if err := f.recordBinding(binding); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("authority claimed by another item", func(t *testing.T) {
		binding := foreign(t, "item-claims-reentry", f.authority.PublicationID())
		if err := f.recordBinding(binding); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("no re-entry authority still needs export and outcome", func(t *testing.T) {
		// The inherited producer's export recorded the old head, and no
		// publication intent or outcome exists for this publication.
		binding := foreign(t, "item-foreign-head", "publish-production-foreign")
		if err := f.recordBinding(binding); !errors.Is(err, errRowInconsistent) {
			t.Fatalf("error = %v, want errRowInconsistent from the producing export", err)
		}
	})
}

func TestReentrySuccessorTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedReentry(t, domain.ReadinessInvalidationHeadChanged, reentryOptions{})
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	target := func(authority domain.PublicationSuccessor) publicationrecord.SuccessorTarget {
		t.Helper()
		var got publicationrecord.SuccessorTarget
		if err := f.st.Read(ctx, func(tx *ReadTx) (err error) {
			got, err = tx.PublicationSuccessorTarget(ctx, f.run.ID, authority.PublicationID())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// The head now on the PR is the observed one; the PR, branch and identity
	// are still the first publication's.
	want := publicationrecord.SuccessorTarget{
		ItemID: f.item.ID, Identity: f.binding.PublicationIdentity, HeadSHA: reentryHead2,
		PRNumber: f.binding.PRNumber, Branch: "feat/meaningful-task",
	}
	if got := target(f.authority); got != want {
		t.Fatalf("re-entry target = %#v, want %#v", got, want)
	}

	// The cycle re-earns readiness on the observed head, then the base
	// advances under it: the next cycle's predecessor is a re-entered item
	// whose inherited outcome still records the first publication's head.
	item, binding := f.reenteredItemAndBinding(t, f.authority, f.binding)
	f.putItem(t, item)
	if err := f.recordBinding(binding); err != nil {
		t.Fatal(err)
	}
	prior := putReentryReview(t, f.st, f.run.ID, 2, reentryBase1, reentryHead2)
	invalidateReadyItem(t, f.st, item.ID, reentryFact(domain.ReadinessInvalidationBaseAdvanced, reentryBase1, reentryBase2))
	next := domain.PublicationSuccessor{
		Version: domain.PublicationReentryVersion, Origin: domain.PublicationSuccessorReadinessInvalidation,
		RunID: f.run.ID, PredecessorItemID: item.ID, PriorReviewInvocationID: prior.InvocationID, ReviewRound: 3,
		Reentry: &domain.PublicationSuccessorReentry{
			Reason: domain.ReadinessInvalidationBaseAdvanced, BaseSHA: reentryBase2, HeadSHA: reentryHead2,
		},
	}
	if err := f.record(t, next); err != nil {
		t.Fatal(err)
	}
	want.ItemID = item.ID
	if got := target(next); got != want {
		t.Fatalf("target after a re-entered predecessor = %#v, want %#v", got, want)
	}
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		chain, err := tx.PublicationSuccessorChain(ctx, f.run.ID)
		if err != nil {
			return err
		}
		if len(chain) != 2 || chain[1].ReadyItemID() != next.ReadyItemID() {
			t.Fatalf("chain = %#v", chain)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReentryChainReadsEachAncestorOnce re-enters the same pull request many
// times over. Each level's binding is proved by its predecessor's, so a gate
// that authenticated the predecessor twice per level would double the cost of
// a read with every re-entry and this depth would not finish.
func TestReentryChainReadsEachAncestorOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedReentry(t, domain.ReadinessInvalidationBaseAdvanced, reentryOptions{})
	const depth = 24
	authority, item, binding := f.authority, f.item, f.binding
	for level := 1; level <= depth; level++ {
		if err := f.record(t, authority); err != nil {
			t.Fatalf("level %d authority: %v", level, err)
		}
		item, binding = f.reenteredItemAndBinding(t, authority, binding)
		f.putItem(t, item)
		if err := f.recordBinding(binding); err != nil {
			t.Fatalf("level %d binding: %v", level, err)
		}
		base, next := authority.Reentry.BaseSHA, fmt.Sprintf("%040d", level)
		prior := putReentryReview(t, f.st, f.run.ID, level+1, base, reentryHead1)
		invalidateReadyItem(t, f.st, item.ID, reentryFact(domain.ReadinessInvalidationBaseAdvanced, base, next))
		authority = domain.PublicationSuccessor{
			Version: domain.PublicationReentryVersion, Origin: domain.PublicationSuccessorReadinessInvalidation,
			RunID: f.run.ID, PredecessorItemID: item.ID, PriorReviewInvocationID: prior.InvocationID,
			ReviewRound: level + 2,
			Reentry: &domain.PublicationSuccessorReentry{
				Reason: domain.ReadinessInvalidationBaseAdvanced, BaseSHA: next, HeadSHA: reentryHead1,
			},
		}
	}
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		chain, err := tx.PublicationSuccessorChain(ctx, f.run.ID)
		if err != nil {
			return err
		}
		if len(chain) != depth {
			t.Fatalf("chain length = %d, want %d", len(chain), depth)
		}
		_, err = tx.GetReadyItemPRBinding(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRemediatedReentryBindingNeedsExportAndOutcome covers the re-entered
// cycle that does publish: after a base advance its remediation pushes a new
// head under the authority's publication ID. That binding names a head the
// authority never did, so it is proved the way every published successor is.
func TestRemediatedReentryBindingNeedsExportAndOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedReentry(t, domain.ReadinessInvalidationBaseAdvanced, reentryOptions{})
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	var target publicationrecord.SuccessorTarget
	if err := f.st.Read(ctx, func(tx *ReadTx) (err error) {
		target, err = tx.PublicationSuccessorTarget(ctx, f.run.ID, f.authority.PublicationID())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	invocationID, attemptID := domain.InvocationID("inv-reentry-remediation"), domain.AttemptID("attempt-reentry-remediation")
	run := f.run
	stage := run.Stages[0]
	stage.Attempts = append(append([]domain.Attempt(nil), stage.Attempts...),
		domain.Attempt{ID: attemptID, StageID: stage.ID, Number: 2, InvocationID: invocationID})
	run.Stages = []domain.Stage{stage}
	input := domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: run.ID, StageID: stage.ID, AttemptID: attemptID,
		Backend: f.admission.Backend, Capabilities: f.admission.Capabilities,
		OperatingMode: f.admission.OperatingMode, CredentialMode: f.admission.CredentialMode,
		EgressProfile: f.admission.EgressProfile, ImageRef: f.admission.ImageRef,
		SpecDigest: f.admission.SpecDigest, PolicyDigest: f.admission.PolicyDigest, InputDigest: f.admission.InputDigest,
		Base: f.admission.Base, Workspace: f.admission.Workspace, AdmittedAt: reentryAt.Add(time.Hour),
	}
	input.Base.BaseSHA = reentryBase2
	admission, err := domain.NewExecutionAdmission(input)
	if err != nil {
		t.Fatal(err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocationID, AdmissionID: admission.ID, ObservedBaseSHA: reentryBase2, HeadSHA: reentryHead2,
		ManifestDigest: "sha256:manifest", RecordedAt: reentryAt.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.Digest("sha256:" + strings.Repeat("f", 64))
	intent, err := json.Marshal(readyPublicationIntent{
		FormatVersion: publicationrecord.IntentFormatSuccessor, Identity: identity,
		InvocationID: f.authority.PublicationID(), Branch: target.Branch,
		Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef, SourceHeadSHA: reentryHead2,
		AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
		ProducingInvocationID: invocationID, ReservationRunID: run.ID, Successor: &target,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := json.Marshal(readyPublicationOutcome{
		Identity: identity, Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef, HeadSHA: reentryHead2,
		Branch: target.Branch, PRNumber: target.PRNumber, EvidenceEligible: true, Successor: &target,
	})
	if err != nil {
		t.Fatal(err)
	}
	remediated := f.authority
	remediated.Reentry = &domain.PublicationSuccessorReentry{
		Reason: domain.ReadinessInvalidationBaseAdvanced, BaseSHA: reentryBase2, HeadSHA: reentryHead2,
	}
	item, binding := f.reenteredItemAndBinding(t, remediated, f.binding)
	binding.ProducingInvocationID, binding.PublicationIdentity = invocationID, identity
	f.putItem(t, item)
	if err := f.st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		return tx.RecordExecutionExport(ctx, export)
	}); err != nil {
		t.Fatal(err)
	}
	// Before the cycle's publication exists the binding is an in-place claim
	// for a head the authority does not name.
	if err := f.recordBinding(binding); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("unpublished remediated binding: %v, want ErrParentKeyMismatch", err)
	}
	intentKey := "publish/" + string(f.authority.PublicationID()) + "/" + readyPublicationIntentKind
	if err := f.st.Write(ctx, func(tx *WriteTx) error {
		if _, _, err := tx.EnqueueOutbox(ctx, intentKey, readyPublicationIntentKind, intent); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, intentKey); err != nil {
			return err
		}
		_, _, err := tx.RecordInbox(ctx, "publish.outcome/"+string(identity), "publish.outcome", outcome)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Once it published, the in-place shape (the predecessor's producer and
	// identity on the authority's head) is no longer accepted for it.
	_, inPlace := f.reenteredItemAndBinding(t, f.authority, f.binding)
	inPlace.HeadSHA = reentryHead2
	if err := f.recordBinding(inPlace); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("in-place binding for a published cycle: %v, want errRowInconsistent", err)
	}
	if err := f.recordBinding(binding); err != nil {
		t.Fatalf("remediated re-entry binding: %v", err)
	}
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		published, err := tx.PublishedProductionReadyItemID(ctx, f.run.ID)
		if err != nil {
			return err
		}
		if published != item.ID {
			t.Fatalf("published ready item = %q, want %q", published, item.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
