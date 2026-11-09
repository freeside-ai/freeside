package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestAttentionItemViewPutGet covers the view row (issue #2930): absent until
// put, insert-only so a second put keeps the first time, written on the
// synchronized path so the write bumps the revision and the row carries it,
// and keyed by digest so another surface of the same item stays unviewed.
func TestAttentionItemViewPutGet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := tempDBPath(t)
	s := openStoreAt(t, path, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedComprehensionDeps(t, ctx, s, f)
	surface, err := domain.NewDecisionSurface(f.item)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	view := domain.AttentionItemView{ItemID: f.item.ID, DecisionSurfaceDigest: surface.Digest, FirstOpenedAt: first}

	get := func(digest domain.Digest) (domain.AttentionItemView, int64, error) {
		var (
			got      domain.AttentionItemView
			revision int64
		)
		err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			got, revision, err = tx.GetAttentionItemView(ctx, f.item.ID, digest)
			return err
		})
		return got, revision, err
	}
	if _, _, err := get(surface.Digest); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get before put = %v, want ErrNotFound", err)
	}

	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItemView(ctx, view) }); err != nil {
		t.Fatalf("put: %v", err)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision+1 {
		t.Fatalf("put moved revision %d -> %d, want one bump", before.Revision, after.Revision)
	}
	got, revision, err := get(surface.Digest)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != view || revision != after.Revision {
		t.Fatalf("get = %+v at %d, want %+v at %d", got, revision, view, after.Revision)
	}

	later := view
	later.FirstOpenedAt = first.Add(time.Hour)
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItemView(ctx, later) }); err != nil {
		t.Fatalf("second put: %v", err)
	}
	if got, revision, err := get(surface.Digest); err != nil || got != view || revision != after.Revision {
		t.Fatalf("after second put get = %+v at %d (%v), want the first %+v at %d", got, revision, err, view, after.Revision)
	}

	other := domain.Digest("sha256:" + strings.Repeat("0", 64))
	if _, _, err := get(other); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get of another surface = %v, want ErrNotFound", err)
	}

	s = tamperStore(t, path, s,
		`UPDATE attention_item_views SET first_opened_at = ? WHERE item_id = ?`,
		"2026-01-02T03:04:05+01:00", string(f.item.ID))
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, _, err := tx.GetAttentionItemView(ctx, f.item.ID, surface.Digest)
		return err
	}); err != nil {
		t.Fatalf("an offset time is the same instant and parses to UTC: %v", err)
	}
	s = tamperStore(t, path, s,
		`UPDATE attention_item_views SET first_opened_at = ? WHERE item_id = ?`, "not a time", string(f.item.ID))
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, _, err := tx.GetAttentionItemView(ctx, f.item.ID, surface.Digest)
		return err
	}); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("malformed row get = %v, want a decode error", err)
	}
}
