package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestCodexReenrollmentEnumRegistration(t *testing.T) {
	t.Parallel()
	outcomes := []CodexReenrollmentOutcome{
		CodexReenrollmentFailed,
		CodexReenrollmentVerified,
	}
	if !slices.Equal(AllCodexReenrollmentOutcomes, outcomes) {
		t.Fatalf("outcomes = %v, want %v", AllCodexReenrollmentOutcomes, outcomes)
	}
	for _, outcome := range AllCodexReenrollmentOutcomes {
		if !outcome.valid() {
			t.Errorf("registered outcome %q is invalid", outcome)
		}
	}
	if CodexReenrollmentOutcome("").valid() {
		t.Error("empty outcome is valid")
	}

	failureClasses := []CodexReenrollmentFailureClass{
		CodexReenrollmentAuthStoreReplacementFailed,
		CodexReenrollmentVerificationFailed,
		CodexReenrollmentLeaseLost,
	}
	if !slices.Equal(AllCodexReenrollmentFailureClasses, failureClasses) {
		t.Fatalf(
			"failure classes = %v, want %v",
			AllCodexReenrollmentFailureClasses,
			failureClasses,
		)
	}
	for _, failureClass := range AllCodexReenrollmentFailureClasses {
		if !failureClass.valid() {
			t.Errorf("registered failure class %q is invalid", failureClass)
		}
	}
	if CodexReenrollmentFailureClass("").valid() {
		t.Error("empty failure class is valid")
	}
}

func TestCodexReenrollmentMarkerBindingMatchesIdentity(t *testing.T) {
	t.Parallel()
	binding := domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID: "codex-other", LeaseFence: 1,
		AuthStoreDigest:      "sha256:replacement",
		AccessTokenExpiresAt: time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC),
	}
	_, err := NewCodexReenrollmentMarker(
		"codex-primary", 1,
		"project-1", 1, domain.StatusOpen, &binding,
	)
	if !errors.Is(err, domain.ErrCodexReenrollmentBindingMismatch) {
		t.Fatalf("cross-identity marker binding = %v, want mismatch", err)
	}
}

func TestCodexReenrollmentMarkerRequiresPositiveOccurrence(t *testing.T) {
	t.Parallel()
	for _, occurrence := range []int{-1, 0} {
		_, err := NewCodexReenrollmentMarker(
			"codex-primary", occurrence, "project-1", 1, domain.StatusOpen, nil,
		)
		if !errors.Is(err, domain.ErrCodexReenrollmentMarkerMismatch) {
			t.Fatalf("occurrence %d = %v, want marker mismatch", occurrence, err)
		}
	}
}

func TestCodexReenrollmentJournalRejectsCompletionBeforeOpening(t *testing.T) {
	t.Parallel()
	openedAt := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	class := CodexReenrollmentLeaseLost
	rec := CodexReenrollmentJournal{
		AuthIdentityID: "codex-primary",
		LeaseFence:     1,
		MarkerItemID:   "system-health-codex-auth-internal-1",
		Holder:         "enroll-1",
		OpenedAt:       openedAt,
		Terminal: &CodexReenrollmentTerminal{
			Outcome:      CodexReenrollmentFailed,
			FailureClass: &class,
			CompletedAt:  openedAt.Add(-time.Nanosecond),
		},
	}
	if err := rec.Validate(); err == nil {
		t.Fatal("journal accepted a terminal that predates its opening")
	}
}

func TestCodexReenrollmentRecoveryBindingValidatesCallerConstructedJournal(t *testing.T) {
	t.Parallel()
	completedAt := time.Date(2026, 8, 11, 1, 2, 4, 0, time.UTC)
	digest := domain.Digest("sha256:replacement")
	expiresAt := completedAt.Add(time.Hour)
	for name, journal := range map[string]CodexReenrollmentJournal{
		"nil verified evidence": {
			AuthIdentityID: "codex-primary", LeaseFence: 1,
			MarkerItemID: "marker-1", Holder: "enroll-1",
			OpenedAt: completedAt.Add(-time.Second),
			Terminal: &CodexReenrollmentTerminal{
				Outcome: CodexReenrollmentVerified, CompletedAt: completedAt,
			},
		},
		"invalid journal coordinates": {
			LeaseFence: 1, MarkerItemID: "marker-1", Holder: "enroll-1",
			OpenedAt: completedAt.Add(-time.Second),
			Terminal: &CodexReenrollmentTerminal{
				Outcome: CodexReenrollmentVerified, AuthStoreDigest: &digest,
				AccessTokenExpiresAt: &expiresAt, CompletedAt: completedAt,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := journal.RecoveryBinding(); err == nil {
				t.Fatal("invalid caller-constructed journal yielded a recovery binding")
			}
		})
	}
}

func TestCodexReenrollmentRecoveryCarrierValidatesCallerConstructedTransition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/freeside.db", Options{})
	at := time.Date(2026, 8, 11, 1, 2, 4, 0, time.UTC)
	commandID := "command-1"
	valid := domain.CodexReenrollmentRecoveryTransition{
		AuthIdentityID: "codex-primary", LeaseFence: 1,
		AuthStoreDigest: "sha256:replacement", AccessTokenExpiresAt: at.Add(time.Hour),
		CommandID: &commandID, Reason: "verified re-enrollment", OccurredAt: at,
	}
	for name, transition := range map[string]domain.CodexReenrollmentRecoveryTransition{
		"nil command": func() domain.CodexReenrollmentRecoveryTransition {
			got := valid
			got.CommandID = nil
			return got
		}(),
		"invalid binding coordinates": func() domain.CodexReenrollmentRecoveryTransition {
			got := valid
			got.AuthIdentityID = ""
			return got
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.CodexReenrollmentRecoveryCarrier(ctx, transition)
				return err
			}); err == nil {
				t.Fatal("invalid caller-constructed transition yielded a recovery carrier")
			}
		})
	}
}

func TestCodexReenrollmentRecoveryCarrierRequiresExactDecisionInstant(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 11, 1, 2, 4, 0, time.UTC)
	binding := domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID: "codex-primary", LeaseFence: 1,
		AuthStoreDigest: "sha256:replacement", AccessTokenExpiresAt: at.Add(time.Hour),
	}
	item, err := NewCodexReenrollmentMarker(
		binding.AuthIdentityID, 1, "project-1", 2, domain.StatusResolved, &binding,
	)
	if err != nil {
		t.Fatal(err)
	}
	item.DecidedAt = &at
	commandID := "command-1"
	transition := domain.CodexReenrollmentRecoveryTransition{
		AuthIdentityID: binding.AuthIdentityID, LeaseFence: binding.LeaseFence,
		AuthStoreDigest: binding.AuthStoreDigest, AccessTokenExpiresAt: binding.AccessTokenExpiresAt,
		CommandID: &commandID, Reason: "verified re-enrollment", OccurredAt: at,
	}
	if err := validateCodexReenrollmentRecoveryCarrier(item, transition); err != nil {
		t.Fatalf("exact decision instant: %v", err)
	}
	for name, mutate := range map[string]func(*domain.AttentionItem, *domain.CodexReenrollmentRecoveryTransition){
		"missing decision stamp": func(item *domain.AttentionItem, _ *domain.CodexReenrollmentRecoveryTransition) {
			item.DecidedAt = nil
		},
		"non-resolved carrier": func(item *domain.AttentionItem, _ *domain.CodexReenrollmentRecoveryTransition) {
			item.Status = domain.StatusOpen
		},
		"transition before decision": func(_ *domain.AttentionItem, transition *domain.CodexReenrollmentRecoveryTransition) {
			transition.OccurredAt = transition.OccurredAt.Add(-time.Nanosecond)
		},
		"transition after decision": func(_ *domain.AttentionItem, transition *domain.CodexReenrollmentRecoveryTransition) {
			transition.OccurredAt = transition.OccurredAt.Add(time.Nanosecond)
		},
	} {
		t.Run(name, func(t *testing.T) {
			gotItem := item
			gotTransition := transition
			mutate(&gotItem, &gotTransition)
			if err := validateCodexReenrollmentRecoveryCarrier(gotItem, gotTransition); !errors.Is(err, domain.ErrCodexReenrollmentMarkerMismatch) {
				t.Fatalf("carrier validation = %v, want marker mismatch", err)
			}
		})
	}
}

func TestCodexReenrollmentVerifiedRequiresUnexpiredEvidence(t *testing.T) {
	t.Parallel()
	completedAt := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	digest := domain.Digest("sha256:replacement")
	for _, expiresAt := range []time.Time{
		completedAt.Add(-time.Nanosecond),
		completedAt,
	} {
		terminal := CodexReenrollmentTerminal{
			Outcome:              CodexReenrollmentVerified,
			AuthStoreDigest:      &digest,
			AccessTokenExpiresAt: &expiresAt,
			CompletedAt:          completedAt,
		}
		if err := terminal.Validate(); err == nil {
			t.Errorf("verified terminal accepted expiry %s at completion %s", expiresAt, completedAt)
		}
	}
	expiresAt := completedAt.Add(time.Nanosecond)
	terminal := CodexReenrollmentTerminal{
		Outcome:              CodexReenrollmentVerified,
		AuthStoreDigest:      &digest,
		AccessTokenExpiresAt: &expiresAt,
		CompletedAt:          completedAt,
	}
	if err := terminal.Validate(); err != nil {
		t.Fatalf("verified terminal rejected live evidence: %v", err)
	}
}

func TestCodexReenrollmentReconstructionRejectsUnknownAndTrailingBody(t *testing.T) {
	t.Parallel()
	for _, mutate := range []struct {
		name string
		body func(string) string
	}{
		{"unknown field", func(body string) string {
			return strings.TrimSuffix(body, "}") + `,"unknown":true}`
		}},
		{"trailing data", func(body string) string { return body + `{}` }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			ctx := context.Background()
			st := openTemplateStoreAt(t, t.TempDir()+"/freeside.db", Options{})
			at := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
			identity := domain.AuthIdentity{
				ID: "codex-primary", Provider: domain.AuthProviderOpenAI, AuthStoreMutationLease: true,
				Interim: domain.InterimClientFacts{AuthStoreVolume: "codex-auth", RefreshStrategy: domain.RefreshOnDemand},
			}
			markerID := domain.ItemID(CodexReenrollmentMarkerPrefix(identity.ID) + "1")
			var rec CodexReenrollmentJournal
			if err := st.Write(ctx, func(tx *WriteTx) error {
				return tx.RecordAuthIdentity(ctx, identity, at)
			}); err != nil {
				t.Fatal(err)
			}
			marker, err := NewCodexReenrollmentMarker(
				identity.ID, 1, "project-1", 1, domain.StatusOpen, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, marker) }); err != nil {
				t.Fatal(err)
			}
			if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
				var err error
				rec, _, err = tx.BeginCodexReenrollmentJournal(
					ctx, identity.ID, markerID, "enroll-1", at, at.Add(time.Minute))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var body string
			if err := st.db.QueryRowContext(ctx,
				`SELECT body FROM codex_reenrollment_operations WHERE auth_identity_id = ? AND lease_fence = ?`,
				identity.ID, rec.LeaseFence).Scan(&body); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx,
				`UPDATE codex_reenrollment_operations SET body = ? WHERE auth_identity_id = ? AND lease_fence = ?`,
				mutate.body(body), identity.ID, rec.LeaseFence); err != nil {
				t.Fatal(err)
			}
			if err := st.Read(ctx, func(tx *ReadTx) error {
				_, _, err := tx.LatestCodexReenrollmentJournal(ctx, identity.ID)
				return err
			}); err == nil {
				t.Fatal("malformed body reconstructed")
			}
		})
	}
}
