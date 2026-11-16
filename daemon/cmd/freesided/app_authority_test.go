package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// openTestKeystore opens the keystore under an App authority writer lock it
// releases at once, so a command the test runs next can take the lock itself.
// The keystore keeps working: the lock gates opening the authority, not using
// what was opened.
func openTestKeystore(t *testing.T, stateDir, credentialsDir string) (*publish.Keystore, error) {
	t.Helper()
	lock, err := publish.AcquireAuthorityWriterLock(stateDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	return publish.NewKeystore(lock, credentialsDir)
}

// openTestAuthorityStore opens the installation authority store as
// openTestKeystore opens the keystore.
func openTestAuthorityStore(t *testing.T, stateDir string) (*publish.InstallationAuthorityStore, error) {
	t.Helper()
	lock, err := publish.AcquireAuthorityWriterLock(stateDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	return publish.NewInstallationAuthorityStore(lock)
}

// holdAppAuthority takes the writer lock on stateDir as another process
// would, for the rest of the test.
func holdAppAuthority(t *testing.T, stateDir string) {
	t.Helper()
	lock, err := publish.AcquireAuthorityWriterLock(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
}

func TestSetupRefusesWhileAnotherHolderHasTheAppAuthority(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "freeside")
	layout, err := operations.Setup(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	holdAppAuthority(t, layout.StateDir)
	var stdout, stderr bytes.Buffer
	err = runSetupCommand(t.Context(),
		[]string{"-config-dir", root, "-operator", "example", "-operator-id", "42"},
		strings.NewReader(""), &stdout, &stderr, &http.Client{},
		"https://api.github.example", "https://github.example")
	if !errors.Is(err, publish.ErrAuthorityWriterLockHeld) || !strings.Contains(err.Error(), "freesided execution pause") {
		t.Fatalf("setup under a held lock = %v, want a refusal naming freesided execution pause", err)
	}
	if _, err := os.Stat(filepath.Join(layout.StateDir, "installation-authority.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("setup wrote the authority under a held lock: %v", err)
	}
}

func TestOnboardRefusesWhileAnotherHolderHasTheAppAuthority(t *testing.T) {
	t.Parallel()
	f := newOnboardIntentFixture(t)
	before, err := f.authority.Document(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stateDir := f.args[slices.Index(f.args, "-state-dir")+1]
	holdAppAuthority(t, stateDir)
	err = runOnboardCommand(t.Context(), f.args, io.Discard, io.Discard)
	if !errors.Is(err, publish.ErrAuthorityWriterLockHeld) {
		t.Fatalf("onboard under a held lock = %v, want ErrAuthorityWriterLockHeld", err)
	}
	after, err := f.authority.Document(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("onboard changed the authority under a held lock:\n%+v\n%+v", before, after)
	}
}

func TestPrelaunchRefusesWhileAnotherHolderHasTheAppAuthority(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	held := t.TempDir()
	holdAppAuthority(t, held)
	f.host.authorityLock = func(string) (*publish.AuthorityWriterLock, error) { return acquireAppAuthority(held) }
	record, body, _, err := f.run(t, f.args())
	if err == nil || record.Passed {
		t.Fatalf("prelaunch passed under a held lock\n%s", body)
	}
	if check := findPrelaunchCheck(record, prelaunchGroupRig, "app_authority_lock"); check.Status != compositionFailed ||
		!strings.Contains(check.Fix, "freesided execution pause") {
		t.Fatalf("app_authority_lock %+v, want failed with the pause named\n%s", check, body)
	}
	for _, name := range []string{"installed_daemon"} {
		if check := findPrelaunchCheck(record, prelaunchGroupRig, name); check.Status != compositionNotRun {
			t.Fatalf("%s %+v ran without the lock\n%s", name, check, body)
		}
	}
	if check := findPrelaunchCheck(record, prelaunchGroupComposition, "composition"); check.Status != compositionNotRun {
		t.Fatalf("composition %+v ran without the lock\n%s", check, body)
	}
	if record.ManifestDigest != nil {
		t.Fatalf("a manifest was composed without the lock\n%s", body)
	}
}

func TestStandalonePreflightReleasesTheLockItTakes(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	_, _, _, release, err := selectRepositoryAuthority(t.Context(), preflightConfig{
		PublicationStateDir: state, PublicationCredentialsDir: t.TempDir(),
	}, time.Now())
	if err == nil {
		release()
		t.Fatal("an empty keystore selected an App")
	}
	lock, err := publish.AcquireAuthorityWriterLock(state)
	if err != nil {
		t.Fatalf("a failed selection kept its lock: %v", err)
	}
	_ = lock.Close()

	holdAppAuthority(t, state)
	_, _, _, _, err = selectRepositoryAuthority(t.Context(), preflightConfig{
		PublicationStateDir: state, PublicationCredentialsDir: t.TempDir(),
	}, time.Now())
	if !errors.Is(err, publish.ErrAuthorityWriterLockHeld) {
		t.Fatalf("selection under a held lock = %v, want ErrAuthorityWriterLockHeld", err)
	}
}
