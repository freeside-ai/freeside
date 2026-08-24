package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const manifestDigest = domain.Digest(
	"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

func enrollment() domain.ClientEnrollment {
	return domain.ClientEnrollment{
		ID: "enroll-1", AuthIdentityID: "auth-1",
		HarnessClient: domain.HarnessClientCodexCLI, Route: "openai_chatgpt_codex",
		AuthMethod:      domain.AuthMethodOAuth,
		CredentialMode:  domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: "acct-1",
	}
}

func generation() domain.EnrollmentGeneration {
	expiry := time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC)
	return domain.EnrollmentGeneration{
		EnrollmentID: "enroll-1", Ordinal: 1,
		AuthStoreVolume: "codex-store", StoreManifestDigest: manifestDigest,
		LeaseFence: 1, AccountBinding: "acct-1",
		TokenExpiry: &expiry,
		RecordedAt:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestClientEnrollmentValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.ClientEnrollment)
		wantErr error
	}{
		{"valid", func(*domain.ClientEnrollment) {}, nil},
		{"no id", func(e *domain.ClientEnrollment) { e.ID = "" }, domain.ErrEmptyID},
		{"no identity", func(e *domain.ClientEnrollment) { e.AuthIdentityID = "" }, domain.ErrEmptyID},
		{"unknown harness client", func(e *domain.ClientEnrollment) {
			e.HarnessClient = "netscape"
		}, domain.ErrInvalidHarnessClientKind},
		{"zero harness client", func(e *domain.ClientEnrollment) { e.HarnessClient = "" }, domain.ErrInvalidHarnessClientKind},
		{"no route", func(e *domain.ClientEnrollment) { e.Route = "" }, domain.ErrEmptyField},
		{"unknown auth method", func(e *domain.ClientEnrollment) { e.AuthMethod = "password" }, domain.ErrInvalidAuthMethod},
		{"zero auth method", func(e *domain.ClientEnrollment) { e.AuthMethod = "" }, domain.ErrInvalidAuthMethod},
		{"unknown credential mode", func(e *domain.ClientEnrollment) { e.CredentialMode = "loose" }, domain.ErrInvalidCredentialMode},
		{"unknown refresh strategy", func(e *domain.ClientEnrollment) { e.RefreshStrategy = "magic" }, domain.ErrInvalidRefreshStrategy},
		{"no account binding", func(e *domain.ClientEnrollment) { e.AccountBinding = "" }, domain.ErrEmptyField},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := enrollment()
			tc.mutate(&e)
			if err := e.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestEnrollmentGenerationValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.EnrollmentGeneration)
		wantErr error
	}{
		{"valid", func(*domain.EnrollmentGeneration) {}, nil},
		{"unpersisted ordinal", func(g *domain.EnrollmentGeneration) { g.Ordinal = 0 }, nil},
		{"no enrollment", func(g *domain.EnrollmentGeneration) { g.EnrollmentID = "" }, domain.ErrEmptyID},
		{"negative ordinal", func(g *domain.EnrollmentGeneration) { g.Ordinal = -1 }, domain.ErrNonPositive},
		{"no volume", func(g *domain.EnrollmentGeneration) { g.AuthStoreVolume = "" }, domain.ErrEmptyField},
		{"malformed manifest digest", func(g *domain.EnrollmentGeneration) {
			g.StoreManifestDigest = "manifest"
		}, domain.ErrInvalidDigest},
		{"zero fence", func(g *domain.EnrollmentGeneration) { g.LeaseFence = 0 }, domain.ErrNonPositive},
		{"no account binding", func(g *domain.EnrollmentGeneration) { g.AccountBinding = "" }, domain.ErrEmptyField},
		{"zero expiry", func(g *domain.EnrollmentGeneration) {
			g.TokenExpiry = &time.Time{}
		}, domain.ErrMissingTimestamp},
		{"non-UTC expiry", func(g *domain.EnrollmentGeneration) {
			local := g.TokenExpiry.In(time.FixedZone("PDT", -7*3600))
			g.TokenExpiry = &local
		}, domain.ErrTimestampNotUTC},
		{"no recorded_at", func(g *domain.EnrollmentGeneration) { g.RecordedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC recorded_at", func(g *domain.EnrollmentGeneration) {
			g.RecordedAt = g.RecordedAt.In(time.FixedZone("PDT", -7*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := generation()
			tc.mutate(&g)
			if err := g.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateEnrollmentIdentityBinding pins the §5.4 account rule at the
// enrollment boundary: an unbound identity cannot hold enrollments, and a
// credential can never re-home onto a different account.
func TestValidateEnrollmentIdentityBinding(t *testing.T) {
	identity := authIdentity()
	identity.AccountBinding = "acct-1"
	cases := []struct {
		name     string
		identity func() domain.AuthIdentity
		mutate   func(*domain.ClientEnrollment)
		wantErr  error
	}{
		{"bound", func() domain.AuthIdentity { return identity }, func(*domain.ClientEnrollment) {}, nil},
		{
			"foreign identity",
			func() domain.AuthIdentity { return identity },
			func(e *domain.ClientEnrollment) { e.AuthIdentityID = "auth-2" },
			domain.ErrParentKeyMismatch,
		},
		{
			"identity without account binding",
			func() domain.AuthIdentity { unbound := identity; unbound.AccountBinding = ""; return unbound },
			func(*domain.ClientEnrollment) {},
			domain.ErrAccountBindingMismatch,
		},
		{
			"credential for another account",
			func() domain.AuthIdentity { return identity },
			func(e *domain.ClientEnrollment) { e.AccountBinding = "acct-2" },
			domain.ErrAccountBindingMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := enrollment()
			tc.mutate(&e)
			if err := domain.ValidateEnrollmentIdentityBinding(tc.identity(), e); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateEnrollmentIdentityBinding = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateEnrollmentGenerationBinding(t *testing.T) {
	cases := []struct {
		name       string
		enrollment func() domain.ClientEnrollment
		mutate     func(*domain.EnrollmentGeneration)
		wantErr    error
	}{
		{"bound", enrollment, func(*domain.EnrollmentGeneration) {}, nil},
		{
			"foreign enrollment",
			enrollment,
			func(g *domain.EnrollmentGeneration) { g.EnrollmentID = "enroll-2" },
			domain.ErrParentKeyMismatch,
		},
		{
			"account differs from the enrollment's",
			enrollment,
			func(g *domain.EnrollmentGeneration) { g.AccountBinding = "acct-2" },
			domain.ErrAccountBindingMismatch,
		},
		{
			// The OAuth method observes expiry; omitting it would defeat
			// admission step 4's margin check.
			"observable expiry omitted",
			enrollment,
			func(g *domain.EnrollmentGeneration) { g.TokenExpiry = nil },
			domain.ErrGenerationExpiryInconsistent,
		},
		{
			// The setup token observes no expiry; recording one would be an
			// invented fact.
			"unobservable expiry recorded",
			func() domain.ClientEnrollment {
				e := enrollment()
				e.AuthMethod = domain.AuthMethodSetupToken
				return e
			},
			func(*domain.EnrollmentGeneration) {},
			domain.ErrGenerationExpiryInconsistent,
		},
		{
			"setup token without expiry",
			func() domain.ClientEnrollment {
				e := enrollment()
				e.AuthMethod = domain.AuthMethodSetupToken
				return e
			},
			func(g *domain.EnrollmentGeneration) { g.TokenExpiry = nil },
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := generation()
			tc.mutate(&g)
			if err := domain.ValidateEnrollmentGenerationBinding(tc.enrollment(), g); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateEnrollmentGenerationBinding = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateGenerationExpiryMargin is the §5.4 admission step 4 fixture:
// where expiry is observable it must cover the attempt deadline plus margin,
// and a shorter window is a typed refusal.
func TestValidateGenerationExpiryMargin(t *testing.T) {
	e := enrollment()
	g := generation()
	expiry := *g.TokenExpiry
	margin := 15 * time.Minute
	cases := []struct {
		name     string
		deadline time.Time
		wantErr  error
	}{
		{"covered with margin to spare", expiry.Add(-2 * margin), nil},
		{"covered exactly", expiry.Add(-margin), nil},
		{"margin cut short", expiry.Add(-margin).Add(time.Second), domain.ErrGenerationExpiryInsufficient},
		{"already expired", expiry.Add(time.Hour), domain.ErrGenerationExpiryInsufficient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.ValidateGenerationExpiryMargin(e, g, tc.deadline, margin)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateGenerationExpiryMargin = %v, want %v", err, tc.wantErr)
			}
		})
	}
	t.Run("unobservable expiry has nothing to check", func(t *testing.T) {
		setup := enrollment()
		setup.AuthMethod = domain.AuthMethodSetupToken
		bare := generation()
		bare.TokenExpiry = nil
		if err := domain.ValidateGenerationExpiryMargin(setup, bare, expiry.Add(time.Hour), margin); err != nil {
			t.Fatalf("ValidateGenerationExpiryMargin = %v, want nil", err)
		}
	})
	t.Run("zero deadline refused", func(t *testing.T) {
		err := domain.ValidateGenerationExpiryMargin(e, g, time.Time{}, margin)
		if !errors.Is(err, domain.ErrMissingTimestamp) {
			t.Fatalf("ValidateGenerationExpiryMargin = %v, want %v", err, domain.ErrMissingTimestamp)
		}
	})
	t.Run("negative margin refused", func(t *testing.T) {
		err := domain.ValidateGenerationExpiryMargin(e, g, expiry.Add(-time.Hour), -time.Second)
		if !errors.Is(err, domain.ErrNonPositive) {
			t.Fatalf("ValidateGenerationExpiryMargin = %v, want %v", err, domain.ErrNonPositive)
		}
	})
}

func TestAuthMethodExpiryObservable(t *testing.T) {
	cases := map[domain.AuthMethod]bool{
		domain.AuthMethodSetupToken: false,
		domain.AuthMethodOAuth:      true,
	}
	for _, method := range domain.AllAuthMethods {
		want, ok := cases[method]
		if !ok {
			t.Fatalf("auth method %q has no expiry expectation in this test", method)
		}
		if got := method.ExpiryObservable(); got != want {
			t.Fatalf("%q.ExpiryObservable() = %v, want %v", method, got, want)
		}
	}
}
