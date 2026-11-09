package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestAttentionItemViewValidate covers the view record's shape: an item id, a
// content-address digest, and a real UTC instant (issue #2930).
func TestAttentionItemViewValidate(t *testing.T) {
	valid := domain.AttentionItemView{
		ItemID:                "item-1",
		DecisionSurfaceDigest: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		FirstOpenedAt:         time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid view: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*domain.AttentionItemView)
		want   error
	}{
		{"empty item id", func(v *domain.AttentionItemView) { v.ItemID = "" }, domain.ErrEmptyID},
		{"bare digest", func(v *domain.AttentionItemView) { v.DecisionSurfaceDigest = "digest-1" }, domain.ErrInvalidDigest},
		{"zero time", func(v *domain.AttentionItemView) { v.FirstOpenedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC time", func(v *domain.AttentionItemView) {
			v.FirstOpenedAt = v.FirstOpenedAt.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := valid
			tc.mutate(&view)
			if err := view.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}
