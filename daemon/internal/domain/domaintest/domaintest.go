// Package domaintest builds domain values that tests in several packages
// need and production code never constructs this way.
package domaintest

import (
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ConcludedAt is the instant MustConclude stamps on a daemon close.
var ConcludedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// MustConclude returns item, already carrying its closing status, with the
// conclusion that close records (#2097), so a fixture that closes an item by
// hand passes the store's transition gate. An item with decided_at gets a
// decision conclusion at that instant, from a fixed test command and device;
// a dismissed item without one is decided at ConcludedAt, since only a
// decision dismisses; any other closed item gets the daemon cause its status
// implies, at ConcludedAt, with a fixed reason. An item that already carries
// a conclusion is returned unchanged. It panics on an item no close
// produces, such as an open one, because that is a broken fixture rather
// than a case under test.
func MustConclude(item domain.AttentionItem) domain.AttentionItem {
	if item.Conclusion != nil {
		return item
	}
	var (
		conclusion domain.Conclusion
		err        error
	)
	if item.Status == domain.StatusDismissed && item.DecidedAt == nil {
		if item, err = item.WithDecidedAt(ConcludedAt); err != nil {
			panic(fmt.Sprintf("domaintest.MustConclude(%s): %v", item.ID, err))
		}
	}
	if item.DecidedAt != nil {
		action := domain.ActionApprove
		if item.Status == domain.StatusDismissed {
			action = domain.ActionDismiss
		}
		conclusion, err = domain.NewDecisionConclusion(*item.DecidedAt, domain.ConclusionDecision{
			CommandID: "test-command", Action: action, DeviceID: "test-device", DeviceDisplayName: "Test device",
		})
	} else {
		var cause domain.ConclusionCause
		switch item.Status {
		case domain.StatusExpired:
			cause = domain.ConcludedByExpiry
		case domain.StatusSuperseded:
			cause = domain.ConcludedBySupersession
		case domain.StatusResolved:
			cause = domain.ConcludedByDaemon
		case domain.StatusOpen, domain.StatusDismissed:
		}
		conclusion, err = domain.NewDaemonConclusion(cause, ConcludedAt, "Closed by the test.", nil)
	}
	if err == nil {
		item, err = item.WithConclusion(conclusion)
	}
	if err != nil {
		panic(fmt.Sprintf("domaintest.MustConclude(%s, status %q): %v", item.ID, item.Status, err))
	}
	return item
}
