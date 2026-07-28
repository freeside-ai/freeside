package signet_test

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// seedHealthItem adds an open system_health item offering stop_unattended
// (the waived-posture shape once #319 lets the engine offer it).
func seedHealthItem(t *testing.T, f fixture, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "unattended execution admitted under the backup-encryption waiver",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge, domain.ActionStopUnattended},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	if err := f.service.PutItem(context.Background(), item); err != nil {
		t.Fatalf("seed health item: %v", err)
	}
	return item
}

// commandOn builds a ClientCommand against an arbitrary seeded item (the
// shared fixture helper binds the fixture's own item).
func commandOn(item domain.AttentionItem, commandID string, action domain.Action) signet.ClientCommand {
	return signet.ClientCommand{
		CommandID: commandID, DeviceID: "device-1", ExpectedEntityVersion: 1,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: action, ItemVersion: item.ItemVersion,
			PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
		},
	}
}

func latestTransition(t *testing.T, s *store.Store) (domain.UnattendedOperationTransition, bool) {
	t.Helper()
	ctx := context.Background()
	var (
		tr    domain.UnattendedOperationTransition
		found bool
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		tr, found, err = tx.LatestUnattendedOperationTransition(ctx)
		return err
	}); err != nil {
		t.Fatalf("LatestUnattendedOperationTransition: %v", err)
	}
	return tr, found
}

func openHealthItems(t *testing.T, s *store.Store) []domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	var items []domain.AttentionItem
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		items, err = tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		return err
	}); err != nil {
		t.Fatalf("ListOpenAttentionItems: %v", err)
	}
	return items
}

// TestSubmitStopUnattended is #319's accepting transaction: one Write
// commits the command record, the resolved item, the durable stopped
// transition bound to the command, and the resume-offering notice; an
// idempotent replay converges with no second effect.
func TestSubmitStopUnattended(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	health := seedHealthItem(t, f, "health-1")
	before := f.revision(t)

	result, err := f.service.Submit(ctx, commandOn(health, "cmd-stop", domain.ActionStopUnattended))
	if err != nil {
		t.Fatalf("Submit(stop_unattended): %v", err)
	}
	if after := f.revision(t); after != before+1 {
		t.Errorf("revision moved %d → %d, want exactly one bump", before, after)
	}

	tr, found := latestTransition(t, f.store)
	if !found || tr.State != domain.UnattendedStopped {
		t.Fatalf("latest transition = %+v (found %v), want stopped", tr, found)
	}
	if tr.CommandID == nil || *tr.CommandID != "cmd-stop" {
		t.Errorf("transition command binding = %v, want cmd-stop", tr.CommandID)
	}
	if !tr.OccurredAt.Equal(*f.now) {
		t.Errorf("transition occurred_at = %v, want the accepting instant %v", tr.OccurredAt, *f.now)
	}

	var decided domain.AttentionItem
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		decided, err = tx.GetAttentionItem(ctx, health.ID)
		return err
	}); err != nil {
		t.Fatalf("GetAttentionItem: %v", err)
	}
	if decided.Status != domain.StatusResolved || decided.DecidedAt == nil {
		t.Errorf("decided item: status %q decided_at %v, want resolved and stamped",
			decided.Status, decided.DecidedAt)
	}

	open := openHealthItems(t, f.store)
	if len(open) != 1 {
		t.Fatalf("open system_health items = %d, want exactly the stopped notice", len(open))
	}
	notice := open[0]
	if !notice.Offers(domain.ActionResumeUnattended) || !notice.Offers(domain.ActionAcknowledge) {
		t.Errorf("stopped notice offers %v, want resume_unattended and acknowledge", notice.RequestedDecision)
	}
	if notice.Subject.Type != domain.SubjectSystem {
		t.Errorf("stopped notice subject = %+v, want system scope", notice.Subject)
	}
	if notice.ProjectID != health.ProjectID {
		t.Errorf("stopped notice project = %q, want inherited %q", notice.ProjectID, health.ProjectID)
	}

	// A retried command converges on the original result: no new revision,
	// no second transition or notice.
	replay, err := f.service.Submit(ctx, commandOn(health, "cmd-stop", domain.ActionStopUnattended))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Revision != result.Revision {
		t.Errorf("replay revision = %d, want the original %d", replay.Revision, result.Revision)
	}
	if got := openHealthItems(t, f.store); len(got) != 1 {
		t.Errorf("open items after replay = %d, want 1", len(got))
	}
}

// TestSubmitSecondStopConverges: a second stop decided on another open health
// item records its own transition (a real operator decision) but converges on
// the existing resume-offering notice instead of accumulating a duplicate
// that would still block after a resume.
func TestSubmitSecondStopConverges(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	first := seedHealthItem(t, f, "health-1")
	second := seedHealthItem(t, f, "health-2")

	if _, err := f.service.Submit(ctx, commandOn(first, "cmd-stop-1", domain.ActionStopUnattended)); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if _, err := f.service.Submit(ctx, commandOn(second, "cmd-stop-2", domain.ActionStopUnattended)); err != nil {
		t.Fatalf("second stop: %v", err)
	}

	open := openHealthItems(t, f.store)
	if len(open) != 1 || !open[0].Offers(domain.ActionResumeUnattended) {
		t.Fatalf("open items after two stops = %+v, want exactly one resume-offering notice", open)
	}
	if tr, found := latestTransition(t, f.store); !found || tr.State != domain.UnattendedStopped {
		t.Fatalf("latest transition = %+v (found %v), want stopped", tr, found)
	}
}

// TestSubmitResumeUnattended is the explicit operator resume: accepting
// resume_unattended on the stopped notice appends the resumed transition and
// resolves the notice atomically, leaving no open resume-offering item.
func TestSubmitResumeUnattended(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	health := seedHealthItem(t, f, "health-1")
	if _, err := f.service.Submit(ctx, commandOn(health, "cmd-stop", domain.ActionStopUnattended)); err != nil {
		t.Fatalf("stop: %v", err)
	}
	open := openHealthItems(t, f.store)
	if len(open) != 1 {
		t.Fatalf("open items after stop = %d, want the stopped notice", len(open))
	}
	notice := open[0]

	if _, err := f.service.Submit(ctx, commandOn(notice, "cmd-resume", domain.ActionResumeUnattended)); err != nil {
		t.Fatalf("Submit(resume_unattended): %v", err)
	}
	tr, found := latestTransition(t, f.store)
	if !found || tr.State != domain.UnattendedResumed {
		t.Fatalf("latest transition = %+v (found %v), want resumed", tr, found)
	}
	if tr.CommandID == nil || *tr.CommandID != "cmd-resume" {
		t.Errorf("transition command binding = %v, want cmd-resume", tr.CommandID)
	}
	if got := openHealthItems(t, f.store); len(got) != 0 {
		t.Errorf("open system_health items after resume = %+v, want none", got)
	}

	// The stop/resume cycle can repeat: a later stop raises a fresh notice
	// under its own command-derived identity (statuses are terminal, so the
	// resolved notice's id cannot be reused).
	remaining := seedHealthItem(t, f, "health-2")
	if _, err := f.service.Submit(ctx, commandOn(remaining, "cmd-stop-2", domain.ActionStopUnattended)); err != nil {
		t.Fatalf("second cycle stop: %v", err)
	}
	open = openHealthItems(t, f.store)
	if len(open) != 1 || !open[0].Offers(domain.ActionResumeUnattended) {
		t.Fatalf("open items after second stop = %+v, want one fresh notice", open)
	}
	if open[0].ID == notice.ID {
		t.Error("second cycle reused the resolved notice's identity")
	}
}

// TestSubmitStopRejectsRevokedDevice: the operating-state transactions ride
// the same accepting transaction as every command, so the active-device gate
// (§5.14 test 15) covers them; this pins it for the new outcome kinds.
func TestSubmitStopRejectsRevokedDevice(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	health := seedHealthItem(t, f, "health-1")

	revoked := f.device
	revoked.Status = domain.DeviceRevoked
	revokedAt := *f.now
	revoked.RevokedAt = &revokedAt
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, revoked)
	}); err != nil {
		t.Fatalf("revoke device: %v", err)
	}

	if _, err := f.service.Submit(ctx, commandOn(health, "cmd-stop", domain.ActionStopUnattended)); !errors.Is(err, signet.ErrDeviceNotActive) {
		t.Fatalf("stop from revoked device = %v, want ErrDeviceNotActive", err)
	}
	if _, found := latestTransition(t, f.store); found {
		t.Error("a refused stop recorded a transition")
	}
}
