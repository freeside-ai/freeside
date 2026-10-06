package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestExternalFindingDispositionValidate: the record takes the review
// disposition's rules, so each outcome carries exactly the binding that
// decided it, and the other binding is null, never empty.
func TestExternalFindingDispositionValidate(t *testing.T) {
	t.Parallel()
	adjudicationDigest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	remediation, empty, emptyDigest := domain.InvocationID("review-run-1-3"), domain.InvocationID(""), domain.Digest("")
	fixed := domain.ExternalFindingDisposition{
		FindingID: "finding-1", RunID: "run-1", Round: 2,
		Disposition: domain.ReviewDispositionFixed, Reason: "remediated in round 3",
		RemediationInvocationID: &remediation,
		CreatedAt:               time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
	}
	decided := fixed
	decided.RemediationInvocationID, decided.AdjudicationDigest = nil, &adjudicationDigest
	for _, disposition := range domain.AllReviewDispositions {
		record := decided
		if disposition == domain.ReviewDispositionFixed {
			record = fixed
		}
		record.Disposition = disposition
		if err := record.Validate(); err != nil {
			t.Fatalf("valid %s disposition rejected: %v", disposition, err)
		}
	}
	for name, tc := range map[string]struct {
		mutate func(*domain.ExternalFindingDisposition)
		want   error
	}{
		"finding":     {func(d *domain.ExternalFindingDisposition) { d.FindingID = "" }, domain.ErrEmptyID},
		"run":         {func(d *domain.ExternalFindingDisposition) { d.RunID = "" }, domain.ErrEmptyID},
		"round":       {func(d *domain.ExternalFindingDisposition) { d.Round = 0 }, domain.ErrNonPositive},
		"disposition": {func(d *domain.ExternalFindingDisposition) { d.Disposition = "ignored" }, domain.ErrInvalidReviewDisposition},
		"reason":      {func(d *domain.ExternalFindingDisposition) { d.Reason = "" }, domain.ErrEmptyField},
		"fixed without remediation": {
			func(d *domain.ExternalFindingDisposition) { d.RemediationInvocationID = nil }, domain.ErrEmptyField,
		},
		"fixed with an empty remediation": {
			func(d *domain.ExternalFindingDisposition) { d.RemediationInvocationID = &empty }, domain.ErrEmptyField,
		},
		"fixed with adjudication": {
			func(d *domain.ExternalFindingDisposition) { d.AdjudicationDigest = &adjudicationDigest },
			domain.ErrInvalidDispositionAdjudication,
		},
		"fixed with an empty adjudication": {
			func(d *domain.ExternalFindingDisposition) { d.AdjudicationDigest = &emptyDigest }, domain.ErrEmptyField,
		},
		"declined without adjudication": {
			func(d *domain.ExternalFindingDisposition) {
				d.Disposition, d.RemediationInvocationID = domain.ReviewDispositionDeclined, nil
			}, domain.ErrEmptyField,
		},
		"deferred with remediation": {
			func(d *domain.ExternalFindingDisposition) {
				d.Disposition, d.AdjudicationDigest = domain.ReviewDispositionDeferred, &adjudicationDigest
			}, domain.ErrInvalidHeadBinding,
		},
		"time":    {func(d *domain.ExternalFindingDisposition) { d.CreatedAt = time.Time{} }, domain.ErrMissingTimestamp},
		"not utc": {func(d *domain.ExternalFindingDisposition) { d.CreatedAt = d.CreatedAt.In(time.FixedZone("x", 3600)) }, domain.ErrTimestampNotUTC},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			record := fixed
			tc.mutate(&record)
			if err := record.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}
