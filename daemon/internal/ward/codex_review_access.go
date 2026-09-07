package ward

import (
	"encoding/json"
	"net/url"
)

// EX_CONFIG distinguishes a failed deterministic workspace preflight from a
// provider process failure, so retrying unchanged setup cannot report success.
const codexReviewAccessFailureExitStatus = 78

// codexReviewAccessCommand uses the CLI's Linux sandbox launcher with the same
// root-read-only, network-restricted profile as `exec -s read-only`. It checks
// cwd as well as Git: the CLI can fall back to another cwd when traversal fails.
// Only the trusted wrapper writes the exit status; model output cannot override
// this gate. Successful Git commands prove access, not the quality of review.
func codexReviewAccessCommand(workspace, base, head string) string {
	probe := "set -eu; " +
		"test \"$(pwd -P)\" = " + shellQuote(workspace) + "; " +
		"test \"$(git rev-parse --show-toplevel)\" = " + shellQuote(workspace) + "; " +
		"test \"$(git rev-parse HEAD)\" = " + shellQuote(head) + "; " +
		"git cat-file -e " + shellQuote(base+"^{commit}") + "; " +
		"git cat-file -e " + shellQuote(head+"^{commit}") + "; " +
		// A pipeline would hide Git failure behind a successful hash command.
		"git --no-pager diff --no-ext-diff --no-textconv " + shellQuote(base) + " " + shellQuote(head) + " -- >/dev/null; " +
		"printf 'freeside-review-access-v1 base=%s head=%s cwd=%s\\n' " +
		shellQuote(base) + " " + shellQuote(head) + " \"$(pwd -P)\""
	// A SandboxState fixes the effective profile directly. Named TOML profiles
	// merge with ambient configuration and can inherit additional permissions.
	cwd, _ := json.Marshal((&url.URL{Scheme: "file", Path: workspace}).String())
	state := `{"permissionProfile":{"type":"managed","file_system":{"type":"restricted","entries":[{"path":{"type":"special","value":{"kind":"root"}},"access":"read"}]},"network":"restricted"},"codexLinuxSandboxExe":null,"sandboxCwd":` + string(cwd) + `,"useLegacyLandlock":false}`
	return "codex sandbox --sandbox-state-json " + shellQuote(state) + " -- sh -c " + shellQuote(probe)
}
