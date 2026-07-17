package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// recordingRoom is the scripted in-test room: deterministic canned
// results keyed by the space-joined argv, every run recorded, and an
// optional inspect hook that observes the materialized workspace while
// it still exists (Verify's scratch is removed on return).
type recordingRoom struct {
	results map[string]StepResult
	inspect func(workdir string)
	runs    [][]string
}

func (r *recordingRoom) Run(_ context.Context, workdir string, argv []string) (StepResult, error) {
	r.runs = append(r.runs, argv)
	if r.inspect != nil {
		r.inspect(workdir)
	}
	if res, ok := r.results[strings.Join(argv, " ")]; ok {
		return res, nil
	}
	return StepResult{Output: []byte("ok\n")}, nil
}

// verifyFixture is the shared end-to-end fixture: a base repository
// carrying the trusted recipe, a candidate commit, and ready options.
func verifyFixture(t *testing.T, changes map[string]string, changeList []importer.Change) (checkout string, opts Options, room *recordingRoom) {
	t.Helper()
	dir, base := initRepo(t, map[string]string{
		testRecipePath: trustedRecipeBytes,
		"README.md":    "base readme",
	})
	head := commitCandidate(t, dir, base, changes)
	room = &recordingRoom{}
	return dir, Options{
		HeadSHA:      head,
		BaseSHA:      base,
		InvocationID: domain.InvocationID("inv-1"),
		RecipeSource: BaseCommitRecipe(),
		Room:         room,
		Changes:      changeList,
	}, room
}

// TestVerifyExecutesTrustedRecipeNotWorkspaceCopy is acceptance 1: the
// candidate rewrites the recipe to a trivial pass, yet the executed
// argv is exactly the trusted recipe's, the hostile copy is present in
// the materialized workspace (so it demonstrably existed and was
// ignored), and the divergence is flagged.
func TestVerifyExecutesTrustedRecipeNotWorkspaceCopy(t *testing.T) {
	hostile := `{"commands": ["true"], "capture": "none"}`
	checkout, opts, room := verifyFixture(
		t,
		map[string]string{testRecipePath: hostile},
		[]importer.Change{{Path: testRecipePath, Kind: importer.ChangeModified, Mode: "100644", Digest: "sha256:aa"}},
	)
	var workspaceRecipe []byte
	room.inspect = func(workdir string) {
		content, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(testRecipePath))) //nolint:gosec // G304: test-owned workspace
		if err != nil {
			t.Errorf("read workspace recipe: %v", err)
		}
		workspaceRecipe = content
	}
	res, err := Verify(context.Background(), checkout, opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	wantRuns := [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}}
	if !reflect.DeepEqual(room.runs, wantRuns) {
		t.Errorf("executed %v, want the trusted recipe's commands %v", room.runs, wantRuns)
	}
	if string(workspaceRecipe) != hostile {
		t.Errorf("workspace recipe = %q, want the hostile candidate copy present but unexecuted", workspaceRecipe)
	}
	if res.RecipeDigest != RecipeDigest([]byte(trustedRecipeBytes)) {
		t.Errorf("result binds recipe digest %s, want the trusted recipe's", res.RecipeDigest)
	}
	var kinds []FindingKind
	for _, f := range res.Findings {
		kinds = append(kinds, f.Kind)
	}
	for _, want := range []FindingKind{FindingRecipeDivergence, FindingVerificationControlPath} {
		found := false
		for _, k := range kinds {
			found = found || k == want
		}
		if !found {
			t.Errorf("findings %v lack %s", kinds, want)
		}
	}
	if res.Outcome != OutcomePassed {
		t.Errorf("outcome = %s, want passed (findings are flags, not failures)", res.Outcome)
	}
}

// TestVerifyMismatchedHeadFailsClosed is acceptance 2's fail-closed
// half: a head the checkout does not hold is a typed error, no command
// runs, and no evidence exists.
func TestVerifyMismatchedHeadFailsClosed(t *testing.T) {
	checkout, opts, room := verifyFixture(t, map[string]string{"main.go": "package main\n"}, nil)
	opts.HeadSHA = "0123456789abcdef0123456789abcdef01234567"
	res, err := Verify(context.Background(), checkout, opts)
	if !errors.Is(err, ErrHeadMismatch) {
		t.Fatalf("err = %v, want ErrHeadMismatch", err)
	}
	if len(room.runs) != 0 {
		t.Errorf("commands ran despite the head mismatch: %v", room.runs)
	}
	if len(res.Evidence) != 0 || res.Outcome != "" {
		t.Errorf("result %+v carries state despite the failure", res)
	}
}

// TestVerifyFailedCommandFailsFast pins the outcome semantics: a
// non-zero exit fails the verification and later commands do not run.
func TestVerifyFailedCommandFailsFast(t *testing.T) {
	checkout, opts, room := verifyFixture(t, map[string]string{"main.go": "package main\n"}, nil)
	room.results = map[string]StepResult{
		"go test ./...": {ExitCode: 2, Output: []byte("FAIL\n")},
	}
	res, err := Verify(context.Background(), checkout, opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Outcome != OutcomeFailed {
		t.Errorf("outcome = %s, want failed", res.Outcome)
	}
	if len(res.Steps) != 1 || len(room.runs) != 1 {
		t.Errorf("steps = %v, runs = %v; want fail-fast after the first command", res.Steps, room.runs)
	}
}

// TestVerifyRoomFailureFailsClosed: a room that cannot execute at all
// is an infrastructure failure, not a verification outcome.
func TestVerifyRoomFailureFailsClosed(t *testing.T) {
	checkout, opts, _ := verifyFixture(t, map[string]string{"main.go": "package main\n"}, nil)
	opts.Room = failingRoom{}
	if _, err := Verify(context.Background(), checkout, opts); err == nil {
		t.Fatal("room failure did not fail Verify")
	}
}

type failingRoom struct{}

func (failingRoom) Run(context.Context, string, []string) (StepResult, error) {
	return StepResult{}, errors.New("room exploded")
}

// TestVerifyInvalidOptions enumerates the fail-loud option boundary.
func TestVerifyInvalidOptions(t *testing.T) {
	checkout, valid, _ := verifyFixture(t, map[string]string{"main.go": "package main\n"}, nil)
	cases := []struct {
		name   string
		mutate func(o Options) Options
	}{
		{"short head", func(o Options) Options { o.HeadSHA = "abc123"; return o }},
		{"uppercase head", func(o Options) Options { o.HeadSHA = strings.ToUpper(o.HeadSHA); return o }},
		{"short base", func(o Options) Options { o.BaseSHA = "abc123"; return o }},
		{"empty invocation", func(o Options) Options { o.InvocationID = ""; return o }},
		{"unset recipe source", func(o Options) Options { o.RecipeSource = RecipeSource{}; return o }},
		{"nil room", func(o Options) Options { o.Room = nil; return o }},
		{"absolute recipe path", func(o Options) Options { o.RecipePath = "/etc/recipe"; return o }},
		{"dotdot recipe path", func(o Options) Options { o.RecipePath = "../verify.json"; return o }},
		{"colon recipe path", func(o Options) Options { o.RecipePath = "a:b.json"; return o }},
		{"glob recipe path", func(o Options) Options { o.RecipePath = "recipes/*.json"; return o }},
		{"negative cap", func(o Options) Options { o.Policy.MaxRecipeBytes = -1; return o }},
		{"negative timeout", func(o Options) Options { o.Policy.CommandTimeout = -1; return o }},
		{"bad widening glob", func(o Options) Options {
			o.Policy.ExtraVerificationControlPatterns = []string{"[unclosed"}
			return o
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Verify(context.Background(), checkout, tc.mutate(valid)); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("err = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

// TestVerifyReportGolden pins the report and transcript contract shapes
// (acceptance 2's binding half): the account binds the exact head SHA
// and recipe digest. The fixture's SHAs are deterministic (pinned
// dates, identity, and content), so the golden holds real values.
func TestVerifyReportGolden(t *testing.T) {
	checkout, opts, room := verifyFixture(
		t,
		map[string]string{"main.go": "package main\n", "go.mod": "module example.test\n"},
		[]importer.Change{
			{Path: "main.go", Kind: importer.ChangeAdded, Mode: "100644", Digest: "sha256:bb"},
			{Path: "go.mod", Kind: importer.ChangeAdded, Mode: "100644", Digest: "sha256:cc"},
		},
	)
	room.results = map[string]StepResult{
		"go test ./...": {Output: []byte("ok  \texample.test\t0.01s\n")},
		"go vet ./...":  {Output: []byte("")},
	}
	res, err := Verify(context.Background(), checkout, opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(res.Evidence) != 2 {
		t.Fatalf("evidence count = %d, want report and transcript", len(res.Evidence))
	}
	if res.Evidence[0].Artifact.Type != ArtifactTypeVerificationReport ||
		res.Evidence[1].Artifact.Type != ArtifactTypeCommandTranscript {
		t.Fatalf("evidence types = %s, %s", res.Evidence[0].Artifact.Type, res.Evidence[1].Artifact.Type)
	}
	golden.Assert(t, "verify_report", res.Evidence[0].Content)
	golden.Assert(t, "verify_transcript", res.Evidence[1].Content)
	if got := contentDigest(res.Evidence[0].Content); res.Evidence[0].Artifact.Digest != got {
		t.Errorf("report artifact digest %s does not address its content %s", res.Evidence[0].Artifact.Digest, got)
	}
}

func TestOutcomeValidity(t *testing.T) {
	for _, o := range AllOutcomes {
		if !o.valid() {
			t.Errorf("registered outcome %q reports invalid", o)
		}
	}
	if Outcome("").valid() {
		t.Error("zero outcome reports valid")
	}
}

// TestVerifyTranscriptCapIsHonest pins that hitting the transcript cap
// is recorded on the result and in the report, not silently dropped.
func TestVerifyTranscriptCapIsHonest(t *testing.T) {
	checkout, opts, room := verifyFixture(t, map[string]string{"main.go": "package main\n"}, nil)
	room.results = map[string]StepResult{
		"go test ./...": {Output: []byte("a long line of test output\n")},
	}
	opts.Policy.MaxTranscriptBytes = 8
	res, err := Verify(context.Background(), checkout, opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.TranscriptTruncated {
		t.Error("transcript hit the cap but TranscriptTruncated is false")
	}
	if !strings.Contains(string(res.Evidence[0].Content), `"transcript_truncated": true`) {
		t.Error("report does not record the transcript truncation")
	}
}

// TestVerifyFreshWorkspacePerCommand is the Codex-review (P1)
// regression: an earlier command's candidate code that mutates the
// workspace must not affect a later command, which runs against a fresh
// head checkout. The room mutates the workspace on the first command
// and reads it on the second, asserting the mutation did not carry over
// and the two commands used different workspaces.
func TestVerifyFreshWorkspacePerCommand(t *testing.T) {
	checkout, opts, _ := verifyFixture(t, map[string]string{"src.go": "package main\n"}, nil)
	var firstDir, secondDir, secondContent string
	mutating := &sequencedRoom{onRun: func(n int, workdir string) {
		switch n {
		case 0:
			firstDir = workdir
			// Candidate code rewrites a source file mid-run.
			_ = os.WriteFile(filepath.Join(workdir, "src.go"), []byte("SABOTAGE"), 0o644) //nolint:gosec // G306: test-owned workspace
		case 1:
			secondDir = workdir
			b, _ := os.ReadFile(filepath.Join(workdir, "src.go")) //nolint:gosec // G304: test-owned workspace
			secondContent = string(b)
		}
	}}
	opts.Room = mutating
	if _, err := Verify(context.Background(), checkout, opts); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if secondContent != "package main\n" {
		t.Errorf("second command saw %q, want the pristine head content (mutation carried over)", secondContent)
	}
	if firstDir == "" || secondDir == "" || firstDir == secondDir {
		t.Errorf("commands shared a workspace: first=%q second=%q", firstDir, secondDir)
	}
}

// sequencedRoom invokes onRun(index, workdir) for each command and
// always reports success.
type sequencedRoom struct {
	onRun func(n int, workdir string)
	n     int
}

func (r *sequencedRoom) Run(_ context.Context, workdir string, _ []string) (StepResult, error) {
	r.onRun(r.n, workdir)
	r.n++
	return StepResult{Output: []byte("ok\n")}, nil
}

// TestVerifyBaseMustBeACommit is the Codex-review regression: ls-tree
// accepts any tree-ish, so a 40-hex tree object passed as BaseSHA would
// silently serve as the recipe source while the report claims it as the
// enforced base commit. It must fail closed instead.
func TestVerifyBaseMustBeACommit(t *testing.T) {
	checkout, opts, room := verifyFixture(t, map[string]string{"main.go": "package main\n"}, nil)
	treeSHA := runGit(t, checkout, "rev-parse", opts.BaseSHA+"^{tree}")
	opts.BaseSHA = treeSHA
	_, err := Verify(context.Background(), checkout, opts)
	if !errors.Is(err, ErrBaseMismatch) {
		t.Fatalf("err = %v, want ErrBaseMismatch", err)
	}
	if len(room.runs) != 0 {
		t.Errorf("commands ran despite the tree-as-base: %v", room.runs)
	}
}

// TestVerifyRejectsSymlinkEntrypoint is the Codex-review regression: a
// recipe entrypoint that is a repo-local symlink is executed by
// following it to its target, so the target (not the lexical link) is
// the real control surface; verification fails closed rather than
// silently flagging only the link.
func TestVerifyRejectsSymlinkEntrypoint(t *testing.T) {
	dir, _ := initRepo(t, map[string]string{"scripts/verify.sh": "#!/bin/sh\ntrue\n"})
	if err := os.Symlink("scripts/verify.sh", filepath.Join(dir, "run-check")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "base+symlink")
	base := runGit(t, dir, "rev-parse", "HEAD")
	head := commitCandidate(t, dir, base, map[string]string{"main.go": "package main\n"})

	room := &recordingRoom{}
	opts := Options{
		HeadSHA: head, BaseSHA: base,
		InvocationID: domain.InvocationID("inv-1"),
		// A config recipe whose entrypoint is the repo-local symlink.
		RecipeSource: ConfigRecipe([]byte(`{"commands": ["./run-check"], "capture": "none"}`)),
		Room:         room,
	}
	_, err := Verify(context.Background(), dir, opts)
	if !errors.Is(err, ErrSymlinkEntrypoint) {
		t.Fatalf("err = %v, want ErrSymlinkEntrypoint", err)
	}
	if len(room.runs) != 0 {
		t.Errorf("commands ran despite the symlink entrypoint: %v", room.runs)
	}
}

// TestVerifyRejectsSymlinkPrefixEntrypoint is the Codex-review
// regression: a recipe entrypoint that traverses a symlinked directory
// prefix (`./run-check/verify.sh` with `run-check` -> `scripts`) runs a
// target the lexical path does not name, so it fails closed like a
// direct symlink entrypoint.
func TestVerifyRejectsSymlinkPrefixEntrypoint(t *testing.T) {
	dir, _ := initRepo(t, map[string]string{"scripts/verify.sh": "#!/bin/sh\ntrue\n"})
	if err := os.Symlink("scripts", filepath.Join(dir, "run-check")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "base+dirlink")
	base := runGit(t, dir, "rev-parse", "HEAD")
	head := commitCandidate(t, dir, base, map[string]string{"main.go": "package main\n"})
	room := &recordingRoom{}
	opts := Options{
		HeadSHA: head, BaseSHA: base,
		InvocationID: domain.InvocationID("inv-1"),
		RecipeSource: ConfigRecipe([]byte(`{"commands": ["./run-check/verify.sh"], "capture": "none"}`)),
		Room:         room,
	}
	_, err := Verify(context.Background(), dir, opts)
	if !errors.Is(err, ErrSymlinkEntrypoint) {
		t.Fatalf("err = %v, want ErrSymlinkEntrypoint", err)
	}
	if len(room.runs) != 0 {
		t.Errorf("commands ran despite the symlinked prefix: %v", room.runs)
	}
}
