package publish

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// testIdentityInput is a valid candidate identity input for push
// tests, so they exercise the same derivation production uses.
func testIdentityInput(repo, headSHA string) IdentityInput {
	return IdentityInput{
		Repo:            repo,
		BaseRef:         "main",
		SourceHeadSHA:   headSHA,
		ArtifactDigests: []domain.Digest{domain.Digest("sha256:" + strings.Repeat("ab", 32))},
	}
}

// testBranch is the branch PushHead will derive for an input.
func testBranch(t *testing.T, in IdentityInput) string {
	t.Helper()
	id, err := DeriveIdentity(in)
	if err != nil {
		t.Fatal(err)
	}
	return id.BranchName()
}

// gitOut runs plain git to build and inspect test fixtures (the
// hardened runner under test never builds fixtures for itself).
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{
		"-c", "user.name=fixture",
		"-c", "user.email=fixture@example.invalid",
		"-c", "commit.gpgsign=false",
		"-c", "protocol.file.allow=always",
	}
	cmd := exec.Command("git", append(base, args...)...) //nolint:gosec // G204: test-authored fixture arguments
	cmd.Env = scrubbedGitEnv()
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// scrubbedGitEnv is the process environment with every GIT_* entry
// dropped: fixture git commands must never inherit ambient git state.
// A `git rebase --exec` run exports GIT_DIR, which would silently
// retarget every fixture command at this repository itself — the same
// ambient-state class the runner under test closes with its fully
// replaced environment.
func scrubbedGitEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			env = append(env, e)
		}
	}
	return env
}

// localRemote is a bare "managed repository" fixture reachable over
// the file scheme, with one commit on main.
type localRemote struct {
	transport *Transport
	repo      string
	bare      string
	baseSHA   string
	work      string
}

func newLocalRemote(t *testing.T) *localRemote {
	t.Helper()
	root := t.TempDir()
	repo := "owner/example"
	bare := filepath.Join(root, "owner", "example.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o750); err != nil {
		t.Fatal(err)
	}
	gitOut(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	gitOut(t, "", "init", "--initial-branch=main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, work, "add", "a.txt")
	gitOut(t, work, "commit", "-m", "one")
	gitOut(t, work, "push", bare, "main:main")
	return &localRemote{
		transport: &Transport{
			tokens:     staticTokenSource{tok: stubToken()},
			gitPath:    "git",
			remoteBase: "file://" + root,
			scheme:     "file",
		},
		repo:    repo,
		bare:    bare,
		baseSHA: gitOut(t, work, "rev-parse", "HEAD"),
		work:    work,
	}
}

// checkoutDir returns a not-yet-existing directory for FetchBase to
// materialize into.
func checkoutDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "checkout")
}

// candidateHead writes a daemon-style plumbing commit onto the base in
// a fetched checkout and returns its SHA.
func candidateHead(t *testing.T, co Checkout) string {
	t.Helper()
	tree := gitOut(t, co.Dir(), "rev-parse", co.BaseSHA()+"^{tree}")
	return gitOut(t, co.Dir(), "commit-tree", tree, "-p", co.BaseSHA(), "-m", "candidate")
}

func TestFetchBaseMaterializesExactBase(t *testing.T) {
	remote := newLocalRemote(t)
	dir := checkoutDir(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, dir)
	if err != nil {
		t.Fatalf("FetchBase: %v", err)
	}
	if co.Dir() != dir || co.BaseSHA() != remote.baseSHA || co.Repo() != remote.repo || co.BaseRef() != "main" {
		t.Errorf("checkout = %+v", co)
	}
	// The shape the importer's base enforcement expects: HEAD detached
	// at exactly the base, the base anchored against gc, and no
	// working-tree content (the importer is pure plumbing).
	if head := gitOut(t, dir, "rev-parse", "HEAD"); head != remote.baseSHA {
		t.Errorf("HEAD = %s, want %s", head, remote.baseSHA)
	}
	if anchor := gitOut(t, dir, "rev-parse", transportAnchorRef); anchor != remote.baseSHA {
		t.Errorf("anchor = %s, want %s", anchor, remote.baseSHA)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Error("fetch materialized working-tree content; the checkout must stay plumbing-only")
	}
}

func TestFetchBaseRefusesSHAAbsentFromRemote(t *testing.T) {
	remote := newLocalRemote(t)
	missing := strings.Repeat("deadbeef", 5)
	_, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", missing, checkoutDir(t))
	if !errors.Is(err, ErrRemoteMissingBase) {
		t.Errorf("error = %v, want ErrRemoteMissingBase", err)
	}
}

func TestFetchBaseRefusesSHAOffTheBaseBranch(t *testing.T) {
	remote := newLocalRemote(t)
	// A commit that exists on the remote but only on another branch:
	// requesting it against main must fail closed, because "the remote
	// holds it" means reachable from the requested base ref.
	gitOut(t, remote.work, "checkout", "-b", "side")
	if err := os.WriteFile(filepath.Join(remote.work, "side.txt"), []byte("side\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, remote.work, "add", "side.txt")
	gitOut(t, remote.work, "commit", "-m", "side")
	sideSHA := gitOut(t, remote.work, "rev-parse", "HEAD")
	gitOut(t, remote.work, "push", remote.bare, "side:side")
	_, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", sideSHA, checkoutDir(t))
	if !errors.Is(err, ErrRemoteMissingBase) {
		t.Errorf("error = %v, want ErrRemoteMissingBase", err)
	}
}

func TestFetchBaseRefusesMissingBranch(t *testing.T) {
	remote := newLocalRemote(t)
	_, err := remote.transport.FetchBase(t.Context(), remote.repo, "absent", remote.baseSHA, checkoutDir(t))
	if !errors.Is(err, ErrRemoteMissingBase) {
		t.Errorf("error = %v, want ErrRemoteMissingBase", err)
	}
}

// TestValidBranchNameAgreesWithGit is the differential enumeration of
// the refname grammar: every name the gate accepts must be one git
// itself accepts, because a name that passes here and fails in git
// has already cost an audited token mint. It sweeps the input space
// (case, dots, slashes, suffixes, nesting, duplication, control and
// glob characters) rather than the one shape a finding cited.
func TestValidBranchNameAgreesWithGit(t *testing.T) {
	corpus := []string{
		"main", "release-1.2", "a_b", "x.y/z", "freeside/publish/0123abcd",
		"a/b/c/d", "A/B", "0", "a-", "-a", "a.b.c", "a..b", "a...b",
		".hidden", "a/.hidden", "a/b/.hidden", "hidden.", "a/hidden.",
		"a.lock", "a/b.lock", "a.lock/b", "a/b/c.lock", "lock", "a.locked",
		"", "/", "//", "a//b", "a/", "/a", "a/b/", "././.",
		"a b", "a\tb", "a\nb", "a\x7fb", "a\x01b",
		"a:b", "a?b", "a*b", "a[b", "a\\b", "a~b", "a^b", "a@{1}", "@{",
		"@", "a@b", "HEAD", "refs/heads/x", "..", "...",
		strings.Repeat("a", 255), strings.Repeat("a", 256),
	}
	for _, name := range corpus {
		if !validBranchName(name) {
			continue // the gate may be stricter than git; only its accepts bind
		}
		cmd := exec.Command("git", "check-ref-format", "refs/heads/"+name) //nolint:gosec // G204: test-authored corpus
		cmd.Env = scrubbedGitEnv()
		if err := cmd.Run(); err != nil {
			t.Errorf("validBranchName(%q) accepted a name git rejects (%v)", name, err)
		}
	}
	// The gate's deliberate extra strictness, asserted so a future
	// loosening toward "whatever git allows" is a visible decision.
	for _, name := range []string{"-flag", strings.Repeat("a", 256), "a/.hidden", "a/b.lock"} {
		if validBranchName(name) {
			t.Errorf("validBranchName(%q) = true; the gate must stay stricter here", name)
		}
	}
}

// TestFetchBaseAcceptsRelativeDir pins that the checkout path is
// resolved against the caller's working directory, not the private
// scratch every git invocation runs in.
func TestFetchBaseAcceptsRelativeDir(t *testing.T) {
	remote := newLocalRemote(t)
	t.Chdir(t.TempDir())
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, "nested/checkout")
	if err != nil {
		t.Fatalf("FetchBase with a relative dir: %v", err)
	}
	if !filepath.IsAbs(co.Dir()) {
		t.Errorf("checkout dir %q is not absolute", co.Dir())
	}
	if head := gitOut(t, co.Dir(), "rev-parse", "HEAD"); head != remote.baseSHA {
		t.Errorf("HEAD = %s, want %s", head, remote.baseSHA)
	}
	head := candidateHead(t, co)
	if _, err := remote.transport.PushHead(t.Context(), co, testIdentityInput(remote.repo, head)); err != nil {
		t.Errorf("PushHead over a relatively-named checkout: %v", err)
	}
}

// TestFetchBaseDoesNotInferMissingBaseFromFailure pins that
// ErrRemoteMissingBase is a structured verdict, not a reading of the
// failure's prose: a fetch that fails while the branch demonstrably
// exists must stay a plain transport failure the drain may retry,
// never definitive base drift. Here the remote repository is made
// unreadable after the branch is confirmed present, so the fetch
// fails for a reason unrelated to the ref existing.
func TestFetchBaseDoesNotInferMissingBaseFromFailure(t *testing.T) {
	remote := newLocalRemote(t)
	objects := filepath.Join(remote.bare, "objects")
	if err := os.Chmod(objects, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(objects, 0o700) }) //nolint:gosec // G302: restoring a test fixture's own directory mode

	_, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err == nil {
		t.Fatal("fetch from an unreadable remote succeeded")
	}
	if errors.Is(err, ErrRemoteMissingBase) {
		t.Errorf("a transport failure was reported as definitive base drift: %v", err)
	}
}

func TestFetchBaseRefusesClaimedDir(t *testing.T) {
	remote := newLocalRemote(t)
	dir := checkoutDir(t)
	if _, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, dir); err == nil {
		t.Error("FetchBase reused a directory another call already claimed")
	}
}

// countingTokenSource counts mints, so ordering tests can prove a
// rejected call never spent a credential.
type countingTokenSource struct {
	mints int
}

func (c *countingTokenSource) Token(context.Context, string) (InstallationToken, error) {
	c.mints++
	return stubToken(), nil
}

// TestRejectedCallsMintNoToken pins the token-after-gates ordering: a
// FetchBase or PushHead refused by a local gate must never reach the
// token source, so no audited mint and no live credential exists for
// a call that had no remote effect.
func TestRejectedCallsMintNoToken(t *testing.T) {
	remote := newLocalRemote(t)
	counter := &countingTokenSource{}
	tr := &Transport{tokens: counter, gitPath: "git", remoteBase: remote.transport.remoteBase, scheme: "file"}

	if _, err := tr.FetchBase(t.Context(), remote.repo, "absent branch name", remote.baseSHA, checkoutDir(t)); err == nil {
		t.Fatal("invalid base ref accepted")
	}
	if _, err := tr.PushHead(t.Context(), Checkout{dir: t.TempDir(), baseSHA: remote.baseSHA}, testIdentityInput(remote.repo, remote.baseSHA)); err == nil {
		t.Fatal("unbound checkout accepted")
	}
	if counter.mints != 0 {
		t.Errorf("rejected calls minted %d tokens, want 0", counter.mints)
	}

	co, err := tr.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if counter.mints != 1 {
		t.Errorf("successful fetch minted %d tokens, want 1", counter.mints)
	}
	head := candidateHead(t, co)
	if _, err := tr.PushHead(t.Context(), co, testIdentityInput(remote.repo, head)); err != nil {
		t.Fatal(err)
	}
	if counter.mints < 2 {
		t.Errorf("successful push minted no token (total %d)", counter.mints)
	}
}

func TestPushHeadCreatesBranch(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	head := candidateHead(t, co)
	in := testIdentityInput(remote.repo, head)
	res, err := remote.transport.PushHead(t.Context(), co, in)
	if err != nil {
		t.Fatalf("PushHead: %v", err)
	}
	if !res.Created {
		t.Error("first push reported Created=false")
	}
	if got := gitOut(t, remote.bare, "rev-parse", "refs/heads/"+testBranch(t, in)); got != head {
		t.Errorf("remote branch = %s, want %s", got, head)
	}
}

func TestPushHeadConvergesOnIdenticalHead(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	head := candidateHead(t, co)
	in := testIdentityInput(remote.repo, head)
	if _, err := remote.transport.PushHead(t.Context(), co, in); err != nil {
		t.Fatal(err)
	}
	refsBefore := gitOut(t, remote.bare, "for-each-ref")
	res, err := remote.transport.PushHead(t.Context(), co, in)
	if err != nil {
		t.Fatalf("re-push: %v", err)
	}
	if res.Created {
		t.Error("re-push of the identical head reported Created=true")
	}
	if refsAfter := gitOut(t, remote.bare, "for-each-ref"); refsAfter != refsBefore {
		t.Errorf("re-push changed remote refs:\nbefore: %s\nafter: %s", refsBefore, refsAfter)
	}
}

func TestPushHeadRefusesForeignBranch(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	head := candidateHead(t, co)
	in := testIdentityInput(remote.repo, head)
	branch := testBranch(t, in)
	// The branch already exists at the base commit. The candidate head
	// is its descendant, so a plain push would fast-forward it — the
	// exact ref move the create-only discipline must refuse.
	gitOut(t, remote.work, "push", remote.bare, remote.baseSHA+":refs/heads/"+branch)
	_, err = remote.transport.PushHead(t.Context(), co, in)
	if !errors.Is(err, ErrPublicationConflict) {
		t.Errorf("error = %v, want ErrPublicationConflict", err)
	}
	if got := gitOut(t, remote.bare, "rev-parse", "refs/heads/"+branch); got != remote.baseSHA {
		t.Errorf("foreign branch moved to %s; create-only push must never move a ref", got)
	}
}

func TestPushHeadMovesOnlyTheIntendedRef(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	head := candidateHead(t, co)
	// Hostile checkout state: extra refs that a refspec-less or
	// tag-following push could leak to the remote.
	gitOut(t, co.Dir(), "branch", "extra", head)
	gitOut(t, co.Dir(), "tag", "v9", head)
	in := testIdentityInput(remote.repo, head)
	if _, err := remote.transport.PushHead(t.Context(), co, in); err != nil {
		t.Fatal(err)
	}
	refs := gitOut(t, remote.bare, "for-each-ref", "--format=%(refname)")
	want := "refs/heads/" + testBranch(t, in) + "\nrefs/heads/main"
	if refs != want {
		t.Errorf("remote refs after push:\n%s\nwant exactly:\n%s", refs, want)
	}
}

func TestPushHeadRefusesHeadMissingFromCheckout(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	missing := strings.Repeat("deadbeef", 5)
	if _, err := remote.transport.PushHead(t.Context(), co, testIdentityInput(remote.repo, missing)); err == nil {
		t.Error("PushHead accepted a head absent from the checkout")
	}
}

func TestPushHeadRefusesInvalidIdentityInput(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.transport.PushHead(t.Context(), co, IdentityInput{}); err == nil {
		t.Error("PushHead accepted an identity input that derives nothing")
	}
}

// TestPushHeadRefusesUnboundCheckout is the reconstruction
// trust-boundary re-gate, at both layers: a Checkout literal that
// FetchBase never minted fails the provenance bit (outside this
// package it is not even constructible), and a forged provenance bit
// over an arbitrary local repository still fails the repo-binding
// re-gate before any token is spent on it.
func TestPushHeadRefusesUnboundCheckout(t *testing.T) {
	remote := newLocalRemote(t)
	stray := t.TempDir()
	gitOut(t, "", "init", "--initial-branch=main", stray)
	if err := os.WriteFile(filepath.Join(stray, "x.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, stray, "add", "x.txt")
	gitOut(t, stray, "commit", "-m", "stray")
	strayHead := gitOut(t, stray, "rev-parse", "HEAD")
	if _, err := remote.transport.PushHead(t.Context(), Checkout{dir: stray, baseSHA: strayHead}, testIdentityInput(remote.repo, strayHead)); err == nil {
		t.Error("PushHead accepted a checkout the transport never minted")
	}
	forged := Checkout{dir: stray, baseSHA: strayHead, baseRef: "main", repo: remote.repo, owner: remote.transport}
	if _, err := remote.transport.PushHead(t.Context(), forged, testIdentityInput(remote.repo, strayHead)); err == nil {
		t.Error("PushHead accepted a repository without the transport's repo binding")
	}
}

func TestPushHeadRefusesCheckoutBoundToOtherRepo(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	head := candidateHead(t, co)
	// The identity targets a repository this checkout was not
	// materialized from; the stamped binding must refuse the push.
	if _, err := remote.transport.PushHead(t.Context(), co, testIdentityInput("owner/other", head)); err == nil {
		t.Error("PushHead pushed to a repository the checkout is not bound to")
	}
}

// TestPushHeadRefusesIdentityOffTheFetchedBaseRef keeps the identity
// bound to what was actually fetched: a valid input naming another
// base branch would publish (and later target a PR at) a branch the
// enforced base was never reachable from.
func TestPushHeadRefusesIdentityOffTheFetchedBaseRef(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	head := candidateHead(t, co)
	in := testIdentityInput(remote.repo, head)
	in.BaseRef = "release"
	if _, err := remote.transport.PushHead(t.Context(), co, in); err == nil {
		t.Error("PushHead accepted an identity whose base ref is not the fetched one")
	}
}

// TestCheckoutIsSealed pins the capability property the whole
// re-gate class rests on: outside this package a Checkout can only
// come from FetchBase and can only be read, so no caller can mint one
// or repoint an existing one at other state. Reflection over the
// exported API is the mechanical check; a new exported field would
// fail it.
func TestCheckoutIsSealed(t *testing.T) {
	typ := reflect.TypeOf(Checkout{})
	for i := range typ.NumField() {
		if f := typ.Field(i); f.IsExported() {
			t.Errorf("Checkout.%s is exported; the capability must be unforgeable and immutable outside the package", f.Name)
		}
	}
	for _, name := range []string{"Dir", "BaseSHA", "BaseRef", "Repo"} {
		m, ok := typ.MethodByName(name)
		if !ok {
			t.Errorf("Checkout has no %s accessor", name)
			continue
		}
		if m.Type.NumOut() != 1 || m.Type.Out(0).Kind() != reflect.String {
			t.Errorf("Checkout.%s does not read a single string", name)
		}
	}
}

func TestPushHeadRefusesBaseMismatch(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	head := candidateHead(t, co)
	// A Checkout claiming a base other than the one the checkout is
	// actually bound to (HEAD) must fail the exact-base re-gate.
	forged := Checkout{dir: co.Dir(), baseSHA: head, baseRef: "main", repo: remote.repo, owner: remote.transport}
	if _, err := remote.transport.PushHead(t.Context(), forged, testIdentityInput(remote.repo, head)); err == nil {
		t.Error("PushHead accepted a checkout whose claimed base is not the enforced base")
	}
}

// TestPushHeadRefusesHeadOffTheBase closes the arbitrary-commit hole:
// a head that does not descend from the enforced base (here a
// parentless commit over the same tree) must be refused even though
// it is a valid commit in the checkout.
func TestPushHeadRefusesHeadOffTheBase(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	tree := gitOut(t, co.Dir(), "rev-parse", co.BaseSHA()+"^{tree}")
	orphan := gitOut(t, co.Dir(), "commit-tree", tree, "-m", "orphan")
	if _, err := remote.transport.PushHead(t.Context(), co, testIdentityInput(remote.repo, orphan)); err == nil {
		t.Error("PushHead accepted a head that does not descend from the enforced base")
	}
}
