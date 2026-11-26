package cordon

import (
	"path/filepath"
	"slices"
)

// LaunchPath is a cordoned launch's PATH, in search order: the agent CLI's
// directory, the version managers' directories under home, then the system
// directories, keeping the first of any repeat. home is the run account's
// resolved home. It is the one definition of the list, which the daemon's
// backend builder and a host's launcher both use, so every host gives a run
// the same list (docs/plan.md §5.7, Tool versions).
//
// No entry is resolved: /opt/homebrew/opt/rustup/bin is a link Homebrew
// moves on every rustup upgrade, and a resolved entry would change the
// configuration digest, and so force a new proof, with each one. A
// directory that doesn't exist on a host stays on the list and holds
// nothing.
//
// A run can write some of these directories (~/.cargo/bin, ~/.local/bin,
// and on a shared account /opt/homebrew/bin), so a later run's PATH holds
// what an earlier run left there. That is why nothing Freeside starts
// resolves through this PATH: it reaches only the programs started under
// the rule file, and every program the daemon, the supervisor, or the
// suite starts with it is named by absolute path, as is the supervisor,
// which starts with an empty environment.
func LaunchPath(agentCLIDir, home string) []string {
	dirs := []string{
		agentCLIDir,
		"/opt/homebrew/bin",
		"/opt/homebrew/opt/rustup/bin",
		filepath.Join(home, ".cargo", "bin"),
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".local", "share", "mise", "shims"),
		"/usr/bin", "/bin", "/usr/sbin", "/sbin",
	}
	var path []string
	for _, dir := range dirs {
		if !slices.Contains(path, dir) {
			path = append(path, dir)
		}
	}
	return path
}
