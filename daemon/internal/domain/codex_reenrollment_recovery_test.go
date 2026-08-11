package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validCodexReenrollmentBinding() domain.CodexReenrollmentRecoveryBinding {
	return domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID: "codex-primary", LeaseFence: 4,
		AuthStoreDigest:      "sha256:replacement-store",
		AccessTokenExpiresAt: time.Date(2026, 8, 12, 3, 4, 5, 0, time.UTC),
	}
}

func TestCodexReenrollmentRecoveryBindingValidate(t *testing.T) {
	valid := validCodexReenrollmentBinding()
	for name, mutate := range map[string]func(*domain.CodexReenrollmentRecoveryBinding){
		"empty identity": func(b *domain.CodexReenrollmentRecoveryBinding) { b.AuthIdentityID = "" },
		"zero fence":     func(b *domain.CodexReenrollmentRecoveryBinding) { b.LeaseFence = 0 },
		"empty digest":   func(b *domain.CodexReenrollmentRecoveryBinding) { b.AuthStoreDigest = "" },
		"zero expiry":    func(b *domain.CodexReenrollmentRecoveryBinding) { b.AccessTokenExpiresAt = time.Time{} },
		"non-UTC expiry": func(b *domain.CodexReenrollmentRecoveryBinding) {
			b.AccessTokenExpiresAt = b.AccessTokenExpiresAt.In(time.FixedZone("CST", -6*60*60))
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := valid
			mutate(&got)
			if err := got.Validate(); err == nil {
				t.Fatal("Validate succeeded")
			}
		})
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid binding: %v", err)
	}
}

func TestCodexReenrollmentBindingIsSystemHealthOnly(t *testing.T) {
	binding := validCodexReenrollmentBinding()
	in := validItemInput(domain.AttentionSpecApproval)
	in.CodexReenrollmentRecoveryBinding = &binding
	if _, err := domain.NewAttentionItem(in, nil); !errors.Is(err, domain.ErrCodexReenrollmentBindingOutsideItem) {
		t.Fatalf("wrong type = %v, want %v", err, domain.ErrCodexReenrollmentBindingOutsideItem)
	}
	in = validItemInput(domain.AttentionSystemHealth)
	in.CodexReenrollmentRecoveryBinding = &binding
	if _, err := domain.NewAttentionItem(in, nil); !errors.Is(err, domain.ErrCodexReenrollmentBindingMismatch) {
		t.Fatalf("binding without action = %v, want mismatch", err)
	}
	in = validItemInput(domain.AttentionSystemHealth)
	in.RequestedDecision = append(in.RequestedDecision, domain.ActionResolveReenrollment)
	if _, err := domain.NewAttentionItem(in, nil); !errors.Is(err, domain.ErrCodexReenrollmentBindingMissing) {
		t.Fatalf("action without binding = %v, want missing", err)
	}
	in = validItemInput(domain.AttentionSpecApproval)
	in.RequestedDecision = append(in.RequestedDecision, domain.ActionResolveReenrollment)
	if _, err := domain.NewAttentionItem(in, nil); err != nil {
		t.Fatalf("per-type action policy leaked into domain validation: %v", err)
	}
	in = validItemInput(domain.AttentionSystemHealth)
	in.RequestedDecision = append(in.RequestedDecision, domain.ActionResolveReenrollment)
	in.CodexReenrollmentRecoveryBinding = &binding
	item, err := domain.NewAttentionItem(in, nil)
	if err != nil {
		t.Fatalf("system health binding: %v", err)
	}
	binding.LeaseFence++
	if item.CodexReenrollmentRecoveryBinding.LeaseFence != 4 {
		t.Fatal("constructed item aliases caller binding")
	}
}

func TestCodexReenrollmentBindingTransitionIsOneWay(t *testing.T) {
	old := mustItem(t, validItemInput(domain.AttentionSystemHealth))
	projectedInput := validItemInput(domain.AttentionSystemHealth)
	projectedInput.ItemVersion = 2
	projectedInput.RequestedDecision = append(projectedInput.RequestedDecision, domain.ActionResolveReenrollment)
	binding := validCodexReenrollmentBinding()
	projectedInput.CodexReenrollmentRecoveryBinding = &binding
	projected := mustItem(t, projectedInput)
	if err := domain.ValidateAttentionItemTransition(old, projected); err != nil {
		t.Fatalf("nil -> verified binding: %v", err)
	}

	removed := projected
	removed.ItemVersion++
	removed.CodexReenrollmentRecoveryBinding = nil
	if err := domain.ValidateAttentionItemTransition(projected, removed); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("removal = %v, want immutable", err)
	}
	replaced := projected
	replaced.ItemVersion++
	replacement := binding
	replacement.AuthStoreDigest = "sha256:other"
	replaced.CodexReenrollmentRecoveryBinding = &replacement
	if err := domain.ValidateAttentionItemTransition(projected, replaced); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("replacement = %v, want immutable", err)
	}
	closed := old
	closed.Status = domain.StatusResolved
	closed.ItemVersion++
	closed.CodexReenrollmentRecoveryBinding = &binding
	if err := domain.ValidateAttentionItemTransition(old, closed); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("attach while concluding = %v, want immutable", err)
	}
}

func TestCodexReenrollmentRecoveryTransitionValidate(t *testing.T) {
	binding := validCodexReenrollmentBinding()
	commandID := "command-resolve-reenrollment"
	transition := domain.CodexReenrollmentRecoveryTransition{
		AuthIdentityID: binding.AuthIdentityID, LeaseFence: binding.LeaseFence,
		AuthStoreDigest:      binding.AuthStoreDigest,
		AccessTokenExpiresAt: binding.AccessTokenExpiresAt,
		CommandID:            &commandID, Reason: "verified recovery accepted",
		OccurredAt: time.Date(2026, 8, 11, 3, 4, 5, 0, time.UTC),
	}
	if err := transition.Validate(); err != nil {
		t.Fatalf("valid transition: %v", err)
	}
	if transition.Binding() != binding || transition.AuthorizingAction() != domain.ActionResolveReenrollment {
		t.Fatalf("transition binding/action = %+v/%q", transition.Binding(), transition.AuthorizingAction())
	}
	transition.CommandID = nil
	if err := transition.Validate(); !errors.Is(err, domain.ErrTransitionUnbacked) {
		t.Fatalf("unbacked transition = %v", err)
	}
}
