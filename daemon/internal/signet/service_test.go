package signet_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// fixture is the §5.14 test bed: a service over a fresh store seeded with one
// open attention item. The item carries no evidence artifacts, so no
// approved-recipe set is needed and the acceptance checks are the only
// policy in play; its offered actions cover the three outcome classes
// (stop resolves, dismiss dismisses, open_pr records without
// concluding).
type fixture struct {
	service *signet.Service
	store   *store.Store
	item    domain.AttentionItem
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, t.TempDir()+"/signet.db", store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	runID := domain.RunID("run-1")
	expires := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Add(24 * time.Hour)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "checks are green and the diff is ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss},
		PRHeadSHA:         "cafebabe", ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate,
		ExpiresWhen:       &expires, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	service := signet.NewService(s)
	if err := service.PutItem(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	return fixture{service: service, store: s, item: item}
}

// TestPutItemRejectsDisallowedAction exercises the signet item boundary: an
// item can be structurally valid and still be illegitimate because its type
// must not offer that action. Rejection happens before the store Write, so it
// neither persists the item nor consumes a server revision.
func TestPutItemRejectsDisallowedAction(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	before := f.revision(t)

	item := f.item
	item.ID = "item-invalid-policy"
	item.Type = domain.AttentionSpecApproval
	item.RequestedDecision = []domain.Action{domain.ActionOpenPR}
	if err := item.Validate(); err != nil {
		t.Fatalf("domain fixture must be structurally valid: %v", err)
	}
	if err := f.service.PutItem(ctx, item); !errors.Is(err, signet.ErrActionNotAllowedForType) {
		t.Fatalf("PutItem error = %v, want ErrActionNotAllowedForType", err)
	}
	if after := f.revision(t); after != before {
		t.Errorf("rejected item moved the revision %d → %d", before, after)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItem(ctx, item.ID)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAttentionItem after rejection = %v, want ErrNotFound", err)
	}
}

// TestPutItemActionlessBlocked is #96's acceptance: an actionless blocked
// item crosses the storage boundary (the plan assigns blocked no action),
// while an empty set on any other type is still rejected by signet policy
// before a Write begins — the cardinality rule lives here, not in domain.
func TestPutItemActionlessBlocked(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	blocked := f.item
	blocked.ID = "item-blocked"
	blocked.Type = domain.AttentionBlocked
	blocked.RequestedDecision = nil
	if err := f.service.PutItem(ctx, blocked); err != nil {
		t.Fatalf("PutItem(actionless blocked): %v", err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, blocked.ID)
		if err != nil {
			return err
		}
		if len(got.RequestedDecision) != 0 {
			t.Errorf("persisted RequestedDecision = %v, want empty", got.RequestedDecision)
		}
		return nil
	}); err != nil {
		t.Fatalf("GetAttentionItem(blocked): %v", err)
	}

	before := f.revision(t)
	actionless := f.item
	actionless.ID = "item-actionless-invalid"
	actionless.RequestedDecision = nil
	if err := actionless.Validate(); err != nil {
		t.Fatalf("domain fixture must be structurally valid: %v", err)
	}
	if err := f.service.PutItem(ctx, actionless); !errors.Is(err, domain.ErrNoActions) {
		t.Fatalf("PutItem error = %v, want ErrNoActions", err)
	}
	if after := f.revision(t); after != before {
		t.Errorf("rejected item moved the revision %d → %d", before, after)
	}
}

// TestSubmitRegatesDurableItemPolicy covers rows that predate the policy or
// bypassed PutItem through an internal store path. New commands fail closed
// against the current per-type table before either a record-only or pending
// action can be accepted/judged, and consume no revision.
func TestSubmitRegatesDurableItemPolicy(t *testing.T) {
	for _, action := range []domain.Action{domain.ActionOpenPR, domain.ActionSnooze} {
		t.Run(string(action), func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			item := f.item
			item.ID = domain.ItemID("item-legacy-" + string(action))
			item.Type = domain.AttentionSpecApproval
			item.RequestedDecision = []domain.Action{action}
			if err := item.Validate(); err != nil {
				t.Fatalf("domain fixture must be structurally valid: %v", err)
			}
			if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, item)
			}); err != nil {
				t.Fatalf("seed direct-store item: %v", err)
			}
			before := f.revision(t)

			cmd := f.command("cmd-legacy-"+string(action), action)
			cmd.Payload.ItemID = item.ID
			if _, err := f.service.Submit(ctx, cmd); !errors.Is(err, signet.ErrActionNotAllowedForType) {
				t.Fatalf("Submit error = %v, want ErrActionNotAllowedForType", err)
			}
			if after := f.revision(t); after != before {
				t.Errorf("rejected command moved the revision %d → %d", before, after)
			}
			if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
				_, err := tx.GetCommand(ctx, cmd.CommandID)
				return err
			}); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetCommand after rejection = %v, want ErrNotFound", err)
			}
		})
	}
}

// command prepares a ClientCommand bound to the fixture item's live state.
func (f fixture) command(commandID string, action domain.Action) signet.ClientCommand {
	return signet.ClientCommand{
		CommandID: commandID, DeviceID: "device-1", ExpectedEntityVersion: 1,
		Payload: signet.DecisionPayload{
			ItemID: f.item.ID, Action: action, ItemVersion: f.item.ItemVersion,
			PRHeadSHA: f.item.PRHeadSHA, ArtifactDigests: f.item.ArtifactDigests,
		},
	}
}

func (f fixture) revision(t *testing.T) int64 {
	t.Helper()
	state, err := f.store.ServerState(context.Background())
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	return state.Revision
}

func (f fixture) itemSnapshot(t *testing.T) (domain.AttentionItem, store.Snapshot) {
	t.Helper()
	var (
		item domain.AttentionItem
		snap store.Snapshot
	)
	if err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		item, snap, err = tx.GetAttentionItemSnapshot(context.Background(), f.item.ID)
		return err
	}); err != nil {
		t.Fatalf("GetAttentionItemSnapshot: %v", err)
	}
	return item, snap
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestSubmitAcceptsAndResolves: an accepted resolving decision commits the
// command and the item transition atomically at one revision (§4:
// "resolutions are transactional and version-checked").
func TestSubmitAcceptsAndResolves(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	before := f.revision(t)

	result, err := f.service.Submit(ctx, f.command("cmd-1", domain.ActionStop))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	after := f.revision(t)
	if after != before+1 {
		t.Errorf("revision moved %d → %d, want exactly one bump", before, after)
	}
	if result.Revision != after {
		t.Errorf("result revision = %d, want the accepting transaction's %d", result.Revision, after)
	}
	if result.Record.CommandID != "cmd-1" || result.Record.Action != domain.ActionStop {
		t.Errorf("record = %+v, want the submitted command", result.Record)
	}

	item, snap := f.itemSnapshot(t)
	if item.Status != domain.StatusResolved || item.ItemVersion != 2 {
		t.Errorf("item after resolve: status %q version %d, want resolved v2", item.Status, item.ItemVersion)
	}
	if snap.AsOfRevision != result.Revision {
		t.Errorf("item as_of_revision = %d, command revision = %d; want the same transaction",
			snap.AsOfRevision, result.Revision)
	}
}

// TestSubmitCrossDeviceConflict is §5.14 test 1: two devices hold the same
// item; the first resolves it; the second device's conflicting submission is
// rejected with the canonical outcome, not accepted and not judged stale.
func TestSubmitCrossDeviceConflict(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	if _, err := f.service.Submit(ctx, f.command("cmd-dev1", domain.ActionStop)); err != nil {
		t.Fatalf("device 1 Submit: %v", err)
	}

	second := f.command("cmd-dev2", domain.ActionDismiss)
	second.DeviceID = "device-2"
	_, err := f.service.Submit(ctx, second)
	if !errors.Is(err, signet.ErrClosedItem) {
		t.Fatalf("device 2 error = %v, want ErrClosedItem", err)
	}
	var closed *signet.ClosedItemError
	if !errors.As(err, &closed) {
		t.Fatalf("error = %v, want *ClosedItemError", err)
	}
	if closed.Item.Status != domain.StatusResolved || closed.Item.ItemVersion != 2 {
		t.Errorf("canonical outcome = %q v%d, want resolved v2", closed.Item.Status, closed.Item.ItemVersion)
	}
	if closed.Snapshot.EntityVersion != 2 || closed.Snapshot.AsOfRevision < 1 {
		t.Errorf("rejection snapshot = %+v, want the resolved row's entity_version 2 with a positive revision", closed.Snapshot)
	}
}

// TestSubmitSupersededVersion is §5.14 test 2: an offline device's submission
// against a superseded version is rejected with the replacement item. Both
// halves of the version check are exercised: the store entity_version
// mismatch that binding equality cannot see, and the payload-binding
// staleness the store detects (translated to the same service error).
func TestSubmitSupersededVersion(t *testing.T) {
	ctx := context.Background()

	advance := func(t *testing.T, f fixture) domain.AttentionItem {
		t.Helper()
		next := f.item
		next.ItemVersion = 2
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutAttentionItem(ctx, next)
		}); err != nil {
			t.Fatalf("advance item: %v", err)
		}
		return next
	}

	assertStaleWithReplacement := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, signet.ErrStaleVersion) {
			t.Fatalf("error = %v, want ErrStaleVersion", err)
		}
		var stale *signet.StaleVersionError
		if !errors.As(err, &stale) {
			t.Fatalf("error = %v, want *StaleVersionError", err)
		}
		if stale.Replacement.ItemVersion != 2 {
			t.Errorf("replacement version = %d, want the advanced 2", stale.Replacement.ItemVersion)
		}
		// The 409 renders an AttentionItemSnapshot, so the metadata the client
		// resubmits against travels with the rejection (no refetch).
		if stale.Snapshot.EntityVersion != 2 || stale.Snapshot.AsOfRevision < 1 {
			t.Errorf("rejection snapshot = %+v, want entity_version 2 with a positive revision", stale.Snapshot)
		}
	}

	t.Run("entity_version mismatch with matching bindings", func(t *testing.T) {
		f := newFixture(t)
		live := advance(t, f)
		// The command binds the live item exactly; only the prepared-against
		// entity_version (1, now 2) is stale. Without the snapshot check this
		// submission would be accepted.
		cmd := f.command("cmd-stale-ev", domain.ActionStop)
		cmd.Payload.ItemVersion = live.ItemVersion
		_, err := f.service.Submit(ctx, cmd)
		assertStaleWithReplacement(t, err)
	})

	t.Run("binding staleness with matching entity_version", func(t *testing.T) {
		f := newFixture(t)
		advance(t, f)
		// The device refreshed its entity_version (2) but rendered its decision
		// against the old payload bindings (item_version 1): the store's
		// binding check fires and is translated to the service error.
		cmd := f.command("cmd-stale-bind", domain.ActionStop)
		cmd.ExpectedEntityVersion = 2
		_, err := f.service.Submit(ctx, cmd)
		assertStaleWithReplacement(t, err)
	})

	t.Run("no side effect was applied", func(t *testing.T) {
		f := newFixture(t)
		advance(t, f)
		before := f.revision(t)
		if _, err := f.service.Submit(ctx, f.command("cmd-stale", domain.ActionStop)); err == nil {
			t.Fatal("stale Submit succeeded, want rejection")
		}
		if after := f.revision(t); after != before {
			t.Errorf("rejected submission moved the revision %d → %d", before, after)
		}
		if item, _ := f.itemSnapshot(t); item.Status != domain.StatusOpen {
			t.Errorf("rejected submission changed the item status to %q", item.Status)
		}
	})
}

// TestSubmitRetryIdempotent is §5.14 test 4: a lost-response retry by
// command_id returns the original committed result with no second effect,
// even after the item's lifecycle has concluded; a changed body under the
// same command_id is a conflict, never a silent overwrite.
func TestSubmitRetryIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	original, err := f.service.Submit(ctx, f.command("cmd-1", domain.ActionStop))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	before := f.revision(t)

	retried, err := f.service.Submit(ctx, f.command("cmd-1", domain.ActionStop))
	if err != nil {
		t.Fatalf("retry Submit: %v", err)
	}
	if marshal(t, retried) != marshal(t, original) {
		t.Errorf("retry result differs from the original:\ngot:  %s\nwant: %s",
			marshal(t, retried), marshal(t, original))
	}
	if retried.Revision != original.Revision {
		t.Errorf("retry revision = %d, want the original %d", retried.Revision, original.Revision)
	}
	if after := f.revision(t); after != before {
		t.Errorf("retry moved the revision %d → %d, want no second effect", before, after)
	}
	if item, _ := f.itemSnapshot(t); item.ItemVersion != 2 {
		t.Errorf("retry re-applied the transition: item at v%d, want v2", item.ItemVersion)
	}

	mutated := f.command("cmd-1", domain.ActionDismiss)
	if _, err := f.service.Submit(ctx, mutated); !errors.Is(err, store.ErrImmutableConflict) {
		t.Errorf("mutated retry error = %v, want ErrImmutableConflict", err)
	}
}

// TestSubmitClosedItemCurrentVersion exercises issue #55's regression class
// at the service boundary: a command carrying the item's current
// entity_version and bindings is still rejected once the lifecycle has
// concluded, with the canonical item, and closure outranks staleness for a
// stale-bound command too.
func TestSubmitClosedItemCurrentVersion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// Conclude the lifecycle out-of-band (the direct store path a workflow
	// reaction would take), then submit against the now-current state.
	closed := f.item
	closed.ItemVersion = 2
	closed.Status = domain.StatusResolved
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, closed)
	}); err != nil {
		t.Fatalf("close item: %v", err)
	}

	current := f.command("cmd-current", domain.ActionStop)
	current.ExpectedEntityVersion = 2 // matches the live row exactly
	current.Payload.ItemVersion = 2
	_, err := f.service.Submit(ctx, current)
	if !errors.Is(err, signet.ErrClosedItem) {
		t.Fatalf("current-version command error = %v, want ErrClosedItem", err)
	}
	var carrier *signet.ClosedItemError
	if !errors.As(err, &carrier) {
		t.Fatalf("error = %v, want *ClosedItemError", err)
	}
	if carrier.Item.Status != domain.StatusResolved {
		t.Errorf("canonical item status = %q, want resolved", carrier.Item.Status)
	}

	stale := f.command("cmd-stale-closed", domain.ActionStop)
	if _, err := f.service.Submit(ctx, stale); !errors.Is(err, signet.ErrClosedItem) {
		t.Errorf("stale command on closed item error = %v, want ErrClosedItem (closure outranks staleness)", err)
	}
}

// TestSubmitNonConcludingAction: an action plan §4 marks as non-resolving
// (open_pr is navigation) records the command without touching the item, so
// the item stays open at its version and entity_version.
func TestSubmitNonConcludingAction(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	before := f.revision(t)

	result, err := f.service.Submit(ctx, f.command("cmd-nav", domain.ActionOpenPR))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.Revision != before+1 {
		t.Errorf("result revision = %d, want the accepting transaction's %d", result.Revision, before+1)
	}
	item, snap := f.itemSnapshot(t)
	if item.Status != domain.StatusOpen || item.ItemVersion != 1 {
		t.Errorf("item after navigation action: status %q v%d, want open v1", item.Status, item.ItemVersion)
	}
	if snap.EntityVersion != 1 {
		t.Errorf("item entity_version = %d, want untouched 1", snap.EntityVersion)
	}
}

// TestSubmitRejectsInvalidAndUnknown: malformed input fails before any
// transaction, an unknown item wraps the store's not-found, and an action the
// item never offered passes the store's gate error through.
func TestSubmitRejectsInvalidAndUnknown(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	t.Run("invalid command fails before any transaction", func(t *testing.T) {
		before := f.revision(t)
		bad := f.command("", domain.ActionStop)
		if _, err := f.service.Submit(ctx, bad); err == nil {
			t.Fatal("empty command_id accepted, want validation error")
		}
		if after := f.revision(t); after != before {
			t.Errorf("invalid input moved the revision %d → %d", before, after)
		}
	})

	t.Run("non-positive expected_entity_version rejected", func(t *testing.T) {
		bad := f.command("cmd-ev0", domain.ActionStop)
		bad.ExpectedEntityVersion = 0
		if _, err := f.service.Submit(ctx, bad); !errors.Is(err, domain.ErrNonPositive) {
			t.Errorf("error = %v, want ErrNonPositive", err)
		}
	})

	t.Run("unknown item wraps store not-found", func(t *testing.T) {
		missing := f.command("cmd-missing", domain.ActionStop)
		missing.Payload.ItemID = "item-none"
		if _, err := f.service.Submit(ctx, missing); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("action not offered passes through", func(t *testing.T) {
		notOffered := f.command("cmd-approve", domain.ActionApprove)
		if _, err := f.service.Submit(ctx, notOffered); !errors.Is(err, store.ErrActionNotOffered) {
			t.Errorf("error = %v, want ErrActionNotOffered", err)
		}
	})

	t.Run("pending-unit actions rejected without side effects", func(t *testing.T) {
		// The full pending class: actions whose transaction a later unit owns
		// (discuss's conversation, #68; snooze's timing update;
		// start_with_changes's revised artifact and supersede) or whose
		// decision carries parameters or conversation-borne content
		// DecisionPayload cannot represent yet. Each must fail loudly
		// instead of recording a command whose data is silently dropped
		// (and discuss must not be double-acceptable at one item version,
		// §5.14 test 7).
		before := f.revision(t)
		pending := []domain.Action{
			domain.ActionDiscuss, domain.ActionSnooze, domain.ActionStartWithChanges,
			domain.ActionContinueUnderPolicy, domain.ActionConvertToPolicy,
			domain.ActionAdjudicate, domain.ActionRetryWithCapability,
			domain.ActionChooseAlternate, domain.ActionRequestChanges,
			domain.ActionAnswerAndRetry, domain.ActionAnswerWithoutRetry,
			domain.ActionReturnToAgent,
		}
		for _, action := range pending {
			cmd := f.command("cmd-pending-"+string(action), action)
			if _, err := f.service.Submit(ctx, cmd); !errors.Is(err, signet.ErrUnsupportedAction) {
				t.Errorf("%s error = %v, want ErrUnsupportedAction", action, err)
			}
		}
		if after := f.revision(t); after != before {
			t.Errorf("pending-action rejection moved the revision %d → %d", before, after)
		}
	})

	t.Run("committed command_id resubmitted as a pending action conflicts", func(t *testing.T) {
		// Idempotency by command_id is judged before the pending-action gate:
		// an id already on record with a supported action, reused with a
		// pending one, is a changed body under an immutable id, never an
		// ErrUnsupportedAction that would hide the collision.
		if _, err := f.service.Submit(ctx, f.command("cmd-reused", domain.ActionOpenPR)); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		reused := f.command("cmd-reused", domain.ActionDiscuss)
		if _, err := f.service.Submit(ctx, reused); !errors.Is(err, store.ErrImmutableConflict) {
			t.Errorf("reused id error = %v, want ErrImmutableConflict", err)
		}
	})
}

// TestSubmitDismissingAction: dismiss concludes the item as dismissed, the
// third outcome class next to resolve and leave-untouched.
func TestSubmitDismissingAction(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	if _, err := f.service.Submit(ctx, f.command("cmd-dismiss", domain.ActionDismiss)); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	item, _ := f.itemSnapshot(t)
	if item.Status != domain.StatusDismissed || item.ItemVersion != 2 {
		t.Errorf("item after dismiss: status %q v%d, want dismissed v2", item.Status, item.ItemVersion)
	}
}
