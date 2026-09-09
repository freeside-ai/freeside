package engine

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestPublicationContinuationOfferDoesNotAdvanceRevisionForClosedDispute(t *testing.T) {
	f := newAttentionDiscussionFixture(t, domain.AttentionReviewDispute, nil)
	item := f.item
	item.ID = domain.ItemID(domain.PublicationContinuationItemPrefix + "old-recheck")
	item.Status, item.DecidedAt = domain.StatusResolved, &f.now
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error { return tx.PutAttentionItem(t.Context(), item) }); err != nil {
		t.Fatal(err)
	}
	before, err := f.store.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{store: f.store}
	for range 2 {
		if err := e.offerPublicationContinuation(t.Context(), domain.Command{CommandID: "old-recheck"}); err != nil {
			t.Fatal(err)
		}
	}
	after, err := f.store.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("unchanged legacy offer advanced sync revision: before=%d after=%d", before.Revision, after.Revision)
	}
}
