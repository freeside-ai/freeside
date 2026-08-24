package signet_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func seedVerifiedCodexReenrollment(
	t *testing.T, f fixture,
) domain.CodexReenrollmentRecoveryBinding {
	t.Helper()
	ctx := context.Background()
	identity := domain.AuthIdentity{
		ID: "codex-primary", Provider: "codex", AuthStoreMutationLease: true, MaxParallelExecutions: 1,
		Interim: domain.InterimClientFacts{AuthStoreVolume: "codex-auth", RefreshStrategy: domain.RefreshOnDemand},
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, identity, *f.now)
	}); err != nil {
		t.Fatalf("seed re-enrollment identity: %v", err)
	}
	marker, err := store.NewCodexReenrollmentMarker(
		identity.ID, 1, "proj-1", 1, domain.StatusOpen, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.PutItem(ctx, marker); err != nil {
		t.Fatal(err)
	}
	var rec store.CodexReenrollmentJournal
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		rec, _, err = tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, marker.ID, "enroll-1", *f.now, f.now.Add(time.Minute))
		if err != nil {
			return err
		}
		return tx.VerifyCodexReenrollment(
			ctx, identity.ID, rec.Holder, rec.LeaseFence,
			"sha256:replacement", f.now.Add(24*time.Hour), f.now.Add(time.Second))
	}); err != nil {
		t.Fatalf("seed verified re-enrollment: %v", err)
	}
	var binding domain.CodexReenrollmentRecoveryBinding
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		latest, found, err := tx.LatestCodexReenrollmentJournal(ctx, identity.ID)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("verified journal missing")
		}
		binding, err = latest.RecoveryBinding()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(time.Second)
	return binding
}

func seedCodexRecoveryItem(
	t *testing.T, f fixture, binding domain.CodexReenrollmentRecoveryBinding,
) domain.AttentionItem {
	t.Helper()
	item, err := store.NewCodexReenrollmentMarker(
		binding.AuthIdentityID, 1, "proj-1", 2, domain.StatusOpen, &binding,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.PutItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	return item
}

func codexCommandOn(item domain.AttentionItem, commandID string, action domain.Action) signet.ClientCommand {
	command := commandOn(item, commandID, action)
	command.ExpectedEntityVersion = 2
	return command
}

func TestSubmitCodexReenrollmentRecoveryIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	binding := seedVerifiedCodexReenrollment(t, f)
	item := seedCodexRecoveryItem(t, f, binding)
	command := codexCommandOn(item, "command-resolve-reenrollment", domain.ActionResolveReenrollment)
	before := f.revision(t)
	result, err := f.service.Submit(ctx, command)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := f.revision(t); got != before+1 {
		t.Fatalf("revision = %d, want %d", got, before+1)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		transition, found, err := tx.LatestCodexReenrollmentRecoveryTransition(ctx, binding.AuthIdentityID)
		if err != nil {
			return err
		}
		if !found || transition.Binding() != binding || transition.CommandID == nil ||
			*transition.CommandID != command.CommandID {
			t.Fatalf("transition = %+v found %v", transition, found)
		}
		decided, err := tx.GetAttentionItem(ctx, item.ID)
		if err == nil && (decided.Status != domain.StatusResolved || decided.DecidedAt == nil ||
			!decided.DecidedAt.Equal(transition.OccurredAt)) {
			t.Fatalf("decided item = %+v", decided)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	replay, err := f.service.Submit(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Revision != result.Revision || f.revision(t) != result.Revision {
		t.Fatal("command replay changed the committed result")
	}
}

func TestSubmitCodexReenrollmentRecoveryRejectsDecisionBeforeVerification(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	binding := seedVerifiedCodexReenrollment(t, f)
	*f.now = f.now.Add(-time.Nanosecond)
	item := seedCodexRecoveryItem(t, f, binding)
	command := codexCommandOn(item, "command-before-verification", domain.ActionResolveReenrollment)
	before := f.revision(t)
	if _, err := f.service.Submit(ctx, command); !errors.Is(err, store.ErrCodexReenrollmentNotVerified) {
		t.Fatalf("pre-verification recovery = %v, want not verified", err)
	}
	if got := f.revision(t); got != before {
		t.Fatalf("rejected recovery moved revision %d -> %d", before, got)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, found, err := tx.LatestCodexReenrollmentRecoveryTransition(ctx, binding.AuthIdentityID)
		if err != nil || found {
			t.Fatalf("pre-verification transition = %t, %v", found, err)
		}
		stored, err := tx.GetAttentionItem(ctx, item.ID)
		if err == nil && stored.Status != domain.StatusOpen {
			t.Fatalf("pre-verification recovery concluded item: %s", stored.Status)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitCodexReenrollmentRecoveryMismatchRollsBack(t *testing.T) {
	for name, mutate := range map[string]func(*domain.CodexReenrollmentRecoveryBinding){
		"identity": func(b *domain.CodexReenrollmentRecoveryBinding) { b.AuthIdentityID = "codex-other" },
		"fence":    func(b *domain.CodexReenrollmentRecoveryBinding) { b.LeaseFence++ },
		"digest":   func(b *domain.CodexReenrollmentRecoveryBinding) { b.AuthStoreDigest = "sha256:forged" },
		"expiry": func(b *domain.CodexReenrollmentRecoveryBinding) {
			b.AccessTokenExpiresAt = b.AccessTokenExpiresAt.Add(time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			binding := seedVerifiedCodexReenrollment(t, f)
			mutate(&binding)
			item := seedCodexRecoveryItem(t, f, binding)
			command := codexCommandOn(item, "command-forged-reenrollment", domain.ActionResolveReenrollment)
			before := f.revision(t)
			if _, err := f.service.Submit(ctx, command); err == nil {
				t.Fatal("mismatched recovery succeeded")
			}
			if got := f.revision(t); got != before {
				t.Fatalf("rejected command moved revision %d -> %d", before, got)
			}
			if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
				stored, err := tx.GetAttentionItem(ctx, item.ID)
				if err == nil && stored.Status != domain.StatusOpen {
					t.Fatalf("rejected recovery concluded item: %s", stored.Status)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSubmitCodexReenrollmentRecoveryRejectsDifferentSystemHealthCarrier(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	binding := seedVerifiedCodexReenrollment(t, f)
	posture := domain.HealthPostureAdvisory
	forged, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "unrelated-system-health-item", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason: "Unrelated system health condition",
		RequestedDecision: []domain.Action{
			domain.ActionAcknowledge, domain.ActionResolveReenrollment,
		},
		CodexReenrollmentRecoveryBinding: &binding,
		ItemVersion:                      1, InterruptionClass: domain.InterruptionExceptional,
		Posture: &posture, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.PutItem(ctx, forged); err != nil {
		t.Fatal(err)
	}
	before := f.revision(t)
	if _, err := f.service.Submit(ctx,
		commandOn(forged, "command-wrong-reenrollment-carrier", domain.ActionResolveReenrollment)); err == nil {
		t.Fatal("different system-health carrier borrowed verified operation")
	}
	if got := f.revision(t); got != before {
		t.Fatalf("rejected carrier moved revision %d -> %d", before, got)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetAttentionItem(ctx, forged.ID)
		if err == nil && stored.Status != domain.StatusOpen {
			t.Fatalf("wrong carrier concluded: %s", stored.Status)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitCodexReenrollmentRecoveryRejectsMutatedMarkerShape(t *testing.T) {
	for name, mutate := range map[string]func(*domain.AttentionItem){
		"reason": func(item *domain.AttentionItem) { item.Reason += " " },
		"actions": func(item *domain.AttentionItem) {
			item.RequestedDecision = []domain.Action{
				domain.ActionResolveReenrollment, domain.ActionAcknowledge,
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			binding := seedVerifiedCodexReenrollment(t, f)
			item, err := store.NewCodexReenrollmentMarker(
				binding.AuthIdentityID, 1, "proj-1", 2, domain.StatusOpen, &binding,
			)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&item)
			if err := f.service.PutItem(ctx, item); err != nil {
				t.Fatal(err)
			}
			before := f.revision(t)
			_, err = f.service.Submit(ctx,
				codexCommandOn(item, "command-mutated-reenrollment", domain.ActionResolveReenrollment))
			if !errors.Is(err, domain.ErrCodexReenrollmentMarkerMismatch) {
				t.Fatalf("mutated marker recovery = %v, want marker mismatch", err)
			}
			if got := f.revision(t); got != before {
				t.Fatalf("rejected marker moved revision %d -> %d", before, got)
			}
			if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
				_, found, err := tx.LatestCodexReenrollmentRecoveryTransition(ctx, binding.AuthIdentityID)
				if err != nil || found {
					t.Fatalf("rejected marker transition = %t, %v", found, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSubmitCodexReenrollmentRecoveryRejectsNonLatestVerifiedOperation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	oldBinding := seedVerifiedCodexReenrollment(t, f)
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		old, found, err := tx.LatestCodexReenrollmentJournal(ctx, oldBinding.AuthIdentityID)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("verified operation missing")
		}
		if err := tx.ReleaseAuthStoreMutationLease(
			ctx, old.AuthIdentityID, old.Holder, old.LeaseFence, f.now.Add(2*time.Second)); err != nil {
			return err
		}
		_, _, err = tx.BeginCodexReenrollmentJournal(
			ctx, old.AuthIdentityID, old.MarkerItemID, "enroll-newer", f.now.Add(3*time.Second), f.now.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	item := seedCodexRecoveryItem(t, f, oldBinding)
	command := codexCommandOn(item, "command-stale-reenrollment", domain.ActionResolveReenrollment)
	before := f.revision(t)
	if _, err := f.service.Submit(ctx, command); err == nil {
		t.Fatal("older verified operation outranked newer pending operation")
	}
	if got := f.revision(t); got != before {
		t.Fatalf("rejected stale recovery moved revision %d -> %d", before, got)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetAttentionItem(ctx, item.ID)
		if err == nil && stored.Status != domain.StatusOpen {
			t.Fatalf("stale recovery concluded item: %s", stored.Status)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAcknowledgeDoesNotResolveCodexReenrollment(t *testing.T) {
	f := newFixture(t)
	binding := seedVerifiedCodexReenrollment(t, f)
	item := seedCodexRecoveryItem(t, f, binding)
	if _, err := f.service.Submit(context.Background(),
		codexCommandOn(item, "command-acknowledge-reenrollment", domain.ActionAcknowledge)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		stored, err := tx.GetAttentionItem(context.Background(), item.ID)
		if err == nil && stored.Status != domain.StatusOpen {
			t.Fatalf("acknowledge concluded item: %s", stored.Status)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
