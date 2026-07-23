package publish_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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
		Owner:         "freeside-ai",
		OwnerID:       testOwnerID,
		Visibility:    publish.AppVisibilityPrivate,
		AppID:         12345,
		Name:          "Freeside Publish",
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

	got, err := ks.LoadApp(want.OwnerID)
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

// TestKeystoreMultipleRegistrations pins the owner-keyed layout,
// deterministic enumeration, metadata, and containment modes.
func TestKeystoreMultipleRegistrations(t *testing.T) {
	ks := newTestKeystore(t)
	personal := testCredentials()
	personal.Owner = "BenNelsonWeiss"
	personal.OwnerID = 111
	personal.Visibility = publish.AppVisibilityPublic
	personal.AppID = 111
	personal.KeyID = "caller-supplied-value-must-not-win"
	org := testCredentials()
	org.Owner = "freeside-ai"
	org.OwnerID = 222
	org.Visibility = publish.AppVisibilityPrivate
	org.AppID = 222

	for _, creds := range []publish.AppCredentials{org, personal} {
		if err := ks.SaveApp(creds); err != nil {
			t.Fatalf("SaveApp(%s): %v", creds.Owner, err)
		}
	}

	apps, err := ks.ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("ListApps returned %d registrations, want 2", len(apps))
	}
	if apps[0].Owner != personal.Owner || apps[1].Owner != org.Owner {
		t.Errorf("owner order = [%s, %s], want [%s, %s]", apps[0].Owner, apps[1].Owner, personal.Owner, org.Owner)
	}
	gotPersonal, err := ks.LoadApp(personal.OwnerID)
	if err != nil {
		t.Fatalf("LoadApp case-insensitive owner: %v", err)
	}
	if gotPersonal.OwnerID != personal.OwnerID ||
		gotPersonal.Visibility != publish.AppVisibilityPublic ||
		gotPersonal.AppID != personal.AppID {
		t.Errorf("personal registration = %+v", gotPersonal)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&personal.Key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(publicDER)
	wantKeyID := "SHA256:" + base64.StdEncoding.EncodeToString(digest[:])
	if gotPersonal.KeyID != wantKeyID {
		t.Errorf("key id = %q, want %q", gotPersonal.KeyID, wantKeyID)
	}

	for path, want := range map[string]fs.FileMode{
		ks.Dir():                              0o700,
		filepath.Join(ks.Dir(), "github-app"): 0o700,
		testAppDir(ks, personal.OwnerID):      0o700,
		filepath.Join(testAppDir(ks, personal.OwnerID), keyFileNameForTest):  0o600,
		filepath.Join(testAppDir(ks, personal.OwnerID), metaFileNameForTest): 0o600,
		testAppDir(ks, org.OwnerID):                                          0o700,
		filepath.Join(testAppDir(ks, org.OwnerID), keyFileNameForTest):       0o600,
		filepath.Join(testAppDir(ks, org.OwnerID), metaFileNameForTest):      0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}
}

// TestKeystoreOwnerRenameKeepsStableRegistration proves a mutable login is
// display metadata, not the on-disk identity: saving the same numeric owner
// after a rename replaces one registration rather than orphaning the old path.
func TestKeystoreOwnerRenameKeepsStableRegistration(t *testing.T) {
	ks := newTestKeystore(t)
	before := testCredentials()
	if err := ks.SaveApp(before); err != nil {
		t.Fatal(err)
	}
	after := before
	after.Owner = "freeside-renamed"
	if err := ks.SaveApp(after); err != nil {
		t.Fatal(err)
	}

	apps, err := ks.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].OwnerID != before.OwnerID || apps[0].Owner != after.Owner {
		t.Errorf("registrations after owner rename = %+v, want one owner %q (%d)", apps, after.Owner, before.OwnerID)
	}
	if _, err := ks.LoadApp(before.OwnerID); err != nil {
		t.Errorf("LoadApp by stable owner ID after rename: %v", err)
	}
}

// TestListAppsFailsClosedOnIncompleteActiveRegistration proves enumeration
// cannot hide a damaged registration and let minting proceed with a partial
// view of the credential set.
func TestListAppsFailsClosedOnIncompleteActiveRegistration(t *testing.T) {
	ks := newTestKeystore(t)
	personal := testCredentials()
	personal.Owner = "BenNelsonWeiss"
	personal.OwnerID = 111
	org := testCredentials()
	org.Owner = "freeside-ai"
	org.OwnerID = 222
	org.AppID = 222
	for _, creds := range []publish.AppCredentials{personal, org} {
		if err := ks.SaveApp(creds); err != nil {
			t.Fatalf("SaveApp(%s): %v", creds.Owner, err)
		}
	}
	if err := os.Remove(filepath.Join(testAppDir(ks, org.OwnerID), keyFileNameForTest)); err != nil {
		t.Fatal(err)
	}

	if _, err := ks.LoadApp(org.OwnerID); err == nil || errors.Is(err, publish.ErrNoAppRegistration) {
		t.Errorf("LoadApp after active-key loss = %v, want corruption error", err)
	}
	apps, err := ks.ListApps()
	if err == nil {
		t.Fatalf("ListApps returned %d registrations after active-key loss, want error", len(apps))
	}
	if !strings.Contains(err.Error(), strconv.FormatInt(org.OwnerID, 10)) {
		t.Errorf("ListApps error %q does not identify damaged owner ID %d", err, org.OwnerID)
	}
}

// TestListAppsSkipsDiscardedIncompleteStage distinguishes an incomplete
// first-save journal from an incomplete active registration: recovery may
// discard the former because no credential was ever activated.
func TestListAppsSkipsDiscardedIncompleteStage(t *testing.T) {
	ks := newTestKeystore(t)
	stage := testAppDir(ks, testCredentials().OwnerID) + ".staging"
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, metaFileNameForTest), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	apps, err := ks.ListApps()
	if err != nil {
		t.Fatalf("ListApps after incomplete stage: %v", err)
	}
	if len(apps) != 0 {
		t.Errorf("ListApps returned %d registrations after discarding incomplete stage, want 0", len(apps))
	}
	if _, err := os.Lstat(stage); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("incomplete stage remains after enumeration: %v", err)
	}
}

// TestListAppsRejectsCompoundJournalSuffix proves an unexpected directory
// cannot masquerade as a known swap journal and disappear from enumeration.
func TestListAppsRejectsCompoundJournalSuffix(t *testing.T) {
	ks := newTestKeystore(t)
	unexpected := filepath.Join(
		ks.Dir(),
		"github-app",
		strconv.FormatInt(testOwnerID, 10)+".old.staging",
	)
	if err := os.MkdirAll(unexpected, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ks.ListApps(); !errors.Is(err, publish.ErrCredentialPermissions) {
		t.Fatalf("ListApps with compound journal suffix = %v, want ErrCredentialPermissions", err)
	}
}

// TestLoadAppAbsentOwner distinguishes a missing owner binding from an
// entirely unauthenticated keystore.
func TestLoadAppAbsentOwner(t *testing.T) {
	ks := newTestKeystore(t)
	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatal(err)
	}
	_, err := ks.LoadApp(999999)
	if !errors.Is(err, publish.ErrNoAppRegistration) {
		t.Fatalf("LoadApp absent owner = %v, want ErrNoAppRegistration", err)
	}
	if errors.Is(err, publish.ErrNoAppCredentials) {
		t.Fatal("absent-owner error aliases the empty-keystore error")
	}
}

// TestMigrateLegacyApp requires explicit attribution before relocating the
// singleton layout, and leaves no silently inferred owner behind.
func TestMigrateLegacyApp(t *testing.T) {
	ks := newTestKeystore(t)
	creds := testCredentials()
	registrationRoot := writeLegacyLayout(t, ks, creds)

	if _, err := ks.LoadApp(creds.OwnerID); !errors.Is(err, publish.ErrLegacyAppMigrationRequired) {
		t.Errorf("LoadApp legacy layout = %v, want ErrLegacyAppMigrationRequired", err)
	}
	if _, err := ks.ListApps(); !errors.Is(err, publish.ErrLegacyAppMigrationRequired) {
		t.Errorf("ListApps legacy layout = %v, want ErrLegacyAppMigrationRequired", err)
	}
	if _, err := ks.MigrateLegacyApp("", 0, publish.AppVisibilityPrivate); err == nil {
		t.Fatal("MigrateLegacyApp without owner succeeded")
	}
	if _, err := os.Stat(filepath.Join(registrationRoot, keyFileNameForTest)); err != nil {
		t.Fatalf("rejected migration moved the legacy key: %v", err)
	}

	migrated, err := ks.MigrateLegacyApp("freeside-ai", testOwnerID, publish.AppVisibilityPrivate)
	if err != nil {
		t.Fatalf("MigrateLegacyApp: %v", err)
	}
	if migrated.Owner != "freeside-ai" || migrated.Visibility != publish.AppVisibilityPrivate {
		t.Errorf("migrated attribution = owner %q visibility %q", migrated.Owner, migrated.Visibility)
	}
	if _, err := os.Stat(filepath.Join(registrationRoot, keyFileNameForTest)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("legacy key remains after migration: %v", err)
	}
	if _, err := ks.LoadApp(testOwnerID); err != nil {
		t.Errorf("LoadApp after migration: %v", err)
	}
}

// TestMigrateLegacyAppResumesJournaledState refutes credential loss if the
// daemon stops after atomically moving the singleton aside but before writing
// the owner-keyed replacement.
func TestMigrateLegacyAppResumesJournaledState(t *testing.T) {
	ks := newTestKeystore(t)
	creds := testCredentials()
	registrationRoot := writeLegacyLayout(t, ks, creds)
	if err := os.Rename(registrationRoot, registrationRoot+".legacy"); err != nil {
		t.Fatal(err)
	}

	migrated, err := ks.MigrateLegacyApp(creds.Owner, creds.OwnerID, creds.Visibility)
	if err != nil {
		t.Fatalf("MigrateLegacyApp from journal: %v", err)
	}
	if migrated.AppID != creds.AppID {
		t.Errorf("migrated AppID = %d, want %d", migrated.AppID, creds.AppID)
	}
	if _, err := ks.LoadApp(creds.OwnerID); err != nil {
		t.Errorf("LoadApp after resumed migration: %v", err)
	}
	if _, err := os.Lstat(registrationRoot + ".legacy"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("legacy journal remains after migration: %v", err)
	}
}

// TestMigrateLegacyAppRecoversSingletonSwapJournals covers upgrades that
// begin while the former singleton SaveApp is between its journaled rename
// steps. Both recoverable states must require explicit attribution and keep
// the only key reachable through migration.
func TestMigrateLegacyAppRecoversSingletonSwapJournals(t *testing.T) {
	for _, suffix := range []string{".old", ".staging"} {
		t.Run(strings.TrimPrefix(suffix, "."), func(t *testing.T) {
			ks := newTestKeystore(t)
			creds := testCredentials()
			registrationRoot := writeLegacyLayout(t, ks, creds)
			if err := os.Rename(registrationRoot, registrationRoot+suffix); err != nil {
				t.Fatal(err)
			}
			if suffix == ".old" {
				if err := os.Mkdir(registrationRoot+".staging", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					filepath.Join(registrationRoot+".staging", keyFileNameForTest),
					[]byte("incomplete"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
			}

			if err := ks.SaveApp(creds); !errors.Is(err, publish.ErrLegacyAppMigrationRequired) {
				t.Fatalf("SaveApp with legacy %s journal = %v, want ErrLegacyAppMigrationRequired", suffix, err)
			}
			if _, err := os.Lstat(testAppDir(ks, creds.OwnerID)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("SaveApp created an owner registration before migration: %v", err)
			}
			if _, err := ks.LoadApp(creds.OwnerID); !errors.Is(err, publish.ErrLegacyAppMigrationRequired) {
				t.Fatalf("LoadApp with legacy %s journal = %v, want ErrLegacyAppMigrationRequired", suffix, err)
			}

			migrated, err := ks.MigrateLegacyApp(creds.Owner, creds.OwnerID, creds.Visibility)
			if err != nil {
				t.Fatalf("MigrateLegacyApp from %s journal: %v", suffix, err)
			}
			if migrated.AppID != creds.AppID {
				t.Errorf("migrated AppID = %d, want %d", migrated.AppID, creds.AppID)
			}
			if _, err := ks.LoadApp(creds.OwnerID); err != nil {
				t.Errorf("LoadApp after %s migration: %v", suffix, err)
			}
			for _, journalSuffix := range []string{".legacy", ".old", ".staging"} {
				if _, err := os.Lstat(registrationRoot + journalSuffix); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("legacy %s journal remains after migration: %v", journalSuffix, err)
				}
			}
		})
	}
}

// TestMigrateLegacyAppDiscardsIncompleteSingletonStage preserves the former
// SaveApp recovery rule: a first-save stage that never held a complete
// credential was never active and must not wedge the upgraded keystore.
func TestMigrateLegacyAppDiscardsIncompleteSingletonStage(t *testing.T) {
	ks := newTestKeystore(t)
	creds := testCredentials()
	registrationRoot := writeLegacyLayout(t, ks, creds)
	stage := registrationRoot + ".staging"
	if err := os.Rename(registrationRoot, stage); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(stage, metaFileNameForTest)); err != nil {
		t.Fatal(err)
	}

	if _, err := ks.MigrateLegacyApp(creds.Owner, creds.OwnerID, creds.Visibility); !errors.Is(err, publish.ErrNoAppCredentials) {
		t.Fatalf("MigrateLegacyApp from incomplete stage = %v, want ErrNoAppCredentials", err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("incomplete legacy stage remains after migration: %v", err)
	}
	if apps, err := ks.ListApps(); err != nil || len(apps) != 0 {
		t.Errorf("ListApps after discarded legacy stage = (%d, %v), want empty", len(apps), err)
	}
	if err := ks.SaveApp(creds); err != nil {
		t.Fatalf("SaveApp after discarded legacy stage: %v", err)
	}
}

// TestMigrateLegacyAppPrefersActiveSingleton proves a completed old-layout
// replacement remains authoritative when the daemon stopped before removing
// its previous-version journal.
func TestMigrateLegacyAppPrefersActiveSingleton(t *testing.T) {
	ks := newTestKeystore(t)
	active := testCredentials()
	registrationRoot := writeLegacyLayout(t, ks, active)

	previousKS := newTestKeystore(t)
	previous := testCredentials()
	previous.AppID = 999
	previousRoot := writeLegacyLayout(t, previousKS, previous)
	if err := os.Rename(previousRoot, registrationRoot+".old"); err != nil {
		t.Fatal(err)
	}

	migrated, err := ks.MigrateLegacyApp(active.Owner, active.OwnerID, active.Visibility)
	if err != nil {
		t.Fatalf("MigrateLegacyApp with old journal: %v", err)
	}
	if migrated.AppID != active.AppID {
		t.Errorf("migrated AppID = %d, want active %d", migrated.AppID, active.AppID)
	}
	if _, err := os.Lstat(registrationRoot + ".old"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("old singleton journal remains after migration: %v", err)
	}
}

// TestMigrateLegacyAppResumeRejectsChangedAttribution simulates a stop after
// the owner-keyed replacement is durable but before the legacy journal is
// removed. A retry cannot duplicate the credential under a new owner or
// change its visibility.
func TestMigrateLegacyAppResumeRejectsChangedAttribution(t *testing.T) {
	ks := newTestKeystore(t)
	creds := testCredentials()
	registrationRoot := writeLegacyLayout(t, ks, creds)
	if err := os.Rename(registrationRoot, registrationRoot+".legacy"); err != nil {
		t.Fatal(err)
	}

	stagedKS := newTestKeystore(t)
	if err := stagedKS.SaveApp(creds); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(registrationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(
		testAppDir(stagedKS, creds.OwnerID),
		testAppDir(ks, creds.OwnerID),
	); err != nil {
		t.Fatal(err)
	}

	if _, err := ks.MigrateLegacyApp("wrong-owner", 999999, publish.AppVisibilityPublic); err == nil {
		t.Fatal("resumed migration accepted changed attribution")
	}
	if _, err := os.Lstat(testAppDir(ks, 999999)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("changed-attribution retry created a second registration: %v", err)
	}
	if _, err := os.Lstat(registrationRoot + ".legacy"); err != nil {
		t.Fatalf("conflicting retry removed the legacy journal: %v", err)
	}

	migrated, err := ks.MigrateLegacyApp(creds.Owner, creds.OwnerID, creds.Visibility)
	if err != nil {
		t.Fatalf("MigrateLegacyApp with original attribution: %v", err)
	}
	if migrated.Owner != creds.Owner || migrated.Visibility != creds.Visibility {
		t.Errorf("resumed attribution = owner %q visibility %q", migrated.Owner, migrated.Visibility)
	}
	if _, err := os.Lstat(registrationRoot + ".legacy"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("legacy journal remains after matching retry: %v", err)
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

	appDir := testAppDir(ks, testCredentials().OwnerID)
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
	if _, err := ks.LoadApp(testCredentials().OwnerID); !errors.Is(err, publish.ErrCredentialPermissions) {
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
	appDir := testAppDir(ks, testCredentials().OwnerID)
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
	if _, err := ks.LoadApp(testCredentials().OwnerID); err != nil {
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
	if _, err := ks.LoadApp(testCredentials().OwnerID); err == nil {
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

// TestKeystoreRejectsSymlinkedOwnerDir covers the new owner-keyed path
// boundary: a planted owner child cannot relocate that registration's key.
func TestKeystoreRejectsSymlinkedOwnerDir(t *testing.T) {
	base := t.TempDir()
	credRoot := filepath.Join(base, "credentials")
	stateDir := filepath.Join(base, "state")
	evil := filepath.Join(stateDir, "evil")
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	registrationRoot := filepath.Join(credRoot, "github-app")
	if err := os.MkdirAll(registrationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		evil,
		filepath.Join(registrationRoot, strconv.FormatInt(testCredentials().OwnerID, 10)),
	); err != nil {
		t.Fatal(err)
	}

	ks, err := publish.NewKeystore(credRoot, stateDir)
	if err != nil {
		t.Fatalf("NewKeystore: %v", err)
	}
	if err := ks.SaveApp(testCredentials()); !errors.Is(err, publish.ErrCredentialPermissions) {
		t.Errorf("SaveApp through symlinked owner dir = %v, want ErrCredentialPermissions", err)
	}
	if _, err := ks.LoadApp(testCredentials().OwnerID); !errors.Is(err, publish.ErrCredentialPermissions) {
		t.Errorf("LoadApp through symlinked owner dir = %v, want ErrCredentialPermissions", err)
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
	noOwnerID := testCredentials()
	noOwnerID.OwnerID = 0
	if err := ks.SaveApp(noOwnerID); err == nil {
		t.Error("SaveApp without owner id succeeded, want error")
	}
	if _, err := ks.LoadApp(testCredentials().OwnerID); !errors.Is(err, publish.ErrNoAppRegistration) {
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
	metaPath := filepath.Join(testAppDir(ks, testCredentials().OwnerID), "app.json")
	meta, err := os.ReadFile(metaPath) //nolint:gosec // test-internal path rooted in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	meta = []byte(strings.Replace(string(meta), `"app_id": 12345`, `"app_id": 0`, 1))
	if err := os.WriteFile(metaPath, meta, 0o600); err != nil { //nolint:gosec // test-internal path rooted in t.TempDir
		t.Fatal(err)
	}
	if _, err := ks.LoadApp(testCredentials().OwnerID); err == nil {
		t.Error("LoadApp with app id 0 succeeded, want error")
	}
}

// TestLoadAppRejectsMismatchedKeyID proves restored metadata cannot identify
// one key for revocation while the keystore actually signs with another.
func TestLoadAppRejectsMismatchedKeyID(t *testing.T) {
	ks := newTestKeystore(t)
	creds := testCredentials()
	if err := ks.SaveApp(creds); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(testAppDir(ks, creds.OwnerID), metaFileNameForTest)
	meta, err := os.ReadFile(metaPath) //nolint:gosec // test-internal path rooted in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	meta = []byte(strings.Replace(string(meta), `"key_id": "SHA256:`, `"key_id": "SHA256:tampered`, 1))
	if err := os.WriteFile(metaPath, meta, 0o600); err != nil { //nolint:gosec // test-internal path rooted in t.TempDir
		t.Fatal(err)
	}
	if _, err := ks.LoadApp(creds.OwnerID); err == nil {
		t.Fatal("LoadApp with mismatched key id succeeded")
	}
}

// TestLoadAppRejectsMismatchedOwnerID proves the stable numeric directory
// binding cannot be replaced by mutable or login-reused owner metadata.
func TestLoadAppRejectsMismatchedOwnerID(t *testing.T) {
	ks := newTestKeystore(t)
	creds := testCredentials()
	if err := ks.SaveApp(creds); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(testAppDir(ks, creds.OwnerID), metaFileNameForTest)
	meta, err := os.ReadFile(metaPath) //nolint:gosec // test-internal path rooted in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	meta = []byte(strings.Replace(
		string(meta),
		`"owner_id": 24680`,
		`"owner_id": 24681`,
		1,
	))
	if err := os.WriteFile(metaPath, meta, 0o600); err != nil { //nolint:gosec // test-internal path rooted in t.TempDir
		t.Fatal(err)
	}
	if _, err := ks.LoadApp(creds.OwnerID); err == nil {
		t.Fatal("LoadApp with owner ID mismatched to its directory succeeded")
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

	// Make staging impossible: the read-only registration root refuses new entries,
	// and SaveApp only strips group/other bits so it won't re-widen it.
	registrationRoot := filepath.Join(ks.Dir(), "github-app")
	if err := os.Chmod(registrationRoot, 0o500); err != nil { //nolint:gosec // deliberately makes staging fail to prove old creds survive
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(registrationRoot, 0o700) }) //nolint:gosec // restore so t.TempDir cleanup can remove it

	replacement := testCredentials()
	replacement.AppID = 99999
	if err := ks.SaveApp(replacement); err == nil {
		t.Fatal("SaveApp with unwritable root succeeded, want error")
	}

	if err := os.Chmod(registrationRoot, 0o700); err != nil { //nolint:gosec // re-widen to read the preserved credentials
		t.Fatal(err)
	}
	got, err := ks.LoadApp(want.OwnerID)
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
	appDir := testAppDir(ks, testCredentials().OwnerID)
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
	if _, err := ks.LoadApp(testCredentials().OwnerID); err != nil {
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
	appDir := testAppDir(ks, testCredentials().OwnerID)
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
	got, err := ks.LoadApp(replacement.OwnerID)
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
	appDir := testAppDir(ks, testCredentials().OwnerID)
	if err := os.Rename(appDir, appDir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(appDir+".staging", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir+".staging", "app.pem"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ks.LoadApp(want.OwnerID)
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
	appDir := testAppDir(ks, testCredentials().OwnerID)
	if err := os.Rename(appDir, appDir+".staging"); err != nil {
		t.Fatal(err)
	}

	got, err := ks.LoadApp(want.OwnerID)
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
	staging := testAppDir(ks, testCredentials().OwnerID) + ".staging"
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "app.pem"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatalf("SaveApp over incomplete initial stage: %v", err)
	}
	if _, err := ks.LoadApp(testCredentials().OwnerID); err != nil {
		t.Fatalf("LoadApp after replacing incomplete initial stage: %v", err)
	}
}

// TestLoadAppClearsIncompleteInitialStage converges a crash before the
// first stage became complete back to the ordinary unauthenticated
// state, so the daemon can ask for re-registration without manual
// filesystem repair.
func TestLoadAppClearsIncompleteInitialStage(t *testing.T) {
	ks := newTestKeystore(t)
	staging := testAppDir(ks, testCredentials().OwnerID) + ".staging"
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "app.pem"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ks.LoadApp(testCredentials().OwnerID); !errors.Is(err, publish.ErrNoAppRegistration) {
		t.Errorf("LoadApp with incomplete initial stage = %v, want ErrNoAppRegistration", err)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("incomplete staging directory survived load")
	}
}

// TestLoadAppEmpty covers the pre-registration (and post-restore) state.
func TestLoadAppEmpty(t *testing.T) {
	ks := newTestKeystore(t)
	if _, err := ks.LoadApp(testCredentials().OwnerID); !errors.Is(err, publish.ErrNoAppRegistration) {
		t.Errorf("LoadApp on empty keystore = %v, want ErrNoAppRegistration", err)
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

func testAppDir(ks *publish.Keystore, ownerID int64) string {
	return filepath.Join(ks.Dir(), "github-app", strconv.FormatInt(ownerID, 10))
}

func writeLegacyLayout(t *testing.T, ks *publish.Keystore, creds publish.AppCredentials) string {
	t.Helper()
	if err := ks.SaveApp(creds); err != nil {
		t.Fatal(err)
	}
	appDir := testAppDir(ks, creds.OwnerID)
	registrationRoot := filepath.Join(ks.Dir(), "github-app")
	metaRaw, err := os.ReadFile(filepath.Join(appDir, metaFileNameForTest)) //nolint:gosec // test fixture under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"owner", "owner_id", "visibility", "key_id", "name"} {
		delete(meta, field)
	}
	legacyMeta, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(appDir, keyFileNameForTest), filepath.Join(registrationRoot, keyFileNameForTest)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registrationRoot, metaFileNameForTest), legacyMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(appDir); err != nil {
		t.Fatal(err)
	}
	return registrationRoot
}

const (
	keyFileNameForTest  = "app.pem"
	metaFileNameForTest = "app.json"
	testOwnerID         = int64(24680)
)
