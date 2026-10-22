// Package gitenv gives daemon test fixtures one process environment to run
// git with, so no fixture command writes to a repository the caller's
// environment names.
//
// Git exports GIT_DIR to hooks and to rebase --exec steps. A fixture
// command that inherits it acts on that repository instead of its own, -C
// notwithstanding: from a linked worktree, a fixture `git init` reinitializes
// the shared repository and writes core.bare = true into its config, and
// fixture commits land in it. Scrubbed drops every GIT_* entry. The rest of
// the environment stays, because git on macOS needs it to find its temporary
// directory without printing a warning into the output fixtures parse.
// Rationale: devlog/2026-10-22-1303-shared-fixture-git-scrub.md.
//
// Usage (from any _test.go), appending the helper's own pins:
//
//	cmd.Env = append(gitenv.Scrubbed(), "GIT_CONFIG_GLOBAL="+os.DevNull)
//
// A helper that replaces the environment outright needs none of this.
// scripts/check.sh daemon test runs the suite under a decoy GIT_DIR and fails
// when a helper that skips Scrubbed writes to it.
package gitenv

import (
	"os"
	"strings"
)

// Scrubbed returns the process environment without its GIT_* entries,
// keeping the other entries in order. Each call returns a fresh slice, so
// callers may append to it.
func Scrubbed() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return env
}
