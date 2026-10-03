package ward

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The probe is a shell program, so its meaning is whatever Git and sh make of
// it. This runs it against a real checkout outside the sandbox wrapper: a base
// that advanced, a head written on the old base, and their merge.
func TestCodexReviewAccessProbeBindsTheWorkspaceCommit(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		return strings.TrimSpace(rungitLive(t, root, args...))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("README.md", "start\n")
	git("add", "--all")
	git("commit", "-q", "-m", "start")
	start := git("rev-parse", "HEAD")
	write("feature.txt", "head\n")
	git("add", "--all")
	git("commit", "-q", "-m", "head")
	head := git("rev-parse", "HEAD")
	git("checkout", "-q", "--detach", start)
	write("README.md", "advanced\n")
	git("add", "--all")
	git("commit", "-q", "-m", "advanced base")
	base := git("rev-parse", "HEAD")
	git("merge", "-q", "--no-ff", "-m", "prospective merge", head)
	merge := git("rev-parse", "HEAD")
	absent := strings.Repeat("f", 40)

	for _, tc := range []struct {
		name                  string
		checkout              string
		base, head, evaluated string
		wantReport            string
	}{
		{
			name: "head workspace", checkout: head, base: start, head: head,
			wantReport: "freeside-review-access-v1 base=" + start + " head=" + head + " cwd=" + root,
		},
		{
			name: "merge workspace", checkout: merge, base: base, head: head, evaluated: merge,
			wantReport: "freeside-review-access-v2 base=" + base + " head=" + head + " evaluated=" + merge + " cwd=" + root,
		},
		{name: "merge requested, workspace at head", checkout: head, base: base, head: head, evaluated: merge},
		{name: "head requested, workspace at merge", checkout: merge, base: base, head: head},
		{name: "merge workspace, absent head", checkout: merge, base: base, head: absent, evaluated: merge},
		{name: "merge workspace, absent base", checkout: merge, base: absent, head: head, evaluated: merge},
		{name: "absent evaluated commit", checkout: merge, base: base, head: head, evaluated: absent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git("checkout", "-q", "--detach", tc.checkout)
			cmd := osexec.Command( //nolint:gosec // generated probe, test-owned checkout
				"sh", "-c", codexReviewAccessProbe(root, tc.base, tc.head, tc.evaluated))
			cmd.Dir = root
			cmd.Env = append(scrubbedLiveGitEnv(),
				"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
			out, err := cmd.CombinedOutput()
			if tc.wantReport == "" {
				if err == nil {
					t.Fatalf("probe accepted a workspace it must refuse:\n%s", out)
				}
				return
			}
			if err != nil || strings.TrimSpace(string(out)) != tc.wantReport {
				t.Fatalf("probe = %v, %q; want %q", err, out, tc.wantReport)
			}
		})
	}
}
