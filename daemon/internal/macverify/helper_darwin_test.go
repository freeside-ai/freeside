//go:build darwin

package macverify

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestHelperRefusesDirectoryRootDoesNotOwn runs the real helper from a
// directory the test user owns, as a hard link outside the executor root
// would be, and checks it refuses before reading any slot list.
func TestHelperRefusesDirectoryRootDoesNotOwn(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bin", "slot-run")
	installHelper(t, path, helperScript)
	if err := os.WriteFile(filepath.Join(dir, "bin", "slots.conf"), []byte("root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", path, "reap", "root") //nolint:gosec // G204: fixed shell running a test-owned copy of the helper
	cmd.Env = []string{"SUDO_UID=" + strconv.Itoa(os.Getuid())}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != helperRefused || !strings.Contains(string(out), "is not owned by root") {
		t.Fatalf("helper = %v\n%s\nwant a refusal with status %d", err, out, helperRefused)
	}
}

func TestUIDOwnsProcess(t *testing.T) {
	t.Parallel()
	if live, err := uidOwnsProcess(os.Getuid()); err != nil || !live {
		t.Fatalf("uidOwnsProcess(own uid) = %v, %v; want true", live, err)
	}
	// No account on a test host has this uid.
	if live, err := uidOwnsProcess(424242); err != nil || live {
		t.Fatalf("uidOwnsProcess(unused uid) = %v, %v; want false", live, err)
	}
}
