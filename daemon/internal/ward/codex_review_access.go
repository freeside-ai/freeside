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
// The report line is read outside the daemon, so it keeps its exact form.
func codexReviewAccessCommand(workspace, base, head string) string {
	// A SandboxState fixes the effective profile directly. Named TOML profiles
	// merge with ambient configuration and can inherit additional permissions.
	cwd, _ := json.Marshal((&url.URL{Scheme: "file", Path: workspace}).String())
	state := `{"permissionProfile":{"type":"managed","file_system":{"type":"restricted","entries":[{"path":{"type":"special","value":{"kind":"root"}},"access":"read"}]},"network":"restricted"},"codexLinuxSandboxExe":null,"sandboxCwd":` + string(cwd) + `,"useLegacyLandlock":false}`
	return "codex sandbox --sandbox-state-json " + shellQuote(state) + " -- sh -c " +
		shellQuote(codexReviewAccessProbe(workspace, base, head))
}

// codexReviewAccessProbe is the shell program the sandbox runs: it exits zero
// only when the workspace is the cwd and Git root, holds the head under
// review, and can read every bound commit and the reviewed diff.
func codexReviewAccessProbe(workspace, base, head string) string {
	probe := "set -eu; " +
		"test \"$(pwd -P)\" = " + shellQuote(workspace) + "; " +
		"test \"$(git rev-parse --show-toplevel)\" = " + shellQuote(workspace) + "; " +
		"test \"$(git rev-parse HEAD)\" = " + shellQuote(head) + "; "
	for _, object := range []string{base, head} {
		probe += "git cat-file -e " + shellQuote(object+"^{commit}") + "; "
	}
	// A pipeline would hide Git failure behind a successful hash command.
	probe += "git --no-pager diff --no-ext-diff --no-textconv " + shellQuote(base) + " " + shellQuote(head) + " -- >/dev/null; " +
		"printf 'freeside-review-access-v1 base=%s head=%s cwd=%s\\n' " +
		shellQuote(base) + " " + shellQuote(head) + " \"$(pwd -P)\""
	return probe
}
