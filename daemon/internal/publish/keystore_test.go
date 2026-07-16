package publish_test

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// testKey generates a throwaway RSA key once per test binary; keystore
// tests need real key material but not a fixed fixture.
var testKey = func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}()

func testCredentials() publish.AppCredentials {
	return publish.AppCredentials{
		AppID:         12345,
		Slug:          "freeside-publish",
		ClientID:      "Iv1.deadbeefdeadbeef",
		Key:           testKey,
		WebhookSecret: publish.Secret("whsec_WEBHOOKWEBHOOK"),
		ClientSecret:  publish.Secret("cs_CLIENTSECRETCLIENTSECRET"),
	}
}

// TestNewKeystoreRejectsOverlap drives the structural containment
// invariant (issue #80 acceptance 2): construction fails closed for
// every overlapping layout, including a symlinked credentials dir that
// resolves back inside the state dir.
func TestNewKeystoreRejectsOverlap(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(filepath.Join(state, "creds"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "creds-link")
	if err := os.Symlink(filepath.Join(state, "creds"), link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		credentials string
		state       string
		wantErr     bool
	}{
		{"equal", state, state, true},
		{"credentials child of state", filepath.Join(state, "creds"), state, true},
		{"credentials grandchild of state", filepath.Join(state, "a", "creds"), state, true},
		{"state child of credentials", filepath.Join(base, "creds"), filepath.Join(base, "creds", "state"), true},
		{"symlink into state", link, state, true},
		{"case-folded nesting", filepath.Join(base, "creds"), filepath.Join(base, "Creds", "state"), true},
		{"case-folded key inside state", filepath.Join(base, "State", "creds"), filepath.Join(base, "state"), true},
		{"unclean nested path", filepath.Join(state, "x", "..", "creds"), state, true},
		{"empty credentials dir", "", state, true},
		{"empty state dir", filepath.Join(base, "creds"), "", true},
		{"disjoint siblings", filepath.Join(base, "creds"), state, false},
		{"disjoint not yet created", filepath.Join(base, "new", "creds"), filepath.Join(base, "new", "state"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := publish.NewKeystore(tc.credentials, tc.state)
			if tc.wantErr {
				if !errors.Is(err, publish.ErrCredentialsInsideStateDir) {
					t.Errorf("NewKeystore(%q, %q) = %v, want ErrCredentialsInsideStateDir", tc.credentials, tc.state, err)
				}
				return
			}
			if err != nil {
				t.Errorf("NewKeystore(%q, %q) = %v, want nil", tc.credentials, tc.state, err)
			}
		})
	}
}

// TestKeystoreRoundTrip saves and reloads the full credential set.
func TestKeystoreRoundTrip(t *testing.T) {
	ks := newTestKeystore(t)
	want := testCredentials()
	if err := ks.SaveApp(want); err != nil {
		t.Fatalf("SaveApp: %v", err)
	}

	got, err := ks.LoadApp()
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if got.AppID != want.AppID || got.Slug != want.Slug || got.ClientID != want.ClientID {
		t.Errorf("LoadApp identity = %+v, want %+v", got, want)
	}
	if got.WebhookSecret.Reveal() != want.WebhookSecret.Reveal() ||
		got.ClientSecret.Reveal() != want.ClientSecret.Reveal() {
		t.Error("LoadApp secrets do not round-trip")
	}
	if !got.Key.Equal(want.Key) {
		t.Error("LoadApp key does not round-trip")
	}
}

// TestKeystoreWritesStayOutsideStateDir walks everything SaveApp wrote
// and asserts every path is under the credentials root and nothing
// appeared under the state dir — the strongest checkpoint-exclusion
// assertion available before checkpoint code exists.
func TestKeystoreWritesStayOutsideStateDir(t *testing.T) {
	base := t.TempDir()
	credRoot := filepath.Join(base, "credentials")
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ks, err := publish.NewKeystore(credRoot, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatalf("SaveApp: %v", err)
	}

	var wrote []string
	err = filepath.WalkDir(ks.Dir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			wrote = append(wrote, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk credentials dir: %v", err)
	}
	if len(wrote) == 0 {
		t.Fatal("SaveApp wrote nothing under the credentials root")
	}
	for _, path := range wrote {
		if rel, err := filepath.Rel(stateDir, path); err == nil && filepath.IsLocal(rel) {
			t.Errorf("credential file %s is inside the state dir", path)
		}
	}

	err = filepath.WalkDir(stateDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != stateDir {
			t.Errorf("state dir gained an entry: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk state dir: %v", err)
	}
}

// TestKeystorePermissions asserts the on-disk modes and that a widened
// key file fails the next load closed.
func TestKeystorePermissions(t *testing.T) {
	ks := newTestKeystore(t)
	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatalf("SaveApp: %v", err)
	}

	appDir := filepath.Join(ks.Dir(), "github-app")
	for path, want := range map[string]fs.FileMode{
		ks.Dir():                          0o700,
		appDir:                            0o700,
		filepath.Join(appDir, "app.pem"):  0o600,
		filepath.Join(appDir, "app.json"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}

	keyPath := filepath.Join(appDir, "app.pem")
	// G302: the widened mode is the point — the next load must refuse it.
	if err := os.Chmod(keyPath, 0o644); err != nil { //nolint:gosec // deliberately widens the key to prove LoadApp fails closed
		t.Fatal(err)
	}
	if _, err := ks.LoadApp(); !errors.Is(err, publish.ErrCredentialPermissions) {
		t.Errorf("LoadApp with 0644 key = %v, want ErrCredentialPermissions", err)
	}
}

// TestSaveAppNarrowsWidePreexistingTargets covers re-registration into a
// keystore whose directories and files were widened: SaveApp must not
// write the fresh key through an exposed inode, so it narrows the
// directories and recreates the credential files owner-only before any
// secret bytes land.
func TestSaveAppNarrowsWidePreexistingTargets(t *testing.T) {
	ks := newTestKeystore(t)
	appDir := filepath.Join(ks.Dir(), "github-app")
	if err := os.MkdirAll(appDir, 0o755); err != nil { //nolint:gosec // deliberately wide: the pre-existing exposed keystore under test
		t.Fatal(err)
	}
	keyPath := filepath.Join(appDir, "app.pem")
	if err := os.WriteFile(keyPath, []byte("stale exposed key"), 0o644); err != nil { //nolint:gosec // deliberately wide, as above
		t.Fatal(err)
	}

	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatalf("SaveApp over widened keystore: %v", err)
	}

	for path, want := range map[string]fs.FileMode{
		ks.Dir():                          0o700,
		appDir:                            0o700,
		keyPath:                           0o600,
		filepath.Join(appDir, "app.json"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}
	if _, err := ks.LoadApp(); err != nil {
		t.Errorf("LoadApp after narrowing SaveApp: %v", err)
	}
}

// TestKeystoreRejectsSymlinkedAppDir covers the pre-existing child
// symlink: construction validates the credentials root, but a
// github-app entry that is itself a link would relocate every write
// onto the state tree, so SaveApp must refuse it (and LoadApp must
// refuse to read through it) with nothing landing at the target.
func TestKeystoreRejectsSymlinkedAppDir(t *testing.T) {
	base := t.TempDir()
	credRoot := filepath.Join(base, "credentials")
	stateDir := filepath.Join(base, "state")
	evil := filepath.Join(stateDir, "evil")
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(credRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, filepath.Join(credRoot, "github-app")); err != nil {
		t.Fatal(err)
	}

	ks, err := publish.NewKeystore(credRoot, stateDir)
	if err != nil {
		t.Fatalf("NewKeystore: %v", err)
	}
	if err := ks.SaveApp(testCredentials()); !errors.Is(err, publish.ErrCredentialPermissions) {
		t.Errorf("SaveApp through symlinked app dir = %v, want ErrCredentialPermissions", err)
	}
	if _, err := ks.LoadApp(); err == nil {
		t.Error("LoadApp through symlinked app dir succeeded, want error")
	}

	entries, err := os.ReadDir(evil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("symlink target gained %d entries; writes escaped the credentials root", len(entries))
	}
}

// TestSaveAppRejectsInvalidIdentity: the exported persistence boundary
// holds the same identity gate as the conversion path, so a direct
// caller cannot overwrite working credentials with an issuer-0
// identity or persist without key material.
func TestSaveAppRejectsInvalidIdentity(t *testing.T) {
	ks := newTestKeystore(t)
	noID := testCredentials()
	noID.AppID = 0
	if err := ks.SaveApp(noID); err == nil {
		t.Error("SaveApp with app id 0 succeeded, want error")
	}
	noKey := testCredentials()
	noKey.Key = nil
	if err := ks.SaveApp(noKey); err == nil {
		t.Error("SaveApp without key succeeded, want error")
	}
	if _, err := ks.LoadApp(); !errors.Is(err, publish.ErrNoAppCredentials) {
		t.Error("rejected SaveApp left credentials in the keystore")
	}
}

// TestLoadAppRejectsInvalidIdentity holds the same App-ID gate at the
// persistence reconstruction boundary, so a corrupted or restored
// metadata file cannot produce issuer-0 credentials.
func TestLoadAppRejectsInvalidIdentity(t *testing.T) {
	ks := newTestKeystore(t)
	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(ks.Dir(), "github-app", "app.json")
	meta, err := os.ReadFile(metaPath) //nolint:gosec // test-internal path rooted in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	meta = []byte(strings.Replace(string(meta), `"app_id": 12345`, `"app_id": 0`, 1))
	if err := os.WriteFile(metaPath, meta, 0o600); err != nil { //nolint:gosec // test-internal path rooted in t.TempDir
		t.Fatal(err)
	}
	if _, err := ks.LoadApp(); err == nil {
		t.Error("LoadApp with app id 0 succeeded, want error")
	}
}

// TestSaveAppRejectsSymlinkedAncestor: a missing ancestor created as a
// symlink after construction would carry MkdirAll (and the key) onto
// the state surface; the creation walk must refuse it and nothing may
// land at the target.
func TestSaveAppRejectsSymlinkedAncestor(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The credentials root's parent does not exist at construction.
	credRoot := filepath.Join(base, "a", "creds")
	ks, err := publish.NewKeystore(credRoot, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	// The attacker plants the missing ancestor as a link to the state
	// tree before the first save.
	if err := os.Symlink(stateDir, filepath.Join(base, "a")); err != nil {
		t.Fatal(err)
	}

	if err := ks.SaveApp(testCredentials()); !errors.Is(err, publish.ErrCredentialPermissions) {
		t.Errorf("SaveApp through symlinked ancestor = %v, want ErrCredentialPermissions", err)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("state dir gained %d entries; writes escaped through the ancestor link", len(entries))
	}
}

// TestSaveAppPreservesOldCredentialsOnFailure: re-registration must
// never destroy the only working credentials before the replacement is
// durable; a failed save leaves the previous credentials loadable.
func TestSaveAppPreservesOldCredentialsOnFailure(t *testing.T) {
	ks := newTestKeystore(t)
	want := testCredentials()
	if err := ks.SaveApp(want); err != nil {
		t.Fatal(err)
	}

	// Make staging impossible: the read-only root refuses new entries,
	// and SaveApp only strips group/other bits so it won't re-widen it.
	if err := os.Chmod(ks.Dir(), 0o500); err != nil { //nolint:gosec // deliberately makes staging fail to prove old creds survive
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ks.Dir(), 0o700) }) //nolint:gosec // restore so t.TempDir cleanup can remove it

	replacement := testCredentials()
	replacement.AppID = 99999
	if err := ks.SaveApp(replacement); err == nil {
		t.Fatal("SaveApp with unwritable root succeeded, want error")
	}

	if err := os.Chmod(ks.Dir(), 0o700); err != nil { //nolint:gosec // re-widen to read the preserved credentials
		t.Fatal(err)
	}
	got, err := ks.LoadApp()
	if err != nil {
		t.Fatalf("LoadApp after failed replacement: %v", err)
	}
	if got.AppID != want.AppID {
		t.Errorf("AppID = %d, want the original %d preserved", got.AppID, want.AppID)
	}
}

// TestSaveAppCleansStaleSwapLeftovers: a crash between the swap steps
// leaves .staging/.old directories; the next save clears them and
// succeeds.
func TestSaveAppCleansStaleSwapLeftovers(t *testing.T) {
	ks := newTestKeystore(t)
	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(ks.Dir(), "github-app")
	for _, leftover := range []string{appDir + ".staging", appDir + ".old"} {
		if err := os.MkdirAll(leftover, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(leftover, "app.pem"), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(leftover, 0o500); err != nil { //nolint:gosec // restored owner-only leftover under test
			t.Fatal(err)
		}
	}

	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatalf("SaveApp over swap leftovers: %v", err)
	}
	for _, leftover := range []string{appDir + ".staging", appDir + ".old"} {
		if _, err := os.Lstat(leftover); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("leftover %s survived SaveApp", leftover)
		}
	}
	if _, err := ks.LoadApp(); err != nil {
		t.Errorf("LoadApp after leftover cleanup: %v", err)
	}
}

// TestSaveAppReplacesReadOnlyCredentials proves cleanup cannot report a false
// failure after replacement has already activated the new credentials.
func TestSaveAppReplacesReadOnlyCredentials(t *testing.T) {
	ks := newTestKeystore(t)
	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(ks.Dir(), "github-app")
	t.Cleanup(func() {
		_ = os.Chmod(appDir, 0o700)        //nolint:gosec // restore only if the assertion fails before replacement
		_ = os.Chmod(appDir+".old", 0o700) //nolint:gosec // restore only if cleanup regresses
	})
	if err := os.Chmod(appDir, 0o500); err != nil { //nolint:gosec // restored owner-only credential directory under test
		t.Fatal(err)
	}

	replacement := testCredentials()
	replacement.AppID = 99999
	if err := ks.SaveApp(replacement); err != nil {
		t.Fatalf("SaveApp over read-only credentials: %v", err)
	}
	got, err := ks.LoadApp()
	if err != nil {
		t.Fatal(err)
	}
	if got.AppID != replacement.AppID {
		t.Errorf("AppID = %d, want replacement %d", got.AppID, replacement.AppID)
	}
	if _, err := os.Lstat(appDir + ".old"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("read-only previous credentials survived cleanup: %v", err)
	}
}

// TestLoadAppRecoversInterruptedReplacement simulates a crash after
// the old active directory was journaled aside but before activation.
// The next load restores the known-good old credentials and discards
// the incomplete staging directory.
func TestLoadAppRecoversInterruptedReplacement(t *testing.T) {
	ks := newTestKeystore(t)
	want := testCredentials()
	if err := ks.SaveApp(want); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(ks.Dir(), "github-app")
	if err := os.Rename(appDir, appDir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(appDir+".staging", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir+".staging", "app.pem"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ks.LoadApp()
	if err != nil {
		t.Fatalf("LoadApp after interrupted replacement: %v", err)
	}
	if got.AppID != want.AppID {
		t.Errorf("AppID = %d, want restored %d", got.AppID, want.AppID)
	}
	for _, leftover := range []string{appDir + ".staging", appDir + ".old"} {
		if _, err := os.Lstat(leftover); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("leftover %s survived recovery", leftover)
		}
	}
}

// TestLoadAppRecoversCompletedInitialStage simulates a first-save crash
// after the complete staging directory was synced but before its
// activation rename. With no old credentials to prefer, the validated
// staging directory becomes active on restart.
func TestLoadAppRecoversCompletedInitialStage(t *testing.T) {
	ks := newTestKeystore(t)
	want := testCredentials()
	if err := ks.SaveApp(want); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(ks.Dir(), "github-app")
	if err := os.Rename(appDir, appDir+".staging"); err != nil {
		t.Fatal(err)
	}

	got, err := ks.LoadApp()
	if err != nil {
		t.Fatalf("LoadApp after completed initial stage: %v", err)
	}
	if got.AppID != want.AppID {
		t.Errorf("AppID = %d, want recovered %d", got.AppID, want.AppID)
	}
	if _, err := os.Lstat(appDir + ".staging"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("staging directory survived recovery")
	}
}

// TestSaveAppReplacesIncompleteInitialStage proves a crash before the
// first staging directory became complete does not permanently wedge
// registration. A later SaveApp already holds fresh converted
// credentials, so it discards the unusable stage and activates them.
func TestSaveAppReplacesIncompleteInitialStage(t *testing.T) {
	ks := newTestKeystore(t)
	staging := filepath.Join(ks.Dir(), "github-app.staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "app.pem"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatalf("SaveApp over incomplete initial stage: %v", err)
	}
	if _, err := ks.LoadApp(); err != nil {
		t.Fatalf("LoadApp after replacing incomplete initial stage: %v", err)
	}
}

// TestLoadAppClearsIncompleteInitialStage converges a crash before the
// first stage became complete back to the ordinary unauthenticated
// state, so the daemon can ask for re-registration without manual
// filesystem repair.
func TestLoadAppClearsIncompleteInitialStage(t *testing.T) {
	ks := newTestKeystore(t)
	staging := filepath.Join(ks.Dir(), "github-app.staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "app.pem"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ks.LoadApp(); !errors.Is(err, publish.ErrNoAppCredentials) {
		t.Errorf("LoadApp with incomplete initial stage = %v, want ErrNoAppCredentials", err)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("incomplete staging directory survived load")
	}
}

// TestLoadAppEmpty covers the pre-registration (and post-restore) state.
func TestLoadAppEmpty(t *testing.T) {
	ks := newTestKeystore(t)
	if _, err := ks.LoadApp(); !errors.Is(err, publish.ErrNoAppCredentials) {
		t.Errorf("LoadApp on empty keystore = %v, want ErrNoAppCredentials", err)
	}
}

func newTestKeystore(t *testing.T) *publish.Keystore {
	t.Helper()
	base := t.TempDir()
	ks, err := publish.NewKeystore(filepath.Join(base, "credentials"), filepath.Join(base, "state"))
	if err != nil {
		t.Fatalf("NewKeystore: %v", err)
	}
	return ks
}
