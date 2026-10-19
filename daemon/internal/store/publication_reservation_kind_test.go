package store_test

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The unpublished re-entry seal recognizes an open reservation by its outbox
// kind, which the publish package owns and the store cannot import.
func TestPublicationReservationKind(t *testing.T) {
	if store.PublicationReservationKind != publish.IntentKindReservation {
		t.Fatalf("store reservation kind %q, publish writes %q",
			store.PublicationReservationKind, publish.IntentKindReservation)
	}
}
