package verify

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// historyProbe is a committed script a history command runs. It prints
// what the command can see of the history as key=value lines the tests
// read back from the transcript, and fails if git can't read any of it.
// Each argument names a path to read at the head.
const historyProbe = `#!/usr/bin/env bash
set -euo pipefail
echo "base=$(git rev-parse --verify "$FREESIDE_VERIFY_BASE_SHA^{commit}")"
echo "head=$(git rev-parse --verify "$FREESIDE_VERIFY_HEAD_SHA^{commit}")"
echo "HEAD=$(git rev-parse HEAD)"
echo "merge-base=$(git merge-base "$FREESIDE_VERIFY_BASE_SHA" "$FREESIDE_VERIFY_HEAD_SHA")"
git log --reverse --format='log=%H %s' "$FREESIDE_VERIFY_BASE_SHA..$FREESIDE_VERIFY_HEAD_SHA"
git diff --name-status "$FREESIDE_VERIFY_BASE_SHA" "$FREESIDE_VERIFY_HEAD_SHA" | sed 's/^/diff=/'
git fsck --connectivity-only --no-dangling >/dev/null
echo "files=$(ls -A | LC_ALL=C sort | tr '\n' ' ')"
for path in "$@"; do
  echo "show=$path:$(git show "$FREESIDE_VERIFY_HEAD_SHA:$path")"
done
`

const historyProbeRecipe = `{"commands": [{"argv": ["bash", "history-probe.sh", "restored.txt"], "room": "project_image", "history": true}], "capture": "none"}`

// candidateCommit is one commit of a multi-commit candidate: its message
// and its changes (an empty string deletes the path).
type candidateCommit struct {
	message string
	changes map[string]string
}

// commitChain commits each step on top of parent, anchors the last commit
// against gc, moves the checkout back to parent, and returns the commits
// in order. The message's first line is the subject; the rest is the body.
func commitChain(t *testing.T, dir, parent string, chain []candidateCommit) []string {
	t.Helper()
	runGit(t, dir, "reset", "-q", "--hard", parent)
	shas := make([]string, 0, len(chain))
	for _, c := range chain {
		for path, content := range c.changes {
			if content == "" {
				runGit(t, dir, "rm", "-q", "--", path)
			}
		}
		writeFiles(t, dir, c.changes)
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-q", "--allow-empty", "--cleanup=verbatim", "-m", c.message)
		shas = append(shas, runGit(t, dir, "rev-parse", "HEAD"))
	}
	runGit(t, dir, "update-ref", "refs/freeside/candidate-"+shas[len(shas)-1], shas[len(shas)-1])
	runGit(t, dir, "reset", "-q", "--hard", parent)
	return shas
}

// historyBase returns a checkout whose base commit sits on top of an
// older one: restored.txt holds "old" before the base and "new" at it, so
// a candidate that restores "old" reuses a blob only older history holds.
func historyBase(t *testing.T, files map[string]string) (dir, base string) {
	t.Helper()
	all := map[string]string{"history-probe.sh": historyProbe, "restored.txt": "old\n", "README.md": "readme\n"}
	for path, content := range files {
		all[path] = content
	}
	dir, root := initRepo(t, all)
	return dir, commitChain(t, dir, root, []candidateCommit{{"Advance the base", map[string]string{"restored.txt": "new\n"}}})[0]
}

func historyOptions(head, base, recipe string, room Room) Options {
	return Options{
		HeadSHA:      head,
		BaseSHA:      base,
		InvocationID: domain.InvocationID("inv-history"),
		RecipeSource: ConfigRecipe([]byte(recipe)),
		Rooms:        imageRooms(room),
	}
}

// probeOutput returns the transcript's key=value lines for key.
func probeOutput(res Result, key string) []string {
	var values []string
	for _, line := range strings.Split(string(res.Evidence[1].Content), "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			values = append(values, value)
		}
	}
	return values
}

// TestVerifyHistoryGivesExactlyTheCandidateRange pins what a history
// command sees, offline, in the real process room: both SHAs resolve,
// base..head lists exactly the candidate's commits with their messages,
// the diff between them works, merge-base returns the base, and HEAD is
// the head. The multi-commit case restores a blob only history older
// than the base holds, which a walk excluding the base would drop.
func TestVerifyHistoryGivesExactlyTheCandidateRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		chain    []candidateCommit
		diff     []string
		restored string
	}{
		{
			name:     "one commit",
			chain:    []candidateCommit{{"Add feature one", map[string]string{"feature.txt": "one\n"}}},
			diff:     []string{"A\tfeature.txt"},
			restored: "new",
		},
		{
			name: "several commits with a restore",
			chain: []candidateCommit{
				{"Add feature one", map[string]string{"feature.txt": "one\n"}},
				{"Restore the old text", map[string]string{"restored.txt": "old\n"}},
				{"Remove the readme", map[string]string{"README.md": ""}},
			},
			diff:     []string{"D\tREADME.md", "A\tfeature.txt", "M\trestored.txt"},
			restored: "old",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkout, base := historyBase(t, nil)
			commits := commitChain(t, checkout, base, tc.chain)
			head := commits[len(commits)-1]
			res, err := Verify(context.Background(), checkout,
				historyOptions(head, base, historyProbeRecipe, &ProcRoom{Home: t.TempDir()}))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.Outcome != OutcomePassed {
				t.Fatalf("outcome = %s, transcript:\n%s", res.Outcome, res.Evidence[1].Content)
			}
			wantLog := make([]string, len(commits))
			for i, sha := range commits {
				wantLog[i] = sha + " " + tc.chain[i].message
			}
			for key, want := range map[string][]string{
				"base": {base}, "head": {head}, "HEAD": {head}, "merge-base": {base},
				"log": wantLog, "diff": tc.diff, "show": {"restored.txt:" + tc.restored},
			} {
				if got := probeOutput(res, key); !slices.Equal(got, want) {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if len(res.Steps) != 1 || !res.Steps[0].History {
				t.Errorf("steps = %+v, want one history step", res.Steps)
			}
			rep, err := ParseReport(res.Evidence[0].Content)
			if err != nil {
				t.Fatalf("ParseReport: %v", err)
			}
			if rep.HistoryBaseSHA != base {
				t.Errorf("report history base = %s, want %s", rep.HistoryBaseSHA, base)
			}
		})
	}
}

// envRoom is a scripted EnvRoom that records, per call, whether it came
// through RunEnv, the extra environment, and the workspace's .git file
// set while the workspace still exists.
type envRoom struct {
	calls []envCall
}

type envCall struct {
	argv    []string
	viaEnv  bool
	env     []string
	gitTree map[string]string // relative path to content; "" for a directory
	hasGit  bool
}

func (r *envRoom) Run(ctx context.Context, workdir string, argv []string) (StepResult, error) {
	return r.record(workdir, argv, false, nil)
}

func (r *envRoom) RunEnv(_ context.Context, workdir string, argv, env []string) (StepResult, error) {
	return r.record(workdir, argv, true, env)
}

func (r *envRoom) record(workdir string, argv []string, viaEnv bool, env []string) (StepResult, error) {
	call := envCall{argv: argv, viaEnv: viaEnv, env: env}
	gitDir := filepath.Join(workdir, ".git")
	if _, err := os.Lstat(gitDir); err == nil {
		call.hasGit = true
		call.gitTree = map[string]string{}
		err := filepath.WalkDir(gitDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(gitDir, p)
			if err != nil || rel == "." {
				return err
			}
			if d.IsDir() {
				call.gitTree[filepath.ToSlash(rel)] = ""
				return nil
			}
			body, err := os.ReadFile(p) //nolint:gosec // G304: test-owned workspace
			call.gitTree[filepath.ToSlash(rel)] = string(body)
			return err
		})
		if err != nil {
			return StepResult{}, err
		}
	}
	r.calls = append(r.calls, call)
	return StepResult{Output: []byte("ok\n")}, nil
}

var _ EnvRoom = (*envRoom)(nil)

// TestVerifyHistoryWritesOnlyTheVerifiersGitDir pins the written .git's
// exact file set and the environment a history command gets, and that
// the commands around it keep today's workspace: no .git, no extra
// environment, through Run.
func TestVerifyHistoryWritesOnlyTheVerifiersGitDir(t *testing.T) {
	t.Parallel()
	checkout, base := historyBase(t, nil)
	head := commitChain(t, checkout, base, []candidateCommit{{"Add feature one", map[string]string{"feature.txt": "one\n"}}})[0]
	recipe := `{"commands": [["go", "vet", "./..."], ` +
		`{"argv": ["bash", "history-probe.sh"], "room": "project_image", "history": true}, ` +
		`["go", "test", "./..."]], "capture": "none"}`
	room := &envRoom{}
	if _, err := Verify(context.Background(), checkout, historyOptions(head, base, recipe, room)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(room.calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(room.calls))
	}
	for _, i := range []int{0, 2} {
		if c := room.calls[i]; c.viaEnv || c.env != nil || c.hasGit {
			t.Errorf("command %q without history got viaEnv=%v env=%q .git=%v", c.argv, c.viaEnv, c.env, c.hasGit)
		}
	}
	c := room.calls[1]
	if !c.viaEnv {
		t.Fatal("history command did not run through RunEnv")
	}
	wantEnv := []string{
		"FREESIDE_VERIFY_BASE_SHA=" + base,
		"FREESIDE_VERIFY_HEAD_SHA=" + head,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=*",
	}
	if !slices.Equal(c.env, wantEnv) {
		t.Errorf("env = %q, want %q", c.env, wantEnv)
	}
	packName := regexp.MustCompile(`^objects/pack/pack-[0-9a-f]{40}\.(pack|idx)$`)
	var packs []string
	for path := range c.gitTree {
		if packName.MatchString(path) {
			packs = append(packs, path)
			delete(c.gitTree, path)
		}
	}
	slices.Sort(packs)
	if len(packs) != 2 || strings.TrimSuffix(packs[0], ".idx") != strings.TrimSuffix(packs[1], ".pack") {
		t.Errorf("packs = %q, want one pack and its index", packs)
	}
	want := map[string]string{
		"HEAD":         head + "\n",
		"config":       "[core]\n\trepositoryformatversion = 0\n\tbare = false\n",
		"shallow":      base + "\n",
		"refs":         "",
		"objects":      "",
		"objects/pack": "",
	}
	if len(c.gitTree) != len(want) {
		t.Errorf(".git holds %q, want exactly %q plus the pack", slices.Sorted(maps.Keys(c.gitTree)), slices.Sorted(maps.Keys(want)))
	}
	for path, content := range want {
		if got, ok := c.gitTree[path]; !ok || got != content {
			t.Errorf(".git/%s = %q (present %v), want %q", path, got, ok, content)
		}
	}
}

// TestVerifyHistoryOnReentryExportsTheHeadsOwnBase pins a base-advance
// re-entry: the workspace holds the prospective merge's tree, while the
// exported base is the head's own base, so base..head is still exactly the
// candidate's commits, and HEAD is the candidate head, not the merge.
func TestVerifyHistoryOnReentryExportsTheHeadsOwnBase(t *testing.T) {
	t.Parallel()
	checkout, oldBase := historyBase(t, nil)
	chain := []candidateCommit{
		{"Add feature one", map[string]string{"feature.txt": "one\n"}},
		{"Add feature two", map[string]string{"feature2.txt": "two\n"}},
	}
	commits := commitChain(t, checkout, oldBase, chain)
	head := commits[1]
	newBase := commitChain(t, checkout, oldBase, []candidateCommit{{"Land other work", map[string]string{"landed.txt": "landed\n"}}})[0]
	evaluated := commitWithParents(t, checkout, mergedTree(t, checkout, newBase, head), newBase, head)
	opts := historyOptions(head, newBase, historyProbeRecipe, &ProcRoom{Home: t.TempDir()})
	opts.EvaluatedSHA = evaluated
	res, err := Verify(context.Background(), checkout, opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Outcome != OutcomePassed {
		t.Fatalf("outcome = %s, transcript:\n%s", res.Outcome, res.Evidence[1].Content)
	}
	for key, want := range map[string][]string{
		"base": {oldBase}, "head": {head}, "HEAD": {head}, "merge-base": {oldBase},
		"log":   {commits[0] + " Add feature one", commits[1] + " Add feature two"},
		"files": {".git README.md feature.txt feature2.txt history-probe.sh landed.txt restored.txt "},
	} {
		if got := probeOutput(res, key); !slices.Equal(got, want) {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	rep, err := ParseReport(res.Evidence[0].Content)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if rep.HistoryBaseSHA != oldBase || rep.BaseSHA != newBase || rep.EvaluatedSHA != evaluated {
		t.Errorf("report bases = history %s, base %s, evaluated %s", rep.HistoryBaseSHA, rep.BaseSHA, rep.EvaluatedSHA)
	}
}

// TestVerifyHistoryRefusesWhatItCantGiveExactly pins the fail-closed
// refusals: no single merge base, a tree path the written .git would
// collide with, and a room that can't pass the environment. Nothing runs.
func TestVerifyHistoryRefusesWhatItCantGiveExactly(t *testing.T) {
	t.Parallel()
	commitTree := func(t *testing.T, dir, tree, message string, parents ...string) string {
		t.Helper()
		args := []string{"commit-tree", tree, "-m", message}
		for _, p := range parents {
			args = append(args, "-p", p)
		}
		sha := runGit(t, dir, args...)
		runGit(t, dir, "update-ref", "refs/freeside/fixture-"+sha, sha)
		return sha
	}
	treeWith := func(t *testing.T, dir, commit, name string) string {
		t.Helper()
		blob := runGitStdin(t, dir, []byte("x\n"), "hash-object", "-w", "--stdin")
		inner := runGitStdin(t, dir, []byte("100644 blob "+blob+"\tconfig\n"), "mktree")
		entries := runGit(t, dir, "ls-tree", commit)
		return runGitStdin(t, dir, []byte(entries+"\n040000 tree "+inner+"\t"+name+"\n"), "mktree")
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, dir, base string) (head, enforcedBase string)
		want  error
		// malformed says whether the refusal also reads as a malformed
		// tree, which the engine stops on rather than retrying.
		malformed bool
	}{
		{"two merge bases", func(t *testing.T, dir, base string) (string, string) {
			tree := runGit(t, dir, "rev-parse", base+"^{tree}")
			b1 := commitTree(t, dir, tree, "side one", base)
			b2 := commitTree(t, dir, tree, "side two", base)
			return commitTree(t, dir, tree, "cross one", b1, b2), commitTree(t, dir, tree, "cross two", b2, b1)
		}, ErrHistoryUnavailable, false},
		{"no merge base", func(t *testing.T, dir, base string) (string, string) {
			tree := runGit(t, dir, "rev-parse", base+"^{tree}")
			return commitTree(t, dir, tree, "unrelated root"), base
		}, ErrHistoryUnavailable, false},
		{"dot-git case variant in the tree", func(t *testing.T, dir, base string) (string, string) {
			return commitTree(t, dir, treeWith(t, dir, base, ".GIT"), "plant a git dir", base), base
		}, ErrHistoryUnavailable, true},
		{"dot-git short name in the tree", func(t *testing.T, dir, base string) (string, string) {
			return commitTree(t, dir, treeWith(t, dir, base, "git~1"), "plant a git dir", base), base
		}, ErrHistoryUnavailable, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkout, base := historyBase(t, nil)
			head, enforced := tc.setup(t, checkout, base)
			room := &envRoom{}
			_, err := Verify(context.Background(), checkout, historyOptions(head, enforced, historyProbeRecipe, room))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Verify = %v, want %v", err, tc.want)
			}
			if got := errors.Is(err, ErrMalformedTree); got != tc.malformed {
				t.Fatalf("errors.Is(%v, ErrMalformedTree) = %t, want %t", err, got, tc.malformed)
			}
			if len(room.calls) != 0 {
				t.Fatalf("commands ran: %+v", room.calls)
			}
		})
	}
	t.Run("room without RunEnv", func(t *testing.T) {
		t.Parallel()
		checkout, base := historyBase(t, nil)
		head := commitChain(t, checkout, base, []candidateCommit{{"Add feature one", map[string]string{"feature.txt": "one\n"}}})[0]
		room := &recordingRoom{}
		_, err := Verify(context.Background(), checkout, historyOptions(head, base, historyProbeRecipe, room))
		if !errors.Is(err, ErrRoomUnavailable) {
			t.Fatalf("Verify = %v, want ErrRoomUnavailable", err)
		}
		if len(room.runs) != 0 {
			t.Fatalf("commands ran: %q", room.runs)
		}
	})
}

// TestVerifyHistoryRunsTheRepositoryChecks runs #3121's two scripts, as
// the repository holds them, through Verify in the process room: a good
// candidate passes both, a bad commit message fails the commit-message
// check, and deleting a migration fails the migration-history check.
func TestVerifyHistoryRunsTheRepositoryChecks(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"run-check.sh":                    "#!/usr/bin/env bash\nset -euo pipefail\nexec bash \"$1\" \"$FREESIDE_VERIFY_BASE_SHA\" \"$FREESIDE_VERIFY_HEAD_SHA\"\n",
		"daemon/migrations/0001_init.sql": "CREATE TABLE t (id INTEGER);\n",
	}
	for _, script := range []string{"check-commit-messages.sh", "check-migration-history.sh"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", script)) //nolint:gosec // G304: a fixed repository script name from this test's own list
		if err != nil {
			t.Fatal(err)
		}
		files["scripts/"+script] = string(body)
	}
	recipe := `{"commands": [` +
		`{"argv": ["bash", "run-check.sh", "scripts/check-commit-messages.sh"], "room": "project_image", "history": true}, ` +
		`{"argv": ["bash", "run-check.sh", "scripts/check-migration-history.sh"], "room": "project_image", "history": true}` +
		`], "capture": "none"}`
	const goodMessage = "Add the feature file\n\nThe feature needs a file of its own."
	cases := []struct {
		name  string
		chain []candidateCommit
		exits []int
	}{
		{"good candidate", []candidateCommit{
			{goodMessage, map[string]string{"feature.txt": "one\n"}},
			{"Extend the feature file\n\nA second commit keeps the range honest.", map[string]string{"feature.txt": "two\n"}},
		}, []int{0, 0}},
		{"bad commit message", []candidateCommit{
			{goodMessage, map[string]string{"feature.txt": "one\n"}},
			{"wip", map[string]string{"feature.txt": "two\n"}},
		}, []int{1}},
		{"deleted migration", []candidateCommit{
			{goodMessage, map[string]string{"daemon/migrations/0001_init.sql": ""}},
		}, []int{0, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkout, base := historyBase(t, files)
			commits := commitChain(t, checkout, base, tc.chain)
			res, err := Verify(context.Background(), checkout,
				historyOptions(commits[len(commits)-1], base, recipe, &ProcRoom{Home: t.TempDir()}))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			var exits []int
			for _, step := range res.Steps {
				exits = append(exits, step.ExitCode)
			}
			if !slices.Equal(exits, tc.exits) {
				t.Fatalf("exits = %v, want %v; transcript:\n%s", exits, tc.exits, res.Evidence[1].Content)
			}
		})
	}
}

// TestVerifyHistoryReportGolden pins the report shape a history step adds:
// the step's history flag and the report's history base. The fixture's
// SHAs are deterministic, so the golden holds real values.
func TestVerifyHistoryReportGolden(t *testing.T) {
	t.Parallel()
	checkout, base := historyBase(t, nil)
	head := commitChain(t, checkout, base, []candidateCommit{{"Add feature one", map[string]string{"feature.txt": "one\n"}}})[0]
	recipe := `{"commands": [["go", "vet", "./..."], ` +
		`{"argv": ["bash", "history-probe.sh"], "room": "project_image", "history": true}], "capture": "none"}`
	res, err := Verify(context.Background(), checkout, historyOptions(head, base, recipe, &envRoom{}))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	golden.Assert(t, "verify_report_history", res.Evidence[0].Content)
	rep, err := ParseReport(res.Evidence[0].Content)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if rep.HistoryBaseSHA != base || rep.Steps[0].History || !rep.Steps[1].History {
		t.Errorf("parsed report = %+v", rep)
	}
}

// TestParseReportRefusesHistoryWithoutABase pins that a report can't
// claim a history step without naming the base its range started at.
func TestParseReportRefusesHistoryWithoutABase(t *testing.T) {
	t.Parallel()
	rep := Report{
		HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
		RecipePath: testRecipePath, RecipeDigest: RecipeDigest([]byte(trustedRecipeBytes)),
		Outcome:  OutcomePassed,
		Steps:    []Step{{Argv: []string{"bash", "check.sh"}, Room: ClassProjectImage, History: true}},
		Findings: []Finding{},
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(append(raw, '\n')); err == nil {
		t.Fatal("ParseReport accepted a history step with no history base")
	}
	rep.HistoryBaseSHA = strings.Repeat("c", 40)
	raw, err = json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(append(raw, '\n')); err != nil {
		t.Fatalf("ParseReport with a history base: %v", err)
	}
}
