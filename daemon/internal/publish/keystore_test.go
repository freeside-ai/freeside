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
	"slices"
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

func TestPendingAuthorityCredentialsFailClosedUntilFinalized(t *testing.T) {
	ks := newTestKeystore(t)
	creds := testCredentials()
	if err := ks.SaveAppPendingAuthority(creds); err != nil {
		t.Fatalf("SaveAppPendingAuthority: %v", err)
	}
	if _, err := ks.LoadApp(creds.OwnerID); !errors.Is(err, publish.ErrPendingAppAuthority) {
		t.Fatalf("LoadApp pending credentials = %v, want ErrPendingAppAuthority", err)
	}
	if _, err := ks.ListApps(); !errors.Is(err, publish.ErrPendingAppAuthority) {
		t.Fatalf("ListApps pending credentials = %v, want ErrPendingAppAuthority", err)
	}
	pending, err := ks.ListAppsIncludingPendingAuthority()
	if err != nil || len(pending) != 1 || !pending[0].AuthorityPending {
		t.Fatalf("setup enumeration = (%+v, %v), want one pending registration", pending, err)
	}
	wrong := creds.Registration()
	wrong.AppID++
	if err := ks.FinalizeAppAuthority(wrong); err == nil {
		t.Fatal("FinalizeAppAuthority accepted a changed canonical registration")
	}
	if _, err := ks.ListApps(); !errors.Is(err, publish.ErrPendingAppAuthority) {
		t.Fatalf("changed-identity finalization cleared marker: %v", err)
	}
	if err := ks.FinalizeAppAuthority(creds.Registration()); err != nil {
		t.Fatalf("FinalizeAppAuthority: %v", err)
	}
	apps, err := ks.ListApps()
	if err != nil || len(apps) != 1 || apps[0].AuthorityPending {
		t.Fatalf("finalized enumeration = (%+v, %v), want one usable registration", apps, err)
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

// TestListAppsEntryPolicy pins the split enumeration applies to a keystore
// entry (#284): the name decides whether an entry could be a registration at
// all, and only then does the kind decide whether it is a valid one. An
// operating system rewrites .DS_Store into any directory it displays, so
// failing closed on it let browsing the credentials directory deny every
// registration; an entry that occupies a registration's name, or a directory
// nobody can explain, still fails closed.
func TestListAppsEntryPolicy(t *testing.T) {
	personal, org := twoRegistrations()
	strayOwner := strconv.FormatInt(org.OwnerID+1, 10)

	writeFile := func(name string) func(*testing.T, string) {
		return func(t *testing.T, root string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, name), []byte("stray"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	makeDir := func(name string) func(*testing.T, string) {
		return func(t *testing.T, root string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	linkToRegistration := func(name string) func(*testing.T, string) {
		return func(t *testing.T, root string) {
			t.Helper()
			target := filepath.Join(root, strconv.FormatInt(personal.OwnerID, 10))
			if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
		}
	}

	cases := []struct {
		name  string
		plant func(*testing.T, string)
		want  error // nil: the registrations enumerate around the entry
	}{
		{name: "finder artifact", plant: writeFile(".DS_Store")},
		{name: "resource fork", plant: writeFile("._app.json")},
		{name: "undotted stray file", plant: writeFile("readme")},
		{name: "file at a registration name", plant: writeFile(strayOwner), want: publish.ErrCredentialPermissions},
		{name: "file at a staging journal name", plant: writeFile(strayOwner + ".staging"), want: publish.ErrCredentialPermissions},
		{name: "file at an old journal name", plant: writeFile(strayOwner + ".old"), want: publish.ErrCredentialPermissions},
		{name: "symlink at a registration name", plant: linkToRegistration(strayOwner), want: publish.ErrCredentialPermissions},
		{name: "symlink at an impossible name", plant: linkToRegistration(".DS_Store"), want: publish.ErrCredentialPermissions},
		{name: "directory at an impossible name", plant: makeDir("notes"), want: publish.ErrCredentialPermissions},
		{name: "non-canonical numeric directory", plant: makeDir("0" + strayOwner), want: publish.ErrCredentialPermissions},
		{name: "signed numeric directory", plant: makeDir("+" + strayOwner), want: publish.ErrCredentialPermissions},
		{name: "zero owner directory", plant: makeDir("0"), want: publish.ErrCredentialPermissions},
		// The legacy singleton's own file names are never skippable, so the
		// gate that precedes enumeration still sees them.
		{name: "legacy key file", plant: writeFile("app.pem"), want: publish.ErrLegacyAppMigrationRequired},
		{name: "legacy metadata file", plant: writeFile("app.json"), want: publish.ErrLegacyAppMigrationRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ks := newTestKeystore(t)
			saveAll(t, ks, personal, org)
			tc.plant(t, filepath.Join(ks.Dir(), "github-app"))

			apps, err := ks.ListApps()
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("ListApps = %v, want %v", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListApps: %v", err)
			}
			got := make([]int64, 0, len(apps))
			for _, app := range apps {
				got = append(got, app.OwnerID)
			}
			want := []int64{personal.OwnerID, org.OwnerID}
			if !slices.Equal(got, want) {
				t.Errorf("ListApps owner IDs = %v, want %v", got, want)
			}
		})
	}
}

// TestUnexpectedEntries proves a skipped entry stays visible to a human
// rather than being silently swallowed.
func TestUnexpectedEntries(t *testing.T) {
	ks := newTestKeystore(t)
	if entries, err := ks.UnexpectedEntries(); err != nil || entries != nil {
		t.Fatalf("UnexpectedEntries on an unpopulated keystore = %v, %v, want nil, nil", entries, err)
	}

	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)
	root := filepath.Join(ks.Dir(), "github-app")
	for _, name := range []string{".DS_Store", "readme"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("stray"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Everything enumeration refuses to skip must stay out of the report, or
	// the two disagree about what was passed over: the legacy singleton's file
	// names in either spelling, and any kind an operating system does not write
	// on its own.
	for _, name := range []string{"app.pem", "APP.JSON"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("legacy"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "readme"), filepath.Join(root, "readme.link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}

	entries, err := ks.UnexpectedEntries()
	if err != nil {
		t.Fatalf("UnexpectedEntries: %v", err)
	}
	if want := []string{".DS_Store", "readme"}; !slices.Equal(entries, want) {
		t.Errorf("UnexpectedEntries = %v, want %v", entries, want)
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

// TestMigrateLegacyAppRefusesStrandedSingletonFile keeps the enumeration skip
// (#284) out of the migration path. Migration enumerates the registration root
// directly, without the legacy gate ListApps runs first, so a legacy credential
// file stranded there must still fail closed: completing the migration around
// it would leave a private key in a root the daemon then treats as active.
// The spellings are folded because the reference platform's filesystem is:
// lstat("app.pem") resolves a file named APP.PEM, so a case-sensitive skip
// would pass over an entry the legacy gate still counts as a singleton.
func TestMigrateLegacyAppRefusesStrandedSingletonFile(t *testing.T) {
	for _, name := range []string{
		keyFileNameForTest, metaFileNameForTest,
		strings.ToUpper(keyFileNameForTest), strings.ToUpper(metaFileNameForTest),
	} {
		t.Run(name, func(t *testing.T) {
			ks := newTestKeystore(t)
			creds := testCredentials()
			registrationRoot := writeLegacyLayout(t, ks, creds)
			if err := os.Rename(registrationRoot, registrationRoot+".legacy"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(registrationRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			stranded := filepath.Join(registrationRoot, name)
			if err := os.WriteFile(stranded, []byte("stranded credential"), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := ks.MigrateLegacyApp(creds.Owner, creds.OwnerID, creds.Visibility)
			if !errors.Is(err, publish.ErrCredentialPermissions) {
				t.Fatalf("MigrateLegacyApp over a stranded %s = %v, want ErrCredentialPermissions", name, err)
			}
			if _, err := os.Lstat(stranded); err != nil {
				t.Errorf("stranded legacy credential disturbed by the refused migration: %v", err)
			}
			if _, err := os.Lstat(registrationRoot + ".legacy"); err != nil {
				t.Errorf("legacy journal cleared by the refused migration: %v", err)
			}
		})
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

func TestPendingAuthorityPreCredentialStageIsNeverPromoted(t *testing.T) {
	ks := newTestKeystore(t)
	appDir := testAppDir(ks, testCredentials().OwnerID)
	staging := appDir + ".staging"
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "authority.pending"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	apps, err := ks.ListAppsIncludingPendingAuthority()
	if err != nil {
		t.Fatalf("ListAppsIncludingPendingAuthority: %v", err)
	}
	if len(apps) != 0 {
		t.Fatalf("marker-only stage promoted as credentials: %+v", apps)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("marker-only stage survived recovery: %v", err)
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

// stripOwnerMetadata reproduces the pre-#245 record shape found on the
// maintainer's machine (#271): an owner-keyed directory whose metadata
// carries the app id and slug but null owner, owner id, visibility, key
// id, and name.
func stripOwnerMetadata(t *testing.T, ks *publish.Keystore, ownerID int64) {
	t.Helper()
	metaPath := filepath.Join(testAppDir(ks, ownerID), metaFileNameForTest)
	metaRaw, err := os.ReadFile(metaPath) //nolint:gosec // test fixture under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"owner", "owner_id", "visibility", "key_id", "name"} {
		meta[field] = nil
	}
	stripped, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, stripped, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestListAppsNamesTheUnreadableRegistration is #271's core property: one
// record with incomplete metadata still fails enumeration closed, but the
// failure now names that record instead of reading as a broken keystore.
func TestListAppsNamesTheUnreadableRegistration(t *testing.T) {
	ks := newTestKeystore(t)
	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)
	stripOwnerMetadata(t, ks, org.OwnerID)

	apps, err := ks.ListApps()
	if err == nil {
		t.Fatalf("ListApps returned %d registrations over an incomplete record, want error", len(apps))
	}
	if !errors.Is(err, publish.ErrUnreadableRegistration) {
		t.Fatalf("ListApps error = %v, want ErrUnreadableRegistration", err)
	}
	var unreadable *publish.UnreadableRegistrationError
	if !errors.As(err, &unreadable) {
		t.Fatalf("ListApps error %v does not carry *UnreadableRegistrationError", err)
	}
	if unreadable.OwnerID != org.OwnerID {
		t.Errorf("unreadable owner id = %d, want %d", unreadable.OwnerID, org.OwnerID)
	}
	if !strings.Contains(err.Error(), strconv.FormatInt(org.OwnerID, 10)) {
		t.Errorf("ListApps error %q does not identify owner ID %d", err, org.OwnerID)
	}
}

// TestQuarantineAppRestoresEnumeration proves the operator's withdrawal is
// what re-opens resolution, and that it preserves rather than deletes the
// record.
func TestQuarantineAppRestoresEnumeration(t *testing.T) {
	ks := newTestKeystore(t)
	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)
	stripOwnerMetadata(t, ks, org.OwnerID)

	if err := ks.QuarantineApp(org.OwnerID); err != nil {
		t.Fatalf("QuarantineApp: %v", err)
	}
	apps, err := ks.ListApps()
	if err != nil {
		t.Fatalf("ListApps after quarantine: %v", err)
	}
	if len(apps) != 1 || apps[0].OwnerID != personal.OwnerID {
		t.Fatalf("ListApps after quarantine = %+v, want only owner %d", apps, personal.OwnerID)
	}
	quarantined := filepath.Join(
		ks.Dir(), "github-app"+".quarantine", strconv.FormatInt(org.OwnerID, 10),
	)
	for _, name := range []string{keyFileNameForTest, metaFileNameForTest} {
		if _, err := os.Lstat(filepath.Join(quarantined, name)); err != nil {
			t.Errorf("quarantined %s: %v", name, err)
		}
	}
	if _, err := os.Lstat(testAppDir(ks, org.OwnerID)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("active registration remains after quarantine: %v", err)
	}
}

// TestQuarantineAppRefusesReadableRegistration keeps the withdrawal from
// becoming a way to drop a working registration out of janitor coverage
// while its installations stay live on GitHub.
func TestQuarantineAppRefusesReadableRegistration(t *testing.T) {
	ks := newTestKeystore(t)
	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)

	if err := ks.QuarantineApp(org.OwnerID); err == nil {
		t.Fatal("QuarantineApp over a readable registration = nil, want refusal")
	}
	apps, err := ks.ListApps()
	if err != nil {
		t.Fatalf("ListApps after refused quarantine: %v", err)
	}
	if len(apps) != 2 {
		t.Errorf("ListApps after refused quarantine returned %d registrations, want 2", len(apps))
	}
}

// TestQuarantineAppRefusesOverwrite keeps a second withdrawal from
// destroying the first record it preserved.
func TestQuarantineAppRefusesOverwrite(t *testing.T) {
	ks := newTestKeystore(t)
	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)
	stripOwnerMetadata(t, ks, org.OwnerID)
	if err := ks.QuarantineApp(org.OwnerID); err != nil {
		t.Fatalf("QuarantineApp: %v", err)
	}

	reSaved := org
	if err := ks.SaveApp(reSaved); err != nil {
		t.Fatal(err)
	}
	stripOwnerMetadata(t, ks, org.OwnerID)
	if err := ks.QuarantineApp(org.OwnerID); err == nil {
		t.Fatal("second QuarantineApp = nil, want refusal rather than overwrite")
	}
}

// TestQuarantineAppWithdrawsSwapLeftovers proves a .staging sibling cannot
// resurrect a withdrawn record on the next enumeration.
func TestQuarantineAppWithdrawsSwapLeftovers(t *testing.T) {
	ks := newTestKeystore(t)
	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)
	stripOwnerMetadata(t, ks, org.OwnerID)
	leftover := testAppDir(ks, org.OwnerID) + ".old"
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{keyFileNameForTest, metaFileNameForTest} {
		source, err := os.ReadFile(filepath.Join(testAppDir(ks, org.OwnerID), name)) //nolint:gosec // test fixture under t.TempDir
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(leftover, name), source, 0o600); err != nil { //nolint:gosec // test fixture under t.TempDir
			t.Fatal(err)
		}
	}

	if err := ks.QuarantineApp(org.OwnerID); err != nil {
		t.Fatalf("QuarantineApp: %v", err)
	}
	if _, err := os.Lstat(leftover); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("swap leftover remains in the active set after quarantine: %v", err)
	}
	apps, err := ks.ListApps()
	if err != nil {
		t.Fatalf("ListApps after quarantine: %v", err)
	}
	if len(apps) != 1 || apps[0].OwnerID != personal.OwnerID {
		t.Fatalf("ListApps after quarantine = %+v, want only owner %d", apps, personal.OwnerID)
	}
}

// TestQuarantineAppAbsentOwner keeps the withdrawal from inventing a record.
func TestQuarantineAppAbsentOwner(t *testing.T) {
	ks := newTestKeystore(t)
	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatal(err)
	}
	if err := ks.QuarantineApp(999999); !errors.Is(err, publish.ErrNoAppRegistration) {
		t.Fatalf("QuarantineApp(absent) = %v, want ErrNoAppRegistration", err)
	}
}

func twoRegistrations() (personal, org publish.AppCredentials) {
	personal = testCredentials()
	personal.Owner = "BenNelsonWeiss"
	personal.OwnerID = 111
	personal.AppID = 111
	org = testCredentials()
	org.Owner = "freeside-ai"
	org.OwnerID = 222
	org.AppID = 222
	return personal, org
}

func saveAll(t *testing.T, ks *publish.Keystore, creds ...publish.AppCredentials) {
	t.Helper()
	for _, c := range creds {
		if err := ks.SaveApp(c); err != nil {
			t.Fatalf("SaveApp(%s): %v", c.Owner, err)
		}
	}
}

// rewriteOwnerID makes a record internally valid but bound to a different
// owner than its directory key: readable to loadAppFrom, rejected by
// enumeration. This is the state that let the doctor and the quarantine
// remedy disagree.
func rewriteOwnerID(t *testing.T, ks *publish.Keystore, dirOwnerID, persistedOwnerID int64) {
	t.Helper()
	metaPath := filepath.Join(testAppDir(ks, dirOwnerID), metaFileNameForTest)
	metaRaw, err := os.ReadFile(metaPath) //nolint:gosec // test fixture under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatal(err)
	}
	meta["owner_id"] = persistedOwnerID
	rewritten, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestQuarantineWithdrawsEveryEnumerationBlocker is the refute-first
// harness for the withdrawal path: it enumerates the record states that
// block ListApps and requires each to be both attributed to its owner and
// withdrawable. A state that enumeration rejects but quarantine refuses
// would leave resolution blocked with no remedy.
func TestQuarantineWithdrawsEveryEnumerationBlocker(t *testing.T) {
	cases := []struct {
		name   string
		damage func(t *testing.T, ks *publish.Keystore, ownerID int64)
	}{
		{"incomplete metadata", func(t *testing.T, ks *publish.Keystore, ownerID int64) {
			stripOwnerMetadata(t, ks, ownerID)
		}},
		{"identity does not bind to directory", func(t *testing.T, ks *publish.Keystore, ownerID int64) {
			rewriteOwnerID(t, ks, ownerID, ownerID+1)
		}},
		{"absent key", func(t *testing.T, ks *publish.Keystore, ownerID int64) {
			if err := os.Remove(filepath.Join(testAppDir(ks, ownerID), keyFileNameForTest)); err != nil {
				t.Fatal(err)
			}
		}},
		{"widened key permissions", func(t *testing.T, ks *publish.Keystore, ownerID int64) {
			//nolint:gosec // deliberately exposed fixture under t.TempDir
			if err := os.Chmod(filepath.Join(testAppDir(ks, ownerID), keyFileNameForTest), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"invalid recovery journal without an active record", func(t *testing.T, ks *publish.Keystore, ownerID int64) {
			appDir := testAppDir(ks, ownerID)
			if err := os.Rename(appDir, appDir+".old"); err != nil {
				t.Fatal(err)
			}
			stripJournalMetadata(t, appDir+".old")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ks := newTestKeystore(t)
			personal, org := twoRegistrations()
			saveAll(t, ks, personal, org)
			tc.damage(t, ks, org.OwnerID)

			apps, err := ks.ListApps()
			if err == nil {
				t.Fatalf("ListApps returned %d registrations over a damaged record, want error", len(apps))
			}
			var unreadable *publish.UnreadableRegistrationError
			if !errors.As(err, &unreadable) {
				t.Fatalf("ListApps error %v does not attribute the damaged record", err)
			}
			if unreadable.OwnerID != org.OwnerID {
				t.Fatalf("attributed owner = %d, want %d", unreadable.OwnerID, org.OwnerID)
			}

			if err := ks.QuarantineApp(unreadable.OwnerID); err != nil {
				t.Fatalf("QuarantineApp(%d) after ListApps routed to it: %v", unreadable.OwnerID, err)
			}
			apps, err = ks.ListApps()
			if err != nil {
				t.Fatalf("ListApps after quarantine: %v", err)
			}
			if len(apps) != 1 || apps[0].OwnerID != personal.OwnerID {
				t.Fatalf("ListApps after quarantine = %+v, want only owner %d", apps, personal.OwnerID)
			}
		})
	}
}

// stripJournalMetadata damages a swap journal in place, so recovery cannot
// promote it and the record has no active directory at all.
func stripJournalMetadata(t *testing.T, journalDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(journalDir, metaFileNameForTest), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestQuarantineAppDiscardedStageLeavesNothingToWithdraw keeps the remedy
// honest about the one state recovery resolves on its own: an incomplete
// first-save stage is discarded, not withdrawn.
func TestQuarantineAppDiscardedStageLeavesNothingToWithdraw(t *testing.T) {
	ks := newTestKeystore(t)
	if err := ks.SaveApp(testCredentials()); err != nil {
		t.Fatal(err)
	}
	stage := testAppDir(ks, 333) + ".staging"
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, metaFileNameForTest), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ks.QuarantineApp(333); !errors.Is(err, publish.ErrNoAppRegistration) {
		t.Fatalf("QuarantineApp over a discarded stage = %v, want ErrNoAppRegistration", err)
	}
	if _, err := ks.ListApps(); err != nil {
		t.Fatalf("ListApps after discarded stage: %v", err)
	}
}

// TestQuarantineAppRefusesKeystoreWideFailure keeps a widened directory
// mode from being read as a damaged record: it fails the load gate for
// every registration alike, so withdrawing the named owner would drop a
// working registration while leaving enumeration just as blocked.
func TestQuarantineAppRefusesKeystoreWideFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		widen  func(ks *publish.Keystore) string
		damage bool
	}{
		{"widened registration root", func(ks *publish.Keystore) string {
			return filepath.Join(ks.Dir(), "github-app")
		}, false},
		{"widened credentials root", func(ks *publish.Keystore) string {
			return ks.Dir()
		}, false},
		{"widened root over a damaged record", func(ks *publish.Keystore) string {
			return filepath.Join(ks.Dir(), "github-app")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ks := newTestKeystore(t)
			personal, org := twoRegistrations()
			saveAll(t, ks, personal, org)
			if tc.damage {
				stripOwnerMetadata(t, ks, org.OwnerID)
			}
			if err := os.Chmod(tc.widen(ks), 0o755); err != nil { //nolint:gosec // deliberately exposed fixture under t.TempDir
				t.Fatal(err)
			}

			if err := ks.QuarantineApp(org.OwnerID); !errors.Is(err, publish.ErrCredentialPermissions) {
				t.Fatalf("QuarantineApp under a keystore-wide failure = %v, want ErrCredentialPermissions", err)
			}
			if _, err := os.Lstat(testAppDir(ks, org.OwnerID)); err != nil {
				t.Errorf("registration withdrawn despite a keystore-wide failure: %v", err)
			}
		})
	}
}

// TestQuarantineAppResumesPartialWithdrawal covers the partial-failure
// state the move order is designed to leave: journals already withdrawn,
// the active directory still in place. Recovery must not promote anything
// (the active directory is present), and the retry must complete rather
// than refuse on a destination its own earlier attempt created.
func TestQuarantineAppResumesPartialWithdrawal(t *testing.T) {
	ks := newTestKeystore(t)
	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)
	stripOwnerMetadata(t, ks, org.OwnerID)

	// Stand in for a run that moved the journal and then failed before the
	// active directory.
	appDir := testAppDir(ks, org.OwnerID)
	journal := appDir + ".old"
	if err := os.MkdirAll(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	quarantineDir := filepath.Join(ks.Dir(), "github-app.quarantine")
	if err := os.MkdirAll(quarantineDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(journal, filepath.Join(quarantineDir, filepath.Base(journal))); err != nil {
		t.Fatal(err)
	}

	if err := ks.QuarantineApp(org.OwnerID); err != nil {
		t.Fatalf("QuarantineApp resuming a partial withdrawal: %v", err)
	}
	apps, err := ks.ListApps()
	if err != nil {
		t.Fatalf("ListApps after resumed withdrawal: %v", err)
	}
	if len(apps) != 1 || apps[0].OwnerID != personal.OwnerID {
		t.Fatalf("ListApps after resumed withdrawal = %+v, want only owner %d", apps, personal.OwnerID)
	}
	for _, name := range []string{
		strconv.FormatInt(org.OwnerID, 10),
		strconv.FormatInt(org.OwnerID, 10) + ".old",
	} {
		if _, err := os.Lstat(filepath.Join(quarantineDir, name)); err != nil {
			t.Errorf("quarantined %s: %v", name, err)
		}
	}
}

// TestQuarantineAppMovesTheActiveRecordLast pins the ordering the
// resumption property depends on: a failure after the active directory
// moved would leave journals recovery promotes back into the active slot,
// and the retry would then collide with its own quarantined record.
func TestQuarantineAppMovesTheActiveRecordLast(t *testing.T) {
	ks := newTestKeystore(t)
	_, org := twoRegistrations()
	if err := ks.SaveApp(org); err != nil {
		t.Fatal(err)
	}
	stripOwnerMetadata(t, ks, org.OwnerID)
	appDir := testAppDir(ks, org.OwnerID)
	if err := os.MkdirAll(appDir+".old", 0o700); err != nil {
		t.Fatal(err)
	}

	// A destination collision on the last source proves ordering: the
	// occupied name is the active record's, so the journals must already
	// have been examined and the active directory reached last.
	quarantineDir := filepath.Join(ks.Dir(), "github-app.quarantine")
	if err := os.MkdirAll(filepath.Join(quarantineDir, strconv.FormatInt(org.OwnerID, 10)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ks.QuarantineApp(org.OwnerID); err == nil {
		t.Fatal("QuarantineApp onto an occupied destination = nil, want refusal")
	}
	if _, err := os.Lstat(appDir); err != nil {
		t.Errorf("active record moved despite the refusal: %v", err)
	}
	if _, err := os.Lstat(appDir + ".old"); err != nil {
		t.Errorf("journal moved despite the refusal: %v", err)
	}
}

// TestListAppsDoesNotAttributePostPromotionCleanup keeps enumeration from
// claiming a record is unreadable when recovery promoted it successfully
// and only the leftover cleanup failed. The registration loads, so
// withdrawal would refuse it; attributing the failure would route the
// operator to a remedy that cannot help while enumeration stays blocked
// on the leftover.
func TestListAppsDoesNotAttributePostPromotionCleanup(t *testing.T) {
	ks := newTestKeystore(t)
	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)

	// No active directory, a valid journal to promote, and a second journal
	// whose contents cannot be unlinked.
	appDir := testAppDir(ks, org.OwnerID)
	if err := os.Rename(appDir, appDir+".old"); err != nil {
		t.Fatal(err)
	}
	undeletable := filepath.Join(appDir+".staging", "sub")
	if err := os.MkdirAll(undeletable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(undeletable, "pinned"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(undeletable, 0o500); err != nil { //nolint:gosec // directory mode; blocks unlink to simulate an uncleanable leftover
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(undeletable, 0o700) }) //nolint:gosec // restore owner traversal so t.TempDir can clean up

	_, err := ks.ListApps()
	if err == nil {
		t.Fatal("ListApps with an uncleanable leftover = nil, want error")
	}
	var unreadable *publish.UnreadableRegistrationError
	if errors.As(err, &unreadable) {
		t.Fatalf("post-promotion cleanup failure attributed as unreadable owner %d: %v", unreadable.OwnerID, err)
	}
	// Once the leftover can be cleared, enumeration succeeds with both
	// registrations present: the failure was never a property of the record,
	// which is exactly why withdrawing it would have been the wrong remedy.
	if err := os.Chmod(undeletable, 0o700); err != nil { //nolint:gosec // directory mode, restoring owner traversal
		t.Fatal(err)
	}
	apps, err := ks.ListApps()
	if err != nil {
		t.Fatalf("ListApps once the leftover is clearable: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("ListApps returned %d registrations, want both", len(apps))
	}
}

// TestQuarantineAppCompletesAnUnsyncedWithdrawal covers the failure
// boundary after every rename has landed: a sync failure leaves nothing to
// move, and a retry must finish the durability sequence rather than report
// a record that is in fact already withdrawn.
func TestQuarantineAppCompletesAnUnsyncedWithdrawal(t *testing.T) {
	ks := newTestKeystore(t)
	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)
	stripOwnerMetadata(t, ks, org.OwnerID)

	// Stand in for a run whose renames all landed and whose sync then failed.
	quarantineDir := filepath.Join(ks.Dir(), "github-app.quarantine")
	if err := os.MkdirAll(quarantineDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ownerKey := strconv.FormatInt(org.OwnerID, 10)
	if err := os.Rename(testAppDir(ks, org.OwnerID), filepath.Join(quarantineDir, ownerKey)); err != nil {
		t.Fatal(err)
	}

	if err := ks.QuarantineApp(org.OwnerID); err != nil {
		t.Fatalf("QuarantineApp completing an unsynced withdrawal: %v", err)
	}
	apps, err := ks.ListApps()
	if err != nil {
		t.Fatalf("ListApps after the completed withdrawal: %v", err)
	}
	if len(apps) != 1 || apps[0].OwnerID != personal.OwnerID {
		t.Fatalf("ListApps = %+v, want only owner %d", apps, personal.OwnerID)
	}
	for _, name := range []string{keyFileNameForTest, metaFileNameForTest} {
		if _, err := os.Lstat(filepath.Join(quarantineDir, ownerKey, name)); err != nil {
			t.Errorf("quarantined %s: %v", name, err)
		}
	}
}

// TestQuarantineAppWithdrawsJournalsAndActiveTogether pins the shape the
// crash barrier protects: with both a journal and an active record, every
// piece leaves the active set and none is left behind on the source side
// for recovery to promote.
func TestQuarantineAppWithdrawsJournalsAndActiveTogether(t *testing.T) {
	ks := newTestKeystore(t)
	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)
	stripOwnerMetadata(t, ks, org.OwnerID)
	appDir := testAppDir(ks, org.OwnerID)
	if err := os.MkdirAll(appDir+".old", 0o700); err != nil {
		t.Fatal(err)
	}

	if err := ks.QuarantineApp(org.OwnerID); err != nil {
		t.Fatalf("QuarantineApp: %v", err)
	}
	for _, leftover := range []string{appDir, appDir + ".old", appDir + ".staging"} {
		if _, err := os.Lstat(leftover); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survives on the source side after withdrawal: %v", leftover, err)
		}
	}
	// Nothing recovery could promote: a second enumeration must not
	// resurrect the record.
	for round := range 2 {
		apps, err := ks.ListApps()
		if err != nil {
			t.Fatalf("ListApps round %d: %v", round, err)
		}
		if len(apps) != 1 || apps[0].OwnerID != personal.OwnerID {
			t.Fatalf("ListApps round %d = %+v, want only owner %d", round, apps, personal.OwnerID)
		}
	}
}

// deficientRootModes enumerates the owner-access deviations a root can
// carry, rather than the one that happened to be reported: no write, no
// search, and neither. Each disables the withdrawal for every record
// alike, so each must be classified keystore-wide.
var deficientRootModes = []struct {
	name string
	mode fs.FileMode
}{
	{"no owner write", 0o500},
	{"no owner search", 0o600},
	{"read only", 0o400},
	{"group and other bits", 0o755},
}

// TestDeficientRegistrationRootIsKeystoreWide is the mirror of the
// widened-mode case: a registration root missing any owner bit blocks
// recovery for every record alike, so it must not be attributed to
// whichever record met it, and the withdrawal must refuse rather than
// attempt a move that cannot succeed. narrowDir deliberately preserves a
// tighter-than-0700 root, so these are supported states, not corruption.
func TestDeficientRegistrationRootIsKeystoreWide(t *testing.T) {
	for _, tc := range deficientRootModes {
		t.Run(tc.name, func(t *testing.T) {
			ks := newTestKeystore(t)
			personal, org := twoRegistrations()
			saveAll(t, ks, personal, org)
			// The owner exists only as a journal, so recovery must rename it
			// into place and cannot.
			appDir := testAppDir(ks, org.OwnerID)
			if err := os.Rename(appDir, appDir+".old"); err != nil {
				t.Fatal(err)
			}
			locked := filepath.Join(ks.Dir(), "github-app")
			if err := os.Chmod(locked, tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // restore owner access

			_, err := ks.ListApps()
			if err == nil {
				t.Fatal("ListApps under a deficient registration root = nil, want error")
			}
			var unreadable *publish.UnreadableRegistrationError
			if errors.As(err, &unreadable) {
				t.Errorf("keystore-wide failure attributed to owner %d: %v", unreadable.OwnerID, err)
			}
			if err := ks.QuarantineApp(org.OwnerID); !errors.Is(err, publish.ErrCredentialPermissions) {
				t.Fatalf("QuarantineApp under a deficient root = %v, want ErrCredentialPermissions", err)
			}

			// Restoring owner access makes it an ordinary recoverable record.
			if err := os.Chmod(locked, 0o700); err != nil { //nolint:gosec // restore owner access
				t.Fatal(err)
			}
			if _, err := os.Lstat(appDir + ".old"); err != nil {
				t.Errorf("journal disturbed by the refused withdrawal: %v", err)
			}
			apps, err := ks.ListApps()
			if err != nil {
				t.Fatalf("ListApps once the root is usable: %v", err)
			}
			if len(apps) != 2 {
				t.Fatalf("ListApps returned %d registrations, want both", len(apps))
			}
		})
	}
}

// TestDeficientCredentialsRootRefusesWithdrawal covers the other root the
// withdrawal writes: enumeration is unaffected, since recovery renames
// within the registration root, but the quarantine directory is created
// beside it, so the withdrawal must refuse rather than fail part-way.
func TestDeficientCredentialsRootRefusesWithdrawal(t *testing.T) {
	for _, tc := range deficientRootModes {
		t.Run(tc.name, func(t *testing.T) {
			ks := newTestKeystore(t)
			personal, org := twoRegistrations()
			saveAll(t, ks, personal, org)
			stripOwnerMetadata(t, ks, org.OwnerID)
			if err := os.Chmod(ks.Dir(), tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(ks.Dir(), 0o700) }) //nolint:gosec // restore owner access

			if err := ks.QuarantineApp(org.OwnerID); !errors.Is(err, publish.ErrCredentialPermissions) {
				t.Fatalf("QuarantineApp under a deficient credentials root = %v, want ErrCredentialPermissions", err)
			}
			if err := os.Chmod(ks.Dir(), 0o700); err != nil { //nolint:gosec // restore owner access
				t.Fatal(err)
			}
			if _, err := os.Lstat(testAppDir(ks, org.OwnerID)); err != nil {
				t.Errorf("record disturbed by the refused withdrawal: %v", err)
			}
			if err := ks.QuarantineApp(org.OwnerID); err != nil {
				t.Fatalf("QuarantineApp once the root is usable: %v", err)
			}
			apps, err := ks.ListApps()
			if err != nil {
				t.Fatalf("ListApps after withdrawal: %v", err)
			}
			if len(apps) != 1 || apps[0].OwnerID != personal.OwnerID {
				t.Fatalf("ListApps = %+v, want only owner %d", apps, personal.OwnerID)
			}
		})
	}
}

// TestDeficientRootDoesNotAttributeActiveRecordFailure covers the other
// attribution branch: an active record that simply fails to load, with no
// recovery involved. Mode 0500 is the mode that makes the point — it
// permits the read and the traversal, so the load failure is genuine, and
// forbids the rename, so withdrawal would refuse. Attribution promises a
// remedy, so it must not be made here either.
func TestDeficientRootDoesNotAttributeActiveRecordFailure(t *testing.T) {
	ks := newTestKeystore(t)
	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)
	stripOwnerMetadata(t, ks, org.OwnerID)
	locked := filepath.Join(ks.Dir(), "github-app")
	if err := os.Chmod(locked, 0o500); err != nil { //nolint:gosec // directory mode is the state under test
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // restore owner access

	_, err := ks.ListApps()
	if err == nil {
		t.Fatal("ListApps over a damaged record under a deficient root = nil, want error")
	}
	var unreadable *publish.UnreadableRegistrationError
	if errors.As(err, &unreadable) {
		t.Errorf("attributed owner %d while withdrawal would refuse: %v", unreadable.OwnerID, err)
	}
	if err := ks.QuarantineApp(org.OwnerID); !errors.Is(err, publish.ErrCredentialPermissions) {
		t.Fatalf("QuarantineApp under a deficient root = %v, want ErrCredentialPermissions", err)
	}

	// With the root usable the promise holds again: the same failure is
	// attributed, and the remedy it advertises works.
	if err := os.Chmod(locked, 0o700); err != nil { //nolint:gosec // restore owner access
		t.Fatal(err)
	}
	_, err = ks.ListApps()
	if !errors.As(err, &unreadable) {
		t.Fatalf("ListApps once the root is usable = %v, want an attributed failure", err)
	}
	if unreadable.OwnerID != org.OwnerID {
		t.Fatalf("attributed owner = %d, want %d", unreadable.OwnerID, org.OwnerID)
	}
	if err := ks.QuarantineApp(unreadable.OwnerID); err != nil {
		t.Fatalf("QuarantineApp after attribution: %v", err)
	}
	apps, err := ks.ListApps()
	if err != nil {
		t.Fatalf("ListApps after withdrawal: %v", err)
	}
	if len(apps) != 1 || apps[0].OwnerID != personal.OwnerID {
		t.Fatalf("ListApps = %+v, want only owner %d", apps, personal.OwnerID)
	}
}

// TestDeficientQuarantineDestinationIsKeystoreWide extends the root-mode
// table to the destination: an existing quarantine directory the owner
// cannot write into fails every rename just as a deficient source root
// does, so it must not be attributed to a record either.
func TestDeficientQuarantineDestinationIsKeystoreWide(t *testing.T) {
	for _, tc := range deficientRootModes {
		t.Run(tc.name, func(t *testing.T) {
			ks := newTestKeystore(t)
			personal, org := twoRegistrations()
			saveAll(t, ks, personal, org)
			stripOwnerMetadata(t, ks, org.OwnerID)
			quarantineDir := filepath.Join(ks.Dir(), "github-app.quarantine")
			if err := os.MkdirAll(quarantineDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(quarantineDir, tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(quarantineDir, 0o700) }) //nolint:gosec // restore owner access

			_, err := ks.ListApps()
			if err == nil {
				t.Fatal("ListApps with a deficient quarantine destination = nil, want error")
			}
			var unreadable *publish.UnreadableRegistrationError
			if errors.As(err, &unreadable) {
				t.Errorf("attributed owner %d while the destination blocks withdrawal: %v", unreadable.OwnerID, err)
			}
			if err := ks.QuarantineApp(org.OwnerID); !errors.Is(err, publish.ErrCredentialPermissions) {
				t.Fatalf("QuarantineApp into a deficient destination = %v, want ErrCredentialPermissions", err)
			}

			// Restoring owner access restores both the attribution and the
			// remedy it advertises.
			if err := os.Chmod(quarantineDir, 0o700); err != nil { //nolint:gosec // restore owner access
				t.Fatal(err)
			}
			_, err = ks.ListApps()
			if !errors.As(err, &unreadable) || unreadable.OwnerID != org.OwnerID {
				t.Fatalf("ListApps once the destination is usable = %v, want owner %d attributed", err, org.OwnerID)
			}
			if err := ks.QuarantineApp(org.OwnerID); err != nil {
				t.Fatalf("QuarantineApp once the destination is usable: %v", err)
			}
			apps, err := ks.ListApps()
			if err != nil {
				t.Fatalf("ListApps after withdrawal: %v", err)
			}
			if len(apps) != 1 || apps[0].OwnerID != personal.OwnerID {
				t.Fatalf("ListApps = %+v, want only owner %d", apps, personal.OwnerID)
			}
		})
	}
}

// TestMalformedQuarantineTargetIsNotAttributed closes the last shape of
// the availability class: the destination directories are fine, but the
// owner's own target inside them is a file or a symlink, so the rename
// cannot land. Attribution promises the withdrawal, so it must ask the
// withdrawal's own preconditions rather than only the directory modes.
func TestMalformedQuarantineTargetIsNotAttributed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, target string)
	}{
		{"regular file", func(t *testing.T, target string) {
			if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, target string) {
			if err := os.Symlink(t.TempDir(), target); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ks := newTestKeystore(t)
			personal, org := twoRegistrations()
			saveAll(t, ks, personal, org)
			stripOwnerMetadata(t, ks, org.OwnerID)
			quarantineDir := filepath.Join(ks.Dir(), "github-app.quarantine")
			if err := os.MkdirAll(quarantineDir, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(quarantineDir, strconv.FormatInt(org.OwnerID, 10))
			tc.plant(t, target)

			_, err := ks.ListApps()
			if err == nil {
				t.Fatal("ListApps with a malformed quarantine target = nil, want error")
			}
			var unreadable *publish.UnreadableRegistrationError
			if errors.As(err, &unreadable) {
				t.Errorf("attributed owner %d while its destination is unusable: %v", unreadable.OwnerID, err)
			}
			if err := ks.QuarantineApp(org.OwnerID); err == nil {
				t.Fatal("QuarantineApp onto a malformed target = nil, want refusal")
			}
			if _, err := os.Lstat(testAppDir(ks, org.OwnerID)); err != nil {
				t.Errorf("record disturbed by the refused withdrawal: %v", err)
			}

			// Clearing the obstruction restores both the attribution and the
			// remedy it advertises.
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			_, err = ks.ListApps()
			if !errors.As(err, &unreadable) || unreadable.OwnerID != org.OwnerID {
				t.Fatalf("ListApps once the target is clear = %v, want owner %d attributed", err, org.OwnerID)
			}
			if err := ks.QuarantineApp(org.OwnerID); err != nil {
				t.Fatalf("QuarantineApp once the target is clear: %v", err)
			}
			apps, err := ks.ListApps()
			if err != nil {
				t.Fatalf("ListApps after withdrawal: %v", err)
			}
			if len(apps) != 1 || apps[0].OwnerID != personal.OwnerID {
				t.Fatalf("ListApps = %+v, want only owner %d", apps, personal.OwnerID)
			}
		})
	}
}

// TestExistingWithdrawalIsNotAttributed pins the other precondition
// attribution now inherits: a destination whose source still exists is a
// distinct earlier withdrawal that the remedy refuses, so it must not be
// advertised either.
func TestExistingWithdrawalIsNotAttributed(t *testing.T) {
	ks := newTestKeystore(t)
	personal, org := twoRegistrations()
	saveAll(t, ks, personal, org)
	stripOwnerMetadata(t, ks, org.OwnerID)
	quarantineDir := filepath.Join(ks.Dir(), "github-app.quarantine")
	occupied := filepath.Join(quarantineDir, strconv.FormatInt(org.OwnerID, 10))
	if err := os.MkdirAll(occupied, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := ks.ListApps()
	if err == nil {
		t.Fatal("ListApps with an occupied destination = nil, want error")
	}
	var unreadable *publish.UnreadableRegistrationError
	if errors.As(err, &unreadable) {
		t.Errorf("attributed owner %d while the withdrawal would refuse: %v", unreadable.OwnerID, err)
	}
	if err := ks.QuarantineApp(org.OwnerID); err == nil {
		t.Fatal("QuarantineApp onto an occupied destination = nil, want refusal")
	}
}
