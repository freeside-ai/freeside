package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func seedTestImage(t *testing.T, ref string) domain.ProjectImage {
	t.Helper()
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeasinbird/gh-imgup", RepositoryID: 1278475858,
		CommitSHA:          strings.Repeat("a", 40),
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
		PreparationCommand: []string{"/usr/local/bin/freeside-project-prepare"},
		BaseImageRef:       domain.ImageRef("example.test/base@sha256:" + strings.Repeat("a", 64)),
		ImageRef:           domain.ImageRef("example.test/project@sha256:" + strings.Repeat(ref, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func readSeededPointer(t *testing.T, st *store.Store, repositoryID int64) (domain.AdmittedImagePointer, bool) {
	t.Helper()
	var (
		pointer domain.AdmittedImagePointer
		found   bool
	)
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		pointer, _, found, err = tx.GetAdmittedImagePointer(t.Context(), repositoryID, domain.HarnessClientClaudeCode)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return pointer, found
}

// The configured agent image seeds a repository's pointer only when it has
// none; once a pointer exists, a start with another configured image keeps it.
func TestSeedAdmittedImagePointerSeedsOnlyWhenAbsent(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	first, second := seedTestImage(t, "b"), seedTestImage(t, "d")
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		if err := tx.RecordProjectImage(t.Context(), first); err != nil {
			return err
		}
		return tx.RecordProjectImage(t.Context(), second)
	}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	seededAt := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	if err := seedAdmittedImagePointer(t.Context(), st, first, seededAt, logger); err != nil {
		t.Fatalf("seed absent: %v", err)
	}
	pointer, found := readSeededPointer(t, st, first.RepositoryID)
	if !found || pointer.ImageID != first.ID || !pointer.AdvancedAt.Equal(seededAt) {
		t.Fatalf("seeded pointer = %+v found=%v, want image %s", pointer, found, first.ID)
	}

	if err := seedAdmittedImagePointer(t.Context(), st, second, seededAt.Add(time.Hour), logger); err != nil {
		t.Fatalf("seed present: %v", err)
	}
	if kept, _ := readSeededPointer(t, st, first.RepositoryID); kept != pointer {
		t.Fatalf("pointer after second start = %+v, want unchanged %+v", kept, pointer)
	}
	if !strings.Contains(logs.String(), "keeping the pointer") {
		t.Fatalf("logs = %q, want the kept-pointer notice", logs.String())
	}
}
