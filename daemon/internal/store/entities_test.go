package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestGoldenRoundTrip is acceptance fixture 7: every persisted domain shape
// has a stable golden form and round-trips store -> domain equal. All the
// puts share one Write, which doubles as the multi-put half of fixture 5:
// one client-visible transaction, one revision bump.
func TestGoldenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)

	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	err = s.Write(ctx, func(tx *store.WriteTx) error {
		// Referential order: parents before their foreign-key dependents.
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		if err := tx.PutConversation(ctx, f.conversation); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, f.invocation); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, f.artifact); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, f.item); err != nil {
			return err
		}
		if err := tx.PutAttentionDelivery(ctx, f.delivery); err != nil {
			return err
		}
		if err := tx.PutFinding(ctx, f.finding); err != nil {
			return err
		}
		if err := tx.PutClassification(ctx, f.class); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, f.policy); err != nil {
			return err
		}
		if err := tx.PutDevice(ctx, f.device); err != nil {
			return err
		}
		return tx.PutCommand(ctx, f.command)
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if after.Revision != before.Revision+1 {
		t.Fatalf("eleven puts in one Write bumped revision %d -> %d, want exactly once", before.Revision, after.Revision)
	}

	cases := []struct {
		name string
		put  any // the fixture that went in
		get  func(tx *store.ReadTx) (any, error)
	}{
		{"run", f.run, func(tx *store.ReadTx) (any, error) { return tx.GetRun(ctx, f.run.ID) }},
		{"conversation", f.conversation, func(tx *store.ReadTx) (any, error) { return tx.GetConversation(ctx, f.conversation.ID) }},
		{"agent_invocation", f.invocation, func(tx *store.ReadTx) (any, error) { return tx.GetAgentInvocation(ctx, f.invocation.ID) }},
		{"artifact", f.artifact, func(tx *store.ReadTx) (any, error) { return tx.GetArtifact(ctx, f.artifact.ID) }},
		{"attention_item", f.item, func(tx *store.ReadTx) (any, error) { return tx.GetAttentionItem(ctx, f.item.ID) }},
		{"attention_delivery", f.delivery, func(tx *store.ReadTx) (any, error) {
			return tx.GetAttentionDelivery(ctx, f.delivery.ItemID, f.delivery.DeviceID, f.delivery.Channel, f.delivery.Attempt)
		}},
		{"finding", f.finding, func(tx *store.ReadTx) (any, error) { return tx.GetFinding(ctx, f.finding.ID) }},
		{"classification", f.class, func(tx *store.ReadTx) (any, error) {
			return tx.GetClassification(ctx, f.class.FindingID, f.class.Version)
		}},
		{"resolved_policy", f.policy, func(tx *store.ReadTx) (any, error) { return tx.GetResolvedPolicy(ctx, f.policy.RunID) }},
		{"command", f.command, func(tx *store.ReadTx) (any, error) { return tx.GetCommand(ctx, f.command.CommandID) }},
		{"device", f.device, func(tx *store.ReadTx) (any, error) { return tx.GetDevice(ctx, f.device.ID) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got any
			err := s.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				got, err = tc.get(tx)
				return err
			})
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			// Compare serialized forms: JSON equality is the round-trip
			// contract, and it sidesteps time.Time's DeepEqual pitfalls.
			gotJSON := marshalIndent(t, got)
			wantJSON := marshalIndent(t, tc.put)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("round-trip mismatch:\ngot:  %s\nwant: %s", gotJSON, wantJSON)
			}
			golden.Assert(t, tc.name, gotJSON)
		})
	}
}

func marshalIndent(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(b, '\n')
}

// TestPutImmutableConflict: write-once records tolerate a byte-identical
// replay (a retry converges on the original row) but refuse an in-place
// rewrite: the domain corrects them with new versions or identities, so a
// same-key write with different content is a contract violation, not an
// update. A rewritten classification version, for example, would erase the
// historical materiality/confidence while sync saw only an ordinary update.
//
// resolved_policy is intentionally absent: its digest is a content address of
// its canonically-ordered keys and the run pins exactly one digest, so a
// same-run write is either identical canonical content (an idempotent replay
// that converges) or different content (a different digest, rejected by content
// validation or the run binding). A distinct persisted body under one run
// cannot be expressed, so the write-once check is unreachable here. That
// stronger invariant is covered by TestResolvedPolicyDigestMatchesRun.
func TestPutImmutableConflict(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)

	seed := func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, f.artifact); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, f.invocation); err != nil {
			return err
		}
		if err := tx.PutFinding(ctx, f.finding); err != nil {
			return err
		}
		if err := tx.PutClassification(ctx, f.class); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, f.policy)
	}

	cases := []struct {
		name    string
		replay  func(tx *store.WriteTx) error // byte-identical re-put
		rewrite func(tx *store.WriteTx) error // same key, different content
	}{
		{
			"artifact",
			func(tx *store.WriteTx) error { return tx.PutArtifact(ctx, f.artifact) },
			func(tx *store.WriteTx) error {
				changed := f.artifact
				changed.Type = "changed_type"
				return tx.PutArtifact(ctx, changed)
			},
		},
		{
			"agent invocation",
			func(tx *store.WriteTx) error { return tx.PutAgentInvocation(ctx, f.invocation) },
			func(tx *store.WriteTx) error {
				changed := f.invocation
				changed.InputIDs = []domain.ArtifactID{"art-other"}
				return tx.PutAgentInvocation(ctx, changed)
			},
		},
		{
			"finding",
			func(tx *store.WriteTx) error { return tx.PutFinding(ctx, f.finding) },
			func(tx *store.WriteTx) error {
				changed := f.finding
				changed.Message = "rewritten observation"
				return tx.PutFinding(ctx, changed)
			},
		},
		{
			"classification",
			func(tx *store.WriteTx) error { return tx.PutClassification(ctx, f.class) },
			func(tx *store.WriteTx) error {
				changed := f.class
				changed.Materiality = "low"
				return tx.PutClassification(ctx, changed)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
			if err := s.Write(ctx, seed); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := s.Write(ctx, tc.replay); err != nil {
				t.Fatalf("identical replay errored, want idempotent success: %v", err)
			}
			err := s.Write(ctx, tc.rewrite)
			if !errors.Is(err, store.ErrImmutableConflict) {
				t.Fatalf("rewrite error = %v, want ErrImmutableConflict", err)
			}
		})
	}
}

// TestCommandIdempotentAndStale covers the command record's two guarantees
// (issue #32 acceptance 3; §5.14 tests 2 and 4):
//   - a retried command_id converges on the original record, and a different
//     body under that id is a conflict;
//   - a new command whose pinned bindings no longer match the live item is
//     rejected as stale, carrying the current item as the replacement;
//   - a command for an absent item is not found.
func TestCommandIdempotentAndStale(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)

	seed := func(s *store.Store) {
		t.Helper()
		err := s.Write(ctx, func(tx *store.WriteTx) error {
			if err := tx.PutConversation(ctx, f.conversation); err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, f.item); err != nil {
				return err
			}
			return tx.PutCommand(ctx, f.command)
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	t.Run("idempotent replay returns the original", func(t *testing.T) {
		s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
		seed(s)
		if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutCommand(ctx, f.command) }); err != nil {
			t.Fatalf("identical replay errored, want convergence: %v", err)
		}
		var got domain.Command
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			got, err = tx.GetCommand(ctx, f.command.CommandID)
			return err
		}); err != nil {
			t.Fatalf("GetCommand: %v", err)
		}
		if string(marshalIndent(t, got)) != string(marshalIndent(t, f.command)) {
			t.Fatalf("replayed command changed:\ngot:  %s\nwant: %s", marshalIndent(t, got), marshalIndent(t, f.command))
		}
	})

	t.Run("changed body under the same id conflicts", func(t *testing.T) {
		s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
		seed(s)
		changed := f.command
		changed.Action = domain.ActionReturnToAgent // same command_id, different decision
		err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutCommand(ctx, changed) })
		if !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("changed-body replay error = %v, want ErrImmutableConflict", err)
		}
	})

	t.Run("committed command still converges after the item advanced", func(t *testing.T) {
		// The crux of §5.14 test 4: a lost-response retry of an already-committed
		// command_id must return the original result even though the item has since
		// advanced past the version the command bound. This is what the
		// idempotency-before-binding order in PutCommand guarantees; without it the
		// retry would be wrongly re-judged stale.
		s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
		seed(s)
		advanced := f.item
		advanced.Status = domain.StatusResolved
		advanced.ItemVersion = 2
		if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, advanced) }); err != nil {
			t.Fatalf("advance item: %v", err)
		}
		// Retrying the original command (still bound to v1) converges, not stale.
		if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutCommand(ctx, f.command) }); err != nil {
			t.Fatalf("retry after advance errored, want convergence: %v", err)
		}
		var got domain.Command
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			got, err = tx.GetCommand(ctx, f.command.CommandID)
			return err
		}); err != nil {
			t.Fatalf("GetCommand: %v", err)
		}
		if got.ItemVersion != 1 {
			t.Errorf("retrieved command item_version = %d, want the originally recorded 1", got.ItemVersion)
		}
	})

	t.Run("stale bindings rejected with the replacement item", func(t *testing.T) {
		s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
		seed(s)
		// Advance the still-open item to v2 (a legitimate transition) after the
		// command bound v1; the item stays open so only staleness is at fault.
		advanced := f.item
		advanced.ItemVersion = 2
		if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, advanced) }); err != nil {
			t.Fatalf("advance item: %v", err)
		}
		// A genuinely new command still binding v1 is stale.
		stale := f.command
		stale.CommandID = "cmd-stale"
		err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutCommand(ctx, stale) })
		if !errors.Is(err, store.ErrStaleCommand) {
			t.Fatalf("stale command error = %v, want ErrStaleCommand", err)
		}
		var sce *store.StaleCommandError
		if !errors.As(err, &sce) {
			t.Fatalf("error = %v, want *StaleCommandError", err)
		}
		if sce.Replacement.ItemVersion != 2 {
			t.Errorf("replacement item_version = %d, want the advanced 2", sce.Replacement.ItemVersion)
		}
	})

	t.Run("new command on a closed item rejected with the canonical item", func(t *testing.T) {
		// The current-version half of issue #55: a command prepared against the
		// resolved item's live bindings passes BindsSameAs, so without the status
		// gate it would be durably recorded as a decision on an already-closed
		// item. Version advance alone does not catch this.
		s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
		seed(s)
		resolved := f.item
		resolved.Status = domain.StatusResolved
		resolved.ItemVersion = 2
		if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, resolved) }); err != nil {
			t.Fatalf("resolve item: %v", err)
		}
		// A genuinely new command bound to the resolved item's current v2 (same
		// head and digest set, so its bindings match exactly).
		current := f.command
		current.CommandID = "cmd-on-closed"
		current.ItemVersion = 2
		err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutCommand(ctx, current) })
		if !errors.Is(err, store.ErrClosedItem) {
			t.Fatalf("closed-item command error = %v, want ErrClosedItem", err)
		}
		var cie *store.ClosedItemError
		if !errors.As(err, &cie) {
			t.Fatalf("error = %v, want *ClosedItemError", err)
		}
		if cie.Item.Status != domain.StatusResolved || cie.Item.ItemVersion != 2 {
			t.Errorf("canonical item = %s v%d, want resolved v2", cie.Item.Status, cie.Item.ItemVersion)
		}

		// A stale-bound new command against the closed item also reports closure,
		// not staleness: the openness gate runs first, since a replacement item
		// inviting a rebind-and-retry can never lead to an accepted command.
		staleOnClosed := f.command
		staleOnClosed.CommandID = "cmd-stale-on-closed"
		err = s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutCommand(ctx, staleOnClosed) })
		if !errors.Is(err, store.ErrClosedItem) {
			t.Fatalf("stale command on closed item = %v, want ErrClosedItem", err)
		}
		cie = nil
		if !errors.As(err, &cie) {
			t.Fatalf("error = %v, want *ClosedItemError", err)
		}
		if cie.Item.Status != domain.StatusResolved || cie.Item.ItemVersion != 2 {
			t.Errorf("canonical item = %s v%d, want resolved v2", cie.Item.Status, cie.Item.ItemVersion)
		}
	})

	t.Run("action not offered by the item is rejected", func(t *testing.T) {
		s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
		// Seed the item (and its conversation) but not a command.
		if err := s.Write(ctx, func(tx *store.WriteTx) error {
			if err := tx.PutConversation(ctx, f.conversation); err != nil {
				return err
			}
			return tx.PutAttentionItem(ctx, f.item)
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// The fixture item offers open_pr / return_to_agent / dismiss; a command
		// for the valid-but-unoffered "stop" action must be refused even though it
		// binds the current version, head, and digests.
		unoffered, err := domain.NewCommand(domain.CommandInput{
			CommandID: "cmd-unoffered", DeviceID: "device-1", ItemID: f.item.ID,
			ItemVersion: f.item.ItemVersion, PRHeadSHA: f.item.PRHeadSHA,
			ArtifactDigests: f.item.ArtifactDigests, Action: domain.ActionStop,
		})
		if err != nil {
			t.Fatalf("NewCommand: %v", err)
		}
		err = s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutCommand(ctx, unoffered) })
		if !errors.Is(err, store.ErrActionNotOffered) {
			t.Fatalf("unoffered action error = %v, want ErrActionNotOffered", err)
		}
	})

	t.Run("command for an absent item is not found", func(t *testing.T) {
		s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
		// No item seeded.
		orphan := f.command
		orphan.CommandID = "cmd-orphan"
		orphan.ItemID = "no-such-item"
		err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutCommand(ctx, orphan) })
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("command for missing item error = %v, want ErrNotFound", err)
		}
	})
}

// TestPutAgainUpdates: a second Put of the same identity on a current-state
// aggregate replaces the body (the upsert path) when the change is a
// legitimate evolution: here a run gaining a retry attempt and a new stage.
func TestPutAgainUpdates(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)

	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, f.run) }); err != nil {
		t.Fatalf("first put: %v", err)
	}
	updated := f.run
	updated.Stages = append([]domain.Stage{}, f.run.Stages...)
	updated.Stages[0].Attempts = append(append([]domain.Attempt{}, f.run.Stages[0].Attempts...),
		domain.Attempt{ID: "attempt-2", StageID: "stage-1", Number: 2, InvocationID: "inv-2"})
	updated.Stages = append(updated.Stages, domain.Stage{ID: "stage-2", RunID: "run-1", Name: "review"})
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, updated) }); err != nil {
		t.Fatalf("second put: %v", err)
	}
	err := s.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetRun(ctx, f.run.ID)
		if err != nil {
			return err
		}
		if len(got.Stages) != 2 || len(got.Stages[0].Attempts) != 2 {
			t.Fatalf("run after append = %d stages / %d attempts, want 2/2", len(got.Stages), len(got.Stages[0].Attempts))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
}

// TestRunFixedBindingsAndHistory: a run's project, spec digest, and policy
// digest are fixed at creation, and its recorded stages and attempts only
// append; a Put that would retarget or rewrite any of them fails.
func TestRunFixedBindingsAndHistory(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)

	cases := []struct {
		name   string
		mutate func(run domain.Run) domain.Run
	}{
		{"project retargeted", func(run domain.Run) domain.Run {
			run.ProjectID = "proj-other"
			return run
		}},
		{"spec digest changed", func(run domain.Run) domain.Run {
			run.SpecDigest = "sha256:spec-v2"
			return run
		}},
		{"policy digest changed", func(run domain.Run) domain.Run {
			run.PolicyDigest = "sha256:policy-v2"
			return run
		}},
		{"stage dropped", func(run domain.Run) domain.Run {
			run.Stages = nil
			return run
		}},
		{"attempt rewritten", func(run domain.Run) domain.Run {
			stages := append([]domain.Stage{}, run.Stages...)
			stages[0].Attempts = []domain.Attempt{{ID: "attempt-x", StageID: "stage-1", Number: 1, InvocationID: "inv-x"}}
			run.Stages = stages
			return run
		}},
		{"stage renamed", func(run domain.Run) domain.Run {
			stages := append([]domain.Stage{}, run.Stages...)
			stages[0].Name = "verification"
			run.Stages = stages
			return run
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
			if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, f.run) }); err != nil {
				t.Fatalf("seed: %v", err)
			}
			err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, tc.mutate(f.run)) })
			if !errors.Is(err, store.ErrImmutableConflict) {
				t.Fatalf("mutating put error = %v, want ErrImmutableConflict", err)
			}
		})
	}
}

// TestConversationAppendOnly: stored messages are immutable (§5.14); an
// update must carry them unchanged and may only append.
func TestConversationAppendOnly(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)
	second := domain.Message{
		ID: "msg-2", ConversationID: "conv-1", Sequence: 2,
		Author: domain.AuthorAgent, Body: "done", CreatedAt: f.conversation.Messages[0].CreatedAt.Add(time.Minute),
	}

	t.Run("append succeeds", func(t *testing.T) {
		s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
		if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutConversation(ctx, f.conversation) }); err != nil {
			t.Fatalf("seed: %v", err)
		}
		grown := f.conversation
		grown.Messages = append(append([]domain.Message{}, f.conversation.Messages...), second)
		if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutConversation(ctx, grown) }); err != nil {
			t.Fatalf("append: %v", err)
		}
	})

	cases := []struct {
		name   string
		mutate func(c domain.Conversation) domain.Conversation
	}{
		{"message dropped", func(c domain.Conversation) domain.Conversation {
			c.Messages = nil
			return c
		}},
		{"message rewritten", func(c domain.Conversation) domain.Conversation {
			messages := append([]domain.Message{}, c.Messages...)
			messages[0].Body = "rewritten"
			c.Messages = messages
			return c
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
			if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutConversation(ctx, f.conversation) }); err != nil {
				t.Fatalf("seed: %v", err)
			}
			err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutConversation(ctx, tc.mutate(f.conversation)) })
			if !errors.Is(err, store.ErrImmutableConflict) {
				t.Fatalf("mutating put error = %v, want ErrImmutableConflict", err)
			}
		})
	}
}

// TestAttentionItemFixedBindings: what an item is about (project, subject,
// type) is fixed at creation; transitions evolve status and evidence on the
// same identity, and a different subject is a new superseding item.
func TestAttentionItemFixedBindings(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)

	seed := func(s *store.Store) {
		t.Helper()
		err := s.Write(ctx, func(tx *store.WriteTx) error {
			if err := tx.PutConversation(ctx, f.conversation); err != nil {
				return err
			}
			return tx.PutAttentionItem(ctx, f.item)
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	t.Run("status transition succeeds", func(t *testing.T) {
		s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
		seed(s)
		resolved := f.item
		resolved.Status = domain.StatusResolved
		resolved.ItemVersion = 2
		if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, resolved) }); err != nil {
			t.Fatalf("transition: %v", err)
		}
	})

	cases := []struct {
		name   string
		mutate func(item domain.AttentionItem) domain.AttentionItem
	}{
		{"project retargeted", func(item domain.AttentionItem) domain.AttentionItem {
			item.ProjectID = "proj-other"
			return item
		}},
		{"subject retargeted", func(item domain.AttentionItem) domain.AttentionItem {
			item.Subject.ID = "run-other"
			runID := domain.RunID("run-other")
			item.Subject.RunID = &runID
			return item
		}},
		{"type changed", func(item domain.AttentionItem) domain.AttentionItem {
			item.Type = domain.AttentionAgentQuestion
			item.RequestedDecision = []domain.Action{domain.ActionAnswerAndRetry, domain.ActionDismiss}
			return item
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
			seed(s)
			err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, tc.mutate(f.item)) })
			if !errors.Is(err, store.ErrImmutableConflict) {
				t.Fatalf("mutating put error = %v, want ErrImmutableConflict", err)
			}
		})
	}
}

// TestAttentionItemStaleWriteRejected: a changed item body must advance
// item_version, or a stale copy could roll back a later transition (a
// resolved v2 overwritten by an open v1); a byte-identical replay converges
// silently.
func TestAttentionItemStaleWriteRejected(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)

	resolved := f.item
	resolved.Status = domain.StatusResolved
	resolved.ItemVersion = 2
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutConversation(ctx, f.conversation); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, f.item); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, resolved) // v1 -> v2 transition
	})
	if err != nil {
		t.Fatalf("seed and transition: %v", err)
	}

	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, resolved) }); err != nil {
		t.Fatalf("identical replay errored, want silent convergence: %v", err)
	}
	err = s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, f.item) }) // stale v1
	if !errors.Is(err, store.ErrStaleWrite) {
		t.Fatalf("stale v1 write error = %v, want ErrStaleWrite", err)
	}
	sameVersion := resolved
	sameVersion.Reason = "changed without a version bump"
	err = s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, sameVersion) })
	if !errors.Is(err, store.ErrStaleWrite) {
		t.Fatalf("same-version changed body error = %v, want ErrStaleWrite", err)
	}
}

// TestDeliveryLifecycleForwardOnly: a delivery's lifecycle only advances
// (submitted -> channel_accepted -> opened) and recorded receipts never
// change; a stale retry must not roll an opened delivery back to submitted.
func TestDeliveryLifecycleForwardOnly(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)

	submitted := domain.AttentionDelivery{
		ItemID: f.delivery.ItemID, DeviceID: f.delivery.DeviceID,
		Channel: f.delivery.Channel, Attempt: f.delivery.Attempt,
		SubmittedAt: f.delivery.SubmittedAt, Status: domain.DeliverySubmitted,
	}
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutConversation(ctx, f.conversation); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, f.item); err != nil {
			return err
		}
		return tx.PutAttentionDelivery(ctx, submitted)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Advance to opened (the fixture carries all receipts, same key).
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionDelivery(ctx, f.delivery) }); err != nil {
		t.Fatalf("advance to opened: %v", err)
	}
	// An identical replay converges silently.
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionDelivery(ctx, f.delivery) }); err != nil {
		t.Fatalf("identical replay errored, want silent convergence: %v", err)
	}
	// A stale retry regressing to submitted is rejected.
	err = s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionDelivery(ctx, submitted) })
	if !errors.Is(err, store.ErrStaleWrite) {
		t.Fatalf("regression error = %v, want ErrStaleWrite", err)
	}
	// An advance that rewrites an already-recorded receipt is rejected.
	// Rebuild from the submitted row: fresh store, seed submitted, then
	// advance with a shifted SubmittedAt.
	s2 := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	err = s2.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutConversation(ctx, f.conversation); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, f.item); err != nil {
			return err
		}
		return tx.PutAttentionDelivery(ctx, submitted)
	})
	if err != nil {
		t.Fatalf("seed second store: %v", err)
	}
	shifted := f.delivery
	shifted.SubmittedAt = f.delivery.SubmittedAt.Add(time.Second)
	err = s2.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionDelivery(ctx, shifted) })
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("receipt rewrite error = %v, want ErrImmutableConflict", err)
	}
}

// TestResolvedPolicyDigestMatchesRun: the digest is a verified content address
// and the run binds its policy by that digest (§5.3, §5.12). Two rejections
// guard the boundary, and the store's binding is the second: a forged digest
// (content changed, old digest kept) fails content validation on encode; an
// authentic policy whose content does not match the run's pinned policy_digest
// fails the store's run binding; only the run's own policy is accepted.
func TestResolvedPolicyDigestMatchesRun(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)

	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, f.run) }); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A forged digest (content changed under the run's expected digest) is
	// rejected by content validation, before the run binding is even consulted.
	forged := f.policy
	forged.Digest = "sha256:policy-other"
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutResolvedPolicy(ctx, forged) }); !errors.Is(err, domain.ErrPolicyDigestMismatch) {
		t.Fatalf("forged policy digest error = %v, want ErrPolicyDigestMismatch", err)
	}

	// An authentic policy for different content has a valid digest of its own,
	// so it passes content validation, but that digest is not what the run
	// pinned: the store's run binding rejects it.
	otherContent, err := domain.NewResolvedPolicy(f.policy.RunID, []domain.PolicyKey{{
		Key: "rein", Value: "loose",
		Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: "sha256:override"},
	}})
	if err != nil {
		t.Fatalf("NewResolvedPolicy: %v", err)
	}
	err = s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutResolvedPolicy(ctx, otherContent) })
	if err == nil || errors.Is(err, domain.ErrPolicyDigestMismatch) {
		t.Fatalf("authentic wrong-run policy error = %v, want the store run-binding rejection", err)
	}

	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutResolvedPolicy(ctx, f.policy) }); err != nil {
		t.Fatalf("matching policy digest rejected: %v", err)
	}
}

// TestGetNotFound: every Get wraps ErrNotFound for a missing row.
func TestGetNotFound(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetRun(ctx, "no-such-run")
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRun error = %v, want ErrNotFound", err)
	}
}

// TestForeignKeysEnforced is acceptance fixture 6: an orphaning write fails,
// proving foreign_keys=ON is effective on the write path, not merely set.
func TestForeignKeysEnforced(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t)

	cases := []struct {
		name string
		put  func(tx *store.WriteTx) error
	}{
		{"delivery without item", func(tx *store.WriteTx) error {
			return tx.PutAttentionDelivery(ctx, f.delivery)
		}},
		{"finding without run", func(tx *store.WriteTx) error {
			return tx.PutFinding(ctx, f.finding)
		}},
		{"classification without finding", func(tx *store.WriteTx) error {
			return tx.PutClassification(ctx, f.class)
		}},
		{"policy without run", func(tx *store.WriteTx) error {
			return tx.PutResolvedPolicy(ctx, f.policy)
		}},
		{"item with dangling conversation", func(tx *store.WriteTx) error {
			return tx.PutAttentionItem(ctx, f.item) // conversation_id conv-1 never inserted
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()}) // fresh store per case
			before, err := s.ServerState(ctx)
			if err != nil {
				t.Fatalf("ServerState: %v", err)
			}
			if err := s.Write(ctx, tc.put); err == nil {
				t.Fatal("orphaning write succeeded, want foreign-key failure")
			}
			after, err := s.ServerState(ctx)
			if err != nil {
				t.Fatalf("ServerState: %v", err)
			}
			if after.Revision != before.Revision {
				t.Fatalf("failed Write moved revision %d -> %d, want rollback", before.Revision, after.Revision)
			}
		})
	}
}
