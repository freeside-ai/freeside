package publish

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestRunOutcomesListADeferredExternalFinding shows the run outcome list
// carries an external finding's deferral, marked external, with the
// finding's message and the disposition's reason. The external review
// fixture lives in this package, so the test does too.
func TestRunOutcomesListADeferredExternalFinding(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeferred, "Tracked for the next release.")
	var outcomes signet.RunOutcomes
	if err := h.f.st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		outcomes, _, err = signet.ReadRunOutcomes(context.Background(), tx, h.f.run.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !outcomes.Complete || len(outcomes.Findings) != 1 {
		t.Fatalf("outcomes = %+v, want one finding and a complete list", outcomes)
	}
	want := signet.RunOutcomeFinding{
		FindingID: h.d.FindingID, Disposition: domain.ReviewDispositionDeferred, Round: h.d.Round,
		External: true, Message: "Unchecked error", Reason: "Tracked for the next release.",
	}
	if got := outcomes.Findings[0]; got != want {
		t.Fatalf("finding = %+v, want %+v", got, want)
	}
}
