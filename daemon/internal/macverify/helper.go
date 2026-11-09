package macverify

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

//go:embed slot-run.sh
var helperScript []byte

//go:embed profile.sb
var defaultSandboxProfile []byte

// helperRefused is slot-run's exit status for a refusal or failure before it
// hands over to the command (REFUSED in slot-run.sh).
const helperRefused = 120

// commandPath is the only PATH a recipe command sees (COMMAND_PATH in
// slot-run.sh, which checks the daemon names the same value).
const commandPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// ErrHelperUntrusted refuses a job when the installed slot-run helper is not
// the exact, root-owned copy this daemon embeds.
var ErrHelperUntrusted = errors.New("macverify: installed slot-run helper is not trusted")

// HelperScript returns the slot-run helper's bytes, which the executor
// install (#2878) places at <ExecutorRoot>/bin/slot-run.
func HelperScript() []byte { return slices.Clone(helperScript) }

// DefaultSandboxProfile returns the profile macverify ships for
// Config.SandboxProfile: network denied and writes confined to the job
// directory and the slot's home, temporary, and cache directories, which
// arrive as the JOB_DIR, SLOT_HOME, SLOT_TMP, and SLOT_CACHE parameters.
func DefaultSandboxProfile() []byte { return slices.Clone(defaultSandboxProfile) }

// helperPath is where the daemon invokes slot-run. The helper derives the
// executor root from its own location, so naming it from ExecutorRoot is
// what makes the root the helper acts on the one the configuration digest
// names.
func helperPath(executorRoot string) string {
	return filepath.Join(executorRoot, "bin", "slot-run")
}

// checkInstalledHelper refuses the installed helper unless it is a regular
// file owned by owner (root in production), not writable by group or others,
// and byte-identical to the embedded script. The daemon binary's identity is
// in the configuration digest, so this check is what binds the root code a
// job runs to that digest. slot-run separately refuses to act from any
// directory chain root does not own.
func checkInstalledHelper(path string, owner uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrHelperUntrusted, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrHelperUntrusted, path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != owner {
		return fmt.Errorf("%w: %s is not owned by uid %d", ErrHelperUntrusted, path, owner)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s is writable by group or others (%v)", ErrHelperUntrusted, path, info.Mode().Perm())
	}
	installed, err := os.ReadFile(path) //nolint:gosec // G304: the configured executor's own helper
	if err != nil {
		return fmt.Errorf("%w: %w", ErrHelperUntrusted, err)
	}
	if !bytes.Equal(installed, helperScript) {
		return fmt.Errorf("%w: %s differs from the helper this daemon embeds", ErrHelperUntrusted, path)
	}
	return nil
}
