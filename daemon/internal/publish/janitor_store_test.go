package publish_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// newAuthorityStore prepares a state directory holding the payload and returns
// the directory alongside a store over it.
func newAuthorityStore(t *testing.T, payload string) (string, *publish.InstallationAuthorityStore) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if payload != "" {
		writeAuthorityFile(t, dir, payload)
	}
	store, err := publish.NewInstallationAuthorityStore(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return dir, store
}

func writeAuthorityFile(t *testing.T, dir, payload string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "installation-authority.json"), []byte(payload), 0o600); err != nil {
		t.Fatalf("write authority: %v", err)
	}
}

func TestInstallationAuthorityStoreServesTheAuthoredEntry(t *testing.T) {
	t.Parallel()
	_, store := newAuthorityStore(t, validAuthorityJSON)

	authority, err := store.InstallationAuthority(t.Context(), 91)
	if err != nil {
		t.Fatalf("installation authority: %v", err)
	}
	if authority.ActiveEpoch != 2 || authority.DurableIntentRevision != 5 {
		t.Fatalf("frontier is %d/%d, want 2/5", authority.ActiveEpoch, authority.DurableIntentRevision)
	}
	if len(authority.TrustedOwners) != 1 || authority.TrustedOwners[0].ID != 4242 {
		t.Fatalf("trusted owners are %+v", authority.TrustedOwners)
	}
	if len(authority.TrustedInstallations) != 1 {
		t.Fatalf("trusted installations are %+v", authority.TrustedInstallations)
	}
	binding := authority.TrustedInstallations[0]
	// The entry's registration ID must reach the binding: the janitor rejects a
	// binding that names a different registration than the App it reconciles.
	if binding.RegistrationID != 91 || binding.InstallationID != 77 {
		t.Fatalf("binding is %+v, want registration 91 installation 77", binding)
	}
	if authority.Pending != nil {
		t.Fatalf("pending envelope is %+v, want none", authority.Pending)
	}
}

func TestInstallationAuthorityStoreInitializesCanonicalRegistrationAuthority(t *testing.T) {
	t.Parallel()
	_, store := newAuthorityStore(t, "")
	if err := store.InitializeDocument(t.Context()); err != nil {
		t.Fatal(err)
	}
	registration := publish.AppRegistration{
		Owner: "operator", OwnerID: 4242, Visibility: publish.AppVisibilityPublic,
		AppID: 91, Name: "freeside-operator", Slug: "freeside-operator",
		ClientID: "Iv1.example",
	}
	if err := store.InitializeRegistration(t.Context(), registration); err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeRegistration(t.Context(), registration); err != nil {
		t.Fatalf("idempotent registration initialization: %v", err)
	}
	document, err := store.Document(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Registrations) != 1 {
		t.Fatalf("registrations = %+v", document.Registrations)
	}
	entry := document.Registrations[0]
	if entry.RegistrationID != registration.AppID ||
		entry.ActiveEpoch != 1 || entry.DurableIntentRevision != 1 ||
		len(entry.TrustedOwners) != 1 ||
		entry.TrustedOwners[0].Login != registration.Owner ||
		entry.TrustedOwners[0].ID != registration.OwnerID ||
		entry.TrustedInstallations == nil ||
		len(entry.TrustedInstallations) != 0 {
		t.Fatalf("initial registration authority = %+v", entry)
	}
}

func TestInstallationAuthorityStoreCopiesTheRegistrationIntoThePendingEnvelope(t *testing.T) {
	t.Parallel()
	payload := withPending(`"active_epoch": 2, "durable_intent_revision": 5,
      "expected_account": "example-org", "expected_account_id": 4242, "installation_id": 77,
      "current_repository_ids": [10, 20], "expected_repository_ids": [10, 20, 30],
      "required_repository_mode": "selected", "expires_at": "2026-07-25T12:00:00Z"`)
	_, store := newAuthorityStore(t, payload)

	authority, err := store.InstallationAuthority(t.Context(), 91)
	if err != nil {
		t.Fatalf("installation authority: %v", err)
	}
	if authority.Pending == nil || authority.Pending.RegistrationID != 91 {
		t.Fatalf("pending envelope is %+v, want registration 91", authority.Pending)
	}
}

func TestInstallationAuthorityStoreDocumentReplacementIsValidatedAndAtomic(t *testing.T) {
	t.Parallel()
	_, store := newAuthorityStore(t, validAuthorityJSON)
	document, err := store.Document(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	document.Registrations[0].DurableIntentRevision++
	if err := store.UpdateDocument(
		t.Context(),
		func(current *publish.InstallationAuthorityDocument) error {
			*current = document
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.Document(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Registrations[0].DurableIntentRevision != 6 {
		t.Fatalf("revision = %d, want 6", reloaded.Registrations[0].DurableIntentRevision)
	}
	invalid := reloaded
	invalid.Registrations[0].TrustedInstallations = nil
	if err := store.UpdateDocument(
		t.Context(),
		func(current *publish.InstallationAuthorityDocument) error {
			*current = invalid
			return nil
		},
	); err == nil {
		t.Fatal("UpdateDocument accepted an implicit destructive empty binding set")
	}
	unchanged, err := store.Document(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Registrations[0].DurableIntentRevision != 6 ||
		unchanged.Registrations[0].TrustedInstallations == nil {
		t.Fatalf("failed replacement changed document: %+v", unchanged)
	}
}

func TestInstallationAuthorityStoreSerializesSeparateWriters(t *testing.T) {
	t.Parallel()
	dir, first := newAuthorityStore(t, validAuthorityJSON)
	second, err := publish.NewInstallationAuthorityStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.UpdateDocument(
			context.Background(),
			func(document *publish.InstallationAuthorityDocument) error {
				close(firstEntered)
				<-releaseFirst
				document.Registrations[0].DurableIntentRevision++
				return nil
			},
		)
	}()
	<-firstEntered
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- second.UpdateDocument(
			context.Background(),
			func(document *publish.InstallationAuthorityDocument) error {
				close(secondEntered)
				document.Registrations[0].DurableIntentRevision += 10
				return nil
			},
		)
	}()
	select {
	case <-secondEntered:
		t.Fatal("separate authority writer entered while the state lock was held")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	document, err := first.Document(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := document.Registrations[0].DurableIntentRevision; got != 16 {
		t.Fatalf("serialized revision = %d, want 16", got)
	}
}

func TestInstallationAuthorityStoreDeniesUnusableState(t *testing.T) {
	t.Parallel()
	// A denial is the only safe answer for every one of these: serving an empty
	// authority instead would tell the janitor that no installation is trusted,
	// which is an instruction to delete all of them.
	cases := []struct {
		name    string
		absent  bool
		prepare func(t *testing.T, dir string)
		lookup  int64
	}{
		{
			name:    "absent file",
			absent:  true,
			prepare: func(t *testing.T, dir string) { t.Helper() },
			lookup:  91,
		},
		{
			name: "group readable",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				chmod(t, filepath.Join(dir, "installation-authority.json"), 0o640)
			},
			lookup: 91,
		},
		{
			name: "world readable",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				chmod(t, filepath.Join(dir, "installation-authority.json"), 0o604)
			},
			lookup: 91,
		},
		{
			name: "symlinked file",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(dir), "elsewhere.json")
				if err := os.WriteFile(target, []byte(validAuthorityJSON), 0o600); err != nil {
					t.Fatalf("write target: %v", err)
				}
				path := filepath.Join(dir, "installation-authority.json")
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove authority: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink authority: %v", err)
				}
			},
			lookup: 91,
		},
		{
			name: "state directory replaced by a symlink",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				elsewhere := filepath.Join(filepath.Dir(dir), "elsewhere")
				if err := os.Mkdir(elsewhere, 0o700); err != nil {
					t.Fatalf("create elsewhere: %v", err)
				}
				writeAuthorityFile(t, elsewhere, validAuthorityJSON)
				if err := os.RemoveAll(dir); err != nil {
					t.Fatalf("remove state dir: %v", err)
				}
				if err := os.Symlink(elsewhere, dir); err != nil {
					t.Fatalf("symlink state dir: %v", err)
				}
			},
			lookup: 91,
		},
		{
			name: "group writable directory",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				chmod(t, dir, 0o770)
			},
			lookup: 91,
		},
		{
			name: "directory in place of the file",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				path := filepath.Join(dir, "installation-authority.json")
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove authority: %v", err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("mkdir authority: %v", err)
				}
			},
			lookup: 91,
		},
		{
			name: "over the size limit",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				padding := strings.Repeat(" ", 1<<20)
				writeAuthorityFile(t, dir, validAuthorityJSON+padding)
			},
			lookup: 91,
		},
		{
			name:    "registration with no entry",
			prepare: func(t *testing.T, dir string) { t.Helper() },
			lookup:  92,
		},
		{
			name:    "non-positive registration",
			prepare: func(t *testing.T, dir string) { t.Helper() },
			lookup:  0,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload := validAuthorityJSON
			if testCase.absent {
				payload = ""
			}
			dir, store := newAuthorityStore(t, payload)
			testCase.prepare(t, dir)

			authority, err := store.InstallationAuthority(t.Context(), testCase.lookup)
			if err == nil {
				t.Fatalf("served %+v, want an error", authority)
			}
			if !errors.Is(err, publish.ErrInstallationAuthoritySnapshot) {
				t.Fatalf("error %v does not match ErrInstallationAuthoritySnapshot", err)
			}
			if len(authority.TrustedInstallations) != 0 || authority.Pending != nil {
				t.Fatalf("served %+v alongside its error", authority)
			}
		})
	}
}

func TestInstallationAuthorityStoreHonoursCancellation(t *testing.T) {
	t.Parallel()
	_, store := newAuthorityStore(t, validAuthorityJSON)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := store.InstallationAuthority(ctx, 91); !errors.Is(err, context.Canceled) {
		t.Fatalf("error is %v, want context.Canceled", err)
	}
}

func TestInstallationAuthorityStoreReadsEveryCall(t *testing.T) {
	t.Parallel()
	dir, store := newAuthorityStore(t, validAuthorityJSON)
	if _, err := store.InstallationAuthority(t.Context(), 91); err != nil {
		t.Fatalf("first read: %v", err)
	}

	// An operator correction must reach the next pass without a restart.
	writeAuthorityFile(t, dir, strings.Replace(validAuthorityJSON, `"repository_ids": [10, 20]`, `"repository_ids": [10]`, 1))
	authority, err := store.InstallationAuthority(t.Context(), 91)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if got := authority.TrustedInstallations[0].RepositoryIDs; len(got) != 1 || got[0] != 10 {
		t.Fatalf("repository IDs are %v, want the corrected [10]", got)
	}
}

func TestNewInstallationAuthorityStoreRejectsUnusableDirectories(t *testing.T) {
	t.Parallel()
	if _, err := publish.NewInstallationAuthorityStore("   "); err == nil {
		t.Fatal("empty state directory was accepted")
	}
	file := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := publish.NewInstallationAuthorityStore(file); err == nil {
		t.Fatal("a regular file was accepted as the state directory")
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}
