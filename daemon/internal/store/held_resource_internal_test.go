package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// putHeldItem stores a publish_blocked item for the fixture's run. A nil
// reference is the hold that predates publication.
func putHeldItem(
	t *testing.T, st *Store, run domain.Run, id domain.ItemID, head string, reference *domain.PRReference,
) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	runID := run.ID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
		Reason: "publication is held", RequestedDecision: []domain.Action{domain.ActionInspectTrustFailure},
		PRHeadSHA: head, PRReference: reference,
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return putTestAttentionItem(ctx, tx, &item) }); err != nil {
		t.Fatal(err)
	}
	return item
}

func recordHeldBinding(st *Store, binding domain.HeldItemPRBinding) error {
	ctx := context.Background()
	return st.Write(ctx, func(tx *WriteTx) error { return tx.RecordHeldItemPRBinding(ctx, binding) })
}

func readHeldBinding(st *Store, id domain.ItemID) (domain.HeldItemPRBinding, error) {
	ctx := context.Background()
	var binding domain.HeldItemPRBinding
	err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		binding, err = tx.GetHeldItemPRBinding(ctx, id)
		return err
	})
	return binding, err
}

// TestHeldItemPRBindingRoundTripsAndRegates covers a hold on a run whose
// first publication already exists: the binding is proven by the same
// admission, export, intent, and outcome as the ready binding, is write-once,
// and is re-checked against its item, run, head, and pull request on every
// read. A corrupt binding also fails the held item's own read.
func TestHeldItemPRBindingRoundTripsAndRegates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedReadyItemBinding(t, "feat/meaningful-task", "deadbeef", "cafed00d")
	st := f.st
	reference := &domain.PRReference{Repo: f.binding.Repo, Number: f.binding.PRNumber}
	hold := putHeldItem(t, st, f.run, domain.ProductionBlockedItemID(f.run.ID), f.binding.HeadSHA, reference)
	binding := domain.HeldItemPRBinding(f.binding)
	binding.ItemID = hold.ID

	if _, err := readHeldBinding(st, hold.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("binding before it is recorded = %v, want ErrNotFound", err)
	}

	t.Run("refused bindings", func(t *testing.T) {
		unreferenced := putHeldItem(t, st, f.run, "hold-before-publication", f.binding.HeadSHA, nil)
		otherPR := putHeldItem(t, st, f.run, "hold-other-pr", f.binding.HeadSHA,
			&domain.PRReference{Repo: f.binding.Repo, Number: 451})
		otherHead := putHeldItem(t, st, f.run, "hold-other-head", "feedface", reference)
		for name, mutate := range map[string]func(*domain.HeldItemPRBinding){
			"ready item":                    func(b *domain.HeldItemPRBinding) { b.ItemID = f.item.ID },
			"hold without a reference":      func(b *domain.HeldItemPRBinding) { b.ItemID = unreferenced.ID },
			"hold naming another pr":        func(b *domain.HeldItemPRBinding) { b.ItemID = otherPR.ID },
			"hold on another head":          func(b *domain.HeldItemPRBinding) { b.ItemID = otherHead.ID },
			"unknown item":                  func(b *domain.HeldItemPRBinding) { b.ItemID = "hold-missing" },
			"another run":                   func(b *domain.HeldItemPRBinding) { b.RunID = "run-other" },
			"head the export did not make":  func(b *domain.HeldItemPRBinding) { b.HeadSHA = "feedface" },
			"repo":                          func(b *domain.HeldItemPRBinding) { b.Repo = "other/repo" },
			"repository id":                 func(b *domain.HeldItemPRBinding) { b.RepositoryID = 515151 },
			"pr number":                     func(b *domain.HeldItemPRBinding) { b.PRNumber = 451 },
			"base ref":                      func(b *domain.HeldItemPRBinding) { b.BaseRef = "release" },
			"publication without an intent": func(b *domain.HeldItemPRBinding) { b.PublicationInvocationID = "publish-production-foreign" },
			"publication identity": func(b *domain.HeldItemPRBinding) {
				b.PublicationIdentity = domain.Digest("sha256:" + strings.Repeat("b", 64))
			},
		} {
			forged := binding
			mutate(&forged)
			if err := recordHeldBinding(st, forged); err == nil {
				t.Errorf("%s: binding was recorded", name)
			}
		}
		var rows int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM held_item_pr_bindings`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("refused bindings left %d rows", rows)
		}
	})

	if err := recordHeldBinding(st, binding); err != nil {
		t.Fatalf("record held binding: %v", err)
	}
	got, err := readHeldBinding(st, hold.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != binding {
		t.Fatalf("binding = %#v, want %#v", got, binding)
	}
	if err := recordHeldBinding(st, binding); err != nil {
		t.Fatalf("replay of the same binding: %v", err)
	}
	restamped := binding
	restamped.RecordedAt = binding.RecordedAt.Add(time.Minute)
	if err := recordHeldBinding(st, restamped); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("different record for the same item = %v, want ErrImmutableConflict", err)
	}
	// The ready item's own binding and read are unaffected by its sibling.
	if err := st.Read(ctx, func(tx *ReadTx) error {
		if _, err := tx.GetReadyItemPRBinding(ctx, f.item.ID); err != nil {
			return err
		}
		_, _, err := tx.GetAttentionItemSnapshot(ctx, hold.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	originalBody, err := encode(binding)
	if err != nil {
		t.Fatal(err)
	}
	restore := func(t *testing.T) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `UPDATE held_item_pr_bindings
			SET run_id = ?, repository_id = ?, pr_number = ?, publication_invocation_id = ?,
			publication_identity = ?, body = ? WHERE item_id = ?`,
			binding.RunID, binding.RepositoryID, binding.PRNumber, binding.PublicationInvocationID,
			binding.PublicationIdentity, originalBody, hold.ID); err != nil {
			t.Fatal(err)
		}
	}
	foreignIdentity := "sha256:" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{name: "repo", sql: `UPDATE held_item_pr_bindings SET body = json_set(body, '$.repo', 'other/repo') WHERE item_id = ?`},
		{name: "repository id", sql: `UPDATE held_item_pr_bindings SET repository_id = 515151, body = json_set(body, '$.repository_id', 515151) WHERE item_id = ?`},
		{name: "pr number", sql: `UPDATE held_item_pr_bindings SET pr_number = 451, body = json_set(body, '$.pr_number', 451) WHERE item_id = ?`},
		{name: "base ref", sql: `UPDATE held_item_pr_bindings SET body = json_set(body, '$.base_ref', 'release') WHERE item_id = ?`},
		{name: "head", sql: `UPDATE held_item_pr_bindings SET body = json_set(body, '$.head_sha', 'feedface') WHERE item_id = ?`},
		{name: "column disagrees with body", sql: `UPDATE held_item_pr_bindings SET pr_number = 451 WHERE item_id = ?`},
		{name: "publication identity", sql: `UPDATE held_item_pr_bindings SET publication_identity = ?, body = json_set(body, '$.publication_identity', ?) WHERE item_id = ?`, args: []any{foreignIdentity, foreignIdentity}},
		{name: "publication invocation", sql: `UPDATE held_item_pr_bindings SET publication_invocation_id = 'publish-production-foreign', body = json_set(body, '$.publication_invocation_id', 'publish-production-foreign') WHERE item_id = ?`},
	} {
		t.Run("stored "+tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx, tc.sql, append(tc.args, hold.ID)...); err != nil {
				t.Fatal(err)
			}
			// A corrupt binding must never read as an absent one, so the
			// refusal is a gate error and not ErrNotFound.
			refused := func(err error) bool {
				return !errors.Is(err, ErrNotFound) &&
					(errors.Is(err, errRowInconsistent) || errors.Is(err, domain.ErrParentKeyMismatch))
			}
			if _, err := readHeldBinding(st, hold.ID); !refused(err) {
				t.Fatalf("retargeted held binding read = %v, want a gate refusal", err)
			}
			if err := st.Read(ctx, func(tx *ReadTx) error {
				_, _, err := tx.GetAttentionItemSnapshot(ctx, hold.ID)
				return err
			}); !refused(err) {
				t.Fatalf("held item with a retargeted binding = %v, want a gate refusal", err)
			}
			restore(t)
		})
	}
	// The body reference is compared to the store-owned anchor, so retargeting
	// the item and its binding together still fails.
	t.Run("item and binding retargeted together", func(t *testing.T) {
		var itemBody string
		if err := st.db.QueryRowContext(ctx,
			`SELECT body FROM attention_items WHERE id = ?`, hold.ID).Scan(&itemBody); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE attention_items
			SET body = json_set(body, '$.pr_reference.number', 451) WHERE id = ?`, hold.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE held_item_pr_bindings
			SET pr_number = 451, body = json_set(body, '$.pr_number', 451) WHERE item_id = ?`, hold.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := readHeldBinding(st, hold.ID); !errors.Is(err, errRowInconsistent) {
			t.Fatalf("retargeted pair = %v, want errRowInconsistent", err)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE attention_items SET body = ? WHERE id = ?`, itemBody, hold.ID); err != nil {
			t.Fatal(err)
		}
		restore(t)
	})
	if got, err := readHeldBinding(st, hold.ID); err != nil || got != binding {
		t.Fatalf("restored binding = %#v, %v", got, err)
	}

	// Several holds may share one publication: a rerun holds under its own
	// item ID on the run's publication invocation.
	rerun := putHeldItem(t, st, f.run, "hold-rerun", f.binding.HeadSHA, reference)
	second := binding
	second.ItemID = rerun.ID
	if err := recordHeldBinding(st, second); err != nil {
		t.Fatalf("second hold on the same publication: %v", err)
	}
}

// TestReenteredHeldBinding covers a hold on a cycle that re-entered in place:
// it published nothing, so its binding is proven by the re-entry authority
// and inherits the predecessor's coordinates, as the ready binding is.
func TestReenteredHeldBinding(t *testing.T) {
	t.Parallel()
	for _, reason := range []domain.ReadinessInvalidationReason{
		domain.ReadinessInvalidationBaseAdvanced, domain.ReadinessInvalidationHeadChanged,
	} {
		t.Run(string(reason), func(t *testing.T) {
			f := seedReentry(t, reason, reentryOptions{})
			if err := f.record(t, f.authority); err != nil {
				t.Fatal(err)
			}
			reference := &domain.PRReference{Repo: f.binding.Repo, Number: f.binding.PRNumber}
			hold := putHeldItem(t, f.st, f.run, f.authority.BlockedItemID(), f.authority.Reentry.HeadSHA, reference)
			binding := domain.HeldItemPRBinding(f.binding)
			binding.ItemID, binding.PublicationInvocationID = hold.ID, f.authority.PublicationID()
			binding.HeadSHA, binding.RecordedAt = f.authority.Reentry.HeadSHA, reentryAt.Add(time.Hour)

			for name, mutate := range map[string]func(*domain.HeldItemPRBinding){
				"repository id":        func(b *domain.HeldItemPRBinding) { b.RepositoryID = 515151 },
				"base ref":             func(b *domain.HeldItemPRBinding) { b.BaseRef = "release" },
				"producing invocation": func(b *domain.HeldItemPRBinding) { b.ProducingInvocationID = "inv-ready-foreign" },
				"publication identity": func(b *domain.HeldItemPRBinding) {
					b.PublicationIdentity = domain.Digest("sha256:" + strings.Repeat("b", 64))
				},
				"authority that does not exist": func(b *domain.HeldItemPRBinding) {
					b.PublicationInvocationID = domain.InvocationID(reentryPublicationPrefix + strings.Repeat("0", 64))
				},
			} {
				forged := binding
				mutate(&forged)
				if err := recordHeldBinding(f.st, forged); !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("re-entered held binding with a foreign %s: %v, want ErrParentKeyMismatch", name, err)
				}
			}
			if err := recordHeldBinding(f.st, binding); err != nil {
				t.Fatal(err)
			}
			got, err := readHeldBinding(f.st, hold.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got != binding {
				t.Fatalf("binding = %#v, want %#v", got, binding)
			}
		})
	}
	t.Run("head the authority did not re-enter for", func(t *testing.T) {
		f := seedReentry(t, domain.ReadinessInvalidationHeadChanged, reentryOptions{})
		if err := f.record(t, f.authority); err != nil {
			t.Fatal(err)
		}
		reference := &domain.PRReference{Repo: f.binding.Repo, Number: f.binding.PRNumber}
		hold := putHeldItem(t, f.st, f.run, f.authority.BlockedItemID(), reentryBase3, reference)
		binding := domain.HeldItemPRBinding(f.binding)
		binding.ItemID, binding.PublicationInvocationID = hold.ID, f.authority.PublicationID()
		binding.HeadSHA = reentryBase3
		if err := recordHeldBinding(f.st, binding); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
}
