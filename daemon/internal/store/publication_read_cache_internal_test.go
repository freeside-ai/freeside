package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestPublicationReadPathDistinguishesAncestry(t *testing.T) {
	t.Parallel()
	paths := [][]string{
		{"successor"},
		{"ready", "successor"},
		{"other", "successor"},
		{"a", "bc", "successor"},
		{"ab", "c", "successor"},
		{"1:a", "successor"},
		{"a:1", "successor"},
		{"a/b", "c", "successor"},
		{"a", "b/c", "successor"},
	}
	seen := make(map[string]bool)
	for _, path := range paths {
		var previous string
		for repeat := range 2 {
			ctx := t.Context()
			for _, key := range path {
				var err error
				ctx, err = publicationReadContext(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
			}
			key := publicationReadPath(ctx)
			if repeat == 0 {
				if seen[key] {
					t.Fatalf("different ancestry shared a cache key: %v", path)
				}
				seen[key] = true
				previous = key
			} else if key != previous {
				t.Fatal("equivalent ancestry depended on context allocation")
			}
			if _, err := publicationReadContext(ctx, path[0]); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("cycle in ancestry was accepted: %v", err)
			}
		}
	}
}

func TestPublicationReadCacheLifetimeAndFailures(t *testing.T) {
	t.Parallel()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	for range 2 {
		if err := st.Read(t.Context(), func(tx *ReadTx) error {
			if tx.publicationSuccessorReads == nil || len(tx.publicationSuccessorReads) != 0 {
				t.Fatal("read did not begin with an empty publication cache")
			}
			for range 2 {
				if _, err := tx.GetPublicationSuccessor(t.Context(), "missing-run", "missing-publication"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing successor: %v", err)
				}
			}
			if len(tx.publicationSuccessorReads) != 0 {
				t.Fatal("failed publication read was retained")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(tx *ReadTx) error {
		if tx.publicationSuccessorReads != nil {
			t.Fatal("write transaction can reuse stale publication authority")
		}
		return nil
	}
	if err := st.Write(t.Context(), func(tx *WriteTx) error { return check(&tx.ReadTx) }); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(t.Context(), func(tx *InternalTx) error { return check(&tx.ReadTx) }); err != nil {
		t.Fatal(err)
	}
}
