package topicstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestOpenRetriesAfterTopicKeyPublicationFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	publicationErr := errors.New("injected topic-key publication failure")
	failingWrite := func(string, []byte, fs.FileMode) error {
		return publicationErr
	}

	if _, _, err := open(t.Context(), dbPath, store.Options{}, failingWrite); !errors.Is(err, publicationErr) {
		t.Fatalf("first open error = %v, want injected publication failure", err)
	}
	for _, path := range []string{dbPath, dbPath + KeySuffix} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat %s after failed publication = %v, want ErrNotExist", path, err)
		}
	}

	st, key, err := Open(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatalf("retry open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close retry store: %v", err)
	}
	if len(key) == 0 {
		t.Fatal("retry returned an empty topic key")
	}
}
