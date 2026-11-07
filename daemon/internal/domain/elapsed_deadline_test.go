package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// elapsedChecks and elapsedReview are the two publication deadlines elapsing
// in firing order: each records its nominal deadline's arming instant and
// elapses a minute after the deadline.
func elapsedChecks() domain.ElapsedDeadline {
	return domain.ElapsedDeadline{
		Kind:            domain.SchedulePRChecksDeadline,
		ArmedAt:         time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		DeadlineSeconds: 1800,
		ElapsedAt:       time.Date(2026, 1, 2, 3, 35, 5, 0, time.UTC),
	}
}

func elapsedReview() domain.ElapsedDeadline {
	return domain.ElapsedDeadline{
		Kind:            domain.ScheduleReviewWaitThreshold,
		ArmedAt:         time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		DeadlineSeconds: 86400,
		ElapsedAt:       time.Date(2026, 1, 3, 3, 5, 5, 0, time.UTC),
	}
}

func TestElapsedDeadlineValidation(t *testing.T) {
	ready := mustItem(t, validItemInput(domain.AttentionReadyForFinalReview))
	ready.ElapsedDeadlines = []domain.ElapsedDeadline{elapsedChecks(), elapsedReview()}
	if err := ready.Validate(); err != nil {
		t.Fatalf("ready item with both deadlines Validate() = %v, want nil", err)
	}

	onTime := elapsedChecks()
	onTime.ElapsedAt = onTime.ArmedAt.Add(30 * time.Minute)
	exact := ready
	exact.ElapsedDeadlines = []domain.ElapsedDeadline{onTime}
	if err := exact.Validate(); err != nil {
		t.Fatalf("elapse at the deadline itself Validate() = %v, want nil", err)
	}

	wrongType := mustItem(t, validItemInput(domain.AttentionSpecApproval))
	wrongType.ElapsedDeadlines = []domain.ElapsedDeadline{elapsedChecks()}
	if err := wrongType.Validate(); !errors.Is(err, domain.ErrCardFactOutsideItem) {
		t.Errorf("non-ready item Validate() = %v, want ErrCardFactOutsideItem", err)
	}

	cases := []struct {
		name   string
		mutate func(*domain.AttentionItem)
		want   error
	}{
		{"empty list", func(i *domain.AttentionItem) { i.ElapsedDeadlines = []domain.ElapsedDeadline{} }, domain.ErrEmptyField},
		{"unknown kind", func(i *domain.AttentionItem) { i.ElapsedDeadlines[0].Kind = domain.ScheduleJanitor }, domain.ErrInvalidElapsedDeadlineKind},
		{"repeated kind", func(i *domain.AttentionItem) { i.ElapsedDeadlines[1] = elapsedChecks() }, domain.ErrElapsedDeadlineRepeated},
		{"zero duration", func(i *domain.AttentionItem) { i.ElapsedDeadlines[0].DeadlineSeconds = 0 }, domain.ErrNonPositive},
		{"zero armed_at", func(i *domain.AttentionItem) { i.ElapsedDeadlines[0].ArmedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC armed_at", func(i *domain.AttentionItem) {
			i.ElapsedDeadlines[0].ArmedAt = i.ElapsedDeadlines[0].ArmedAt.In(time.FixedZone("x", 3600))
		}, domain.ErrTimestampNotUTC},
		{"non-UTC elapsed_at", func(i *domain.AttentionItem) {
			i.ElapsedDeadlines[1].ElapsedAt = i.ElapsedDeadlines[1].ElapsedAt.In(time.FixedZone("x", 3600))
		}, domain.ErrTimestampNotUTC},
		{"elapsed before deadline", func(i *domain.AttentionItem) {
			i.ElapsedDeadlines[0].ElapsedAt = i.ElapsedDeadlines[0].ArmedAt.Add(30*time.Minute - time.Second)
		}, domain.ErrElapsedDeadlineEarly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := ready
			item.ElapsedDeadlines = []domain.ElapsedDeadline{elapsedChecks(), elapsedReview()}
			tc.mutate(&item)
			if err := item.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestElapsedDeadlineTransitions pins the append-only rule: a successor keeps
// every stored entry as its prefix, in order.
func TestElapsedDeadlineTransitions(t *testing.T) {
	none := mustItem(t, validItemInput(domain.AttentionReadyForFinalReview))
	one := none
	one.ItemVersion++
	one.ElapsedDeadlines = []domain.ElapsedDeadline{elapsedChecks()}
	both := one
	both.ItemVersion++
	both.ElapsedDeadlines = []domain.ElapsedDeadline{elapsedChecks(), elapsedReview()}

	for _, tc := range []struct {
		name     string
		old, upd domain.AttentionItem
	}{
		{"first append", none, one},
		{"second append", one, both},
		{"two appends at once", none, both},
	} {
		if err := domain.ValidateAttentionItemTransition(tc.old, tc.upd); err != nil {
			t.Errorf("%s = %v, want accepted", tc.name, err)
		}
	}

	dropped := both
	dropped.ItemVersion++
	dropped.ElapsedDeadlines = nil
	truncated := both
	truncated.ItemVersion++
	truncated.ElapsedDeadlines = both.ElapsedDeadlines[:1]
	reordered := both
	reordered.ItemVersion++
	reordered.ElapsedDeadlines = []domain.ElapsedDeadline{elapsedReview(), elapsedChecks()}
	changed := one
	changed.ItemVersion++
	moved := elapsedChecks()
	moved.ElapsedAt = moved.ElapsedAt.Add(time.Minute)
	changed.ElapsedDeadlines = []domain.ElapsedDeadline{moved}
	for _, tc := range []struct {
		name     string
		old, upd domain.AttentionItem
	}{
		{"drop all", both, dropped},
		{"drop the last", both, truncated},
		{"reorder", both, reordered},
		{"rewrite an entry", one, changed},
	} {
		if err := domain.ValidateAttentionItemTransition(tc.old, tc.upd); !errors.Is(err, domain.ErrImmutableTransition) {
			t.Errorf("%s = %v, want ErrImmutableTransition", tc.name, err)
		}
	}
}
