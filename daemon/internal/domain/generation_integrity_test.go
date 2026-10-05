package domain_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func integrityMark() domain.GenerationIntegrityMark {
	return domain.GenerationIntegrityMark{
		EnrollmentID: "enroll-1", Ordinal: 1,
		Finding:    domain.CredentialIntegrityTruncation,
		ObservedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestGenerationIntegrityMarkValidate(t *testing.T) {
	for _, finding := range domain.AllCredentialIntegrityFindings {
		mark := integrityMark()
		mark.Finding = finding
		if err := mark.Validate(); err != nil {
			t.Fatalf("mark with finding %q: %v", finding, err)
		}
	}
	cases := []struct {
		name    string
		mutate  func(*domain.GenerationIntegrityMark)
		wantErr error
	}{
		{"empty enrollment id", func(m *domain.GenerationIntegrityMark) { m.EnrollmentID = "" }, domain.ErrEmptyID},
		// A mark names a persisted generation, so the unpersisted zero an
		// EnrollmentGeneration admits is malformed here.
		{"zero ordinal", func(m *domain.GenerationIntegrityMark) { m.Ordinal = 0 }, domain.ErrNonPositive},
		{"negative ordinal", func(m *domain.GenerationIntegrityMark) { m.Ordinal = -1 }, domain.ErrNonPositive},
		{"empty finding", func(m *domain.GenerationIntegrityMark) { m.Finding = "" }, domain.ErrInvalidCredentialIntegrityFinding},
		{"unknown finding", func(m *domain.GenerationIntegrityMark) { m.Finding = "expired" }, domain.ErrInvalidCredentialIntegrityFinding},
		{"zero observed_at", func(m *domain.GenerationIntegrityMark) { m.ObservedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC observed_at", func(m *domain.GenerationIntegrityMark) {
			m.ObservedAt = m.ObservedAt.In(time.FixedZone("offset", 3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mark := integrityMark()
			tc.mutate(&mark)
			if err := mark.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestGenerationIntegrityMarkedError pins the refusal's two reads: the class
// through the sentinel, and the exact generation and finding through the
// concrete type, both surviving the wrapping an admission caller adds.
func TestGenerationIntegrityMarkedError(t *testing.T) {
	for _, finding := range domain.AllCredentialIntegrityFindings {
		t.Run(string(finding), func(t *testing.T) {
			mark := integrityMark()
			mark.Ordinal = 3
			mark.Finding = finding
			err := fmt.Errorf("role implementer: %w", &domain.GenerationIntegrityMarkedError{Mark: mark})
			if !errors.Is(err, domain.ErrGenerationIntegrityMarked) {
				t.Fatalf("errors.Is(%v, ErrGenerationIntegrityMarked) = false", err)
			}
			var marked *domain.GenerationIntegrityMarkedError
			if !errors.As(err, &marked) || marked.Mark != mark {
				t.Fatalf("errors.As = %+v, want mark %+v", marked, mark)
			}
			for _, want := range []string{"enroll-1", "generation 3", string(finding)} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %q", err, want)
				}
			}
			if errors.Is(err, domain.ErrGenerationExpiryInsufficient) {
				t.Fatal("the marked refusal matches an unrelated generation sentinel")
			}
		})
	}
}
