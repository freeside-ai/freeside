package publish

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
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

// testGatedHead mints the publication gate capability that tr will
// accept, claiming tr's authority for a stub publisher on first use the
// way the daemon's wiring claims it for the real one. Production mints
// the capability at exactly one site — inside Publisher.publish, once
// every gate has passed — so a transport test exercising the transport's
// own re-gates rather than the publication gate has to stand in for that
// site. Tests of the gate requirement itself pass the zero GatedHead, or
// one issued for another transport.
func testGatedHead(t *testing.T, tr *Transport, in IdentityInput) GatedHead {
	t.Helper()
	if tr.publisher == nil {
		if err := tr.AuthorizePublisher(&Publisher{}); err != nil {
			t.Fatal(err)
		}
	}
	gated, err := gateHead(in, tr.publisher)
	if err != nil {
		t.Fatal(err)
	}
	return gated
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
	return newLocalRemoteForRepo(t, "owner/example")
}

// newLocalRemoteForRepo builds the fixture under a caller-chosen
// owner/name, so a test composing the transport with fixtures keyed to
// another repository (the publisher's forge fake, its trust profile) can
// have all of them name the same one.
func newLocalRemoteForRepo(t *testing.T, repo string) *localRemote {
	t.Helper()
	root := t.TempDir()
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		t.Fatalf("fixture repo %q is not owner/name", repo)
	}
	bare := filepath.Join(root, owner, name+".git")
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
	if _, err := remote.transport.PushHead(t.Context(), co, testGatedHead(t, remote.transport, testIdentityInput(remote.repo, head))); err != nil {
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
	if _, err := tr.PushHead(t.Context(), Checkout{dir: t.TempDir(), baseSHA: remote.baseSHA}, testGatedHead(t, tr, testIdentityInput(remote.repo, remote.baseSHA))); err == nil {
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
	if _, err := tr.PushHead(t.Context(), co, testGatedHead(t, tr, testIdentityInput(remote.repo, head))); err != nil {
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
	res, err := remote.transport.PushHead(t.Context(), co, testGatedHead(t, remote.transport, in))
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
	if _, err := remote.transport.PushHead(t.Context(), co, testGatedHead(t, remote.transport, in)); err != nil {
		t.Fatal(err)
	}
	refsBefore := gitOut(t, remote.bare, "for-each-ref")
	res, err := remote.transport.PushHead(t.Context(), co, testGatedHead(t, remote.transport, in))
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
	_, err = remote.transport.PushHead(t.Context(), co, testGatedHead(t, remote.transport, in))
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
	if _, err := remote.transport.PushHead(t.Context(), co, testGatedHead(t, remote.transport, in)); err != nil {
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
	if _, err := remote.transport.PushHead(t.Context(), co, testGatedHead(t, remote.transport, testIdentityInput(remote.repo, missing))); err == nil {
		t.Error("PushHead accepted a head absent from the checkout")
	}
}

// TestPushHeadRefusesUngatedHead is the #288 boundary: the zero
// GatedHead is the only one a caller outside this package can build, and
// a push carrying it is refused before the checkout is even observed —
// no token minted, no ref on the remote. A candidate that never faced
// Publisher's gates therefore cannot reach the managed repository even
// with a genuine Checkout in hand.
func TestPushHeadRefusesUngatedHead(t *testing.T) {
	remote := newLocalRemote(t)
	counter := &countingTokenSource{}
	tr := &Transport{tokens: counter, gitPath: "git", remoteBase: remote.transport.remoteBase, scheme: "file"}
	co, err := tr.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	head := candidateHead(t, co)
	mintsAfterFetch := counter.mints

	if _, err := tr.PushHead(t.Context(), co, GatedHead{}); !errors.Is(err, ErrUngatedPublication) {
		t.Errorf("error = %v, want ErrUngatedPublication", err)
	}
	if counter.mints != mintsAfterFetch {
		t.Errorf("ungated push minted %d tokens, want 0", counter.mints-mintsAfterFetch)
	}
	// The identity the ungated head would have targeted holds no ref: the
	// refusal is zero-effect, not a rejected-after-the-fact cleanup.
	branch := testBranch(t, testIdentityInput(remote.repo, head))
	if refs := gitOut(t, remote.bare, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, branch) {
		t.Errorf("ungated push created %s; remote refs: %s", branch, refs)
	}

	// A capability minted for a different transport is refused just as
	// hard, and just as early: the gate this transport honours is its own
	// publisher's, not any publisher's.
	other := testGatedHead(t, remote.transport, testIdentityInput(remote.repo, head))
	if _, err := tr.PushHead(t.Context(), co, other); !errors.Is(err, ErrUngatedPublication) {
		t.Errorf("error = %v, want ErrUngatedPublication for a foreign publisher's gate", err)
	}
	if counter.mints != mintsAfterFetch {
		t.Errorf("foreign-gated push minted %d tokens, want 0", counter.mints-mintsAfterFetch)
	}
	if refs := gitOut(t, remote.bare, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, branch) {
		t.Errorf("foreign-gated push created %s; remote refs: %s", branch, refs)
	}
}

// TestGatedHeadIsSealed pins the capability property PushHead's gate
// requirement rests on: outside this package a GatedHead can only be the
// zero value and can only be read, so no caller can mint one or repoint
// one it holds at another candidate. A new exported field fails it.
func TestGatedHeadIsSealed(t *testing.T) {
	typ := reflect.TypeOf(GatedHead{})
	for i := range typ.NumField() {
		if f := typ.Field(i); f.IsExported() {
			t.Errorf("GatedHead.%s is exported; the capability must be unforgeable and immutable outside the package", f.Name)
		}
	}
	for _, name := range []string{"Repo", "BaseRef", "SourceHeadSHA"} {
		m, ok := typ.MethodByName(name)
		if !ok {
			t.Errorf("GatedHead has no %s accessor", name)
			continue
		}
		if m.Type.NumOut() != 1 || m.Type.Out(0).Kind() != reflect.String {
			t.Errorf("GatedHead.%s does not read a single string", name)
		}
	}
	// Identity is the one non-string accessor: it must hand back the
	// sealed Identity value, never anything a holder could mutate into a
	// different branch derivation.
	m, ok := typ.MethodByName("Identity")
	if !ok || m.Type.NumOut() != 1 || m.Type.Out(0) != reflect.TypeOf(Identity{}) {
		t.Errorf("GatedHead.Identity does not read a single Identity")
	}
}

// TestNoExportedGatedHeadMint is the half of the seal reflection cannot
// see. Sealing the fields stops a caller from building a GatedHead, but
// an exported function that returns one would hand out the capability
// just as effectively, and no property of the type would change. So read
// the package's own production sources and fail on any exported function
// or method yielding a GatedHead. This has to hold for every future
// change to the package, not only for the one that introduced it, which
// is why it is a test and not a review habit: an added exported mint
// otherwise leaves the whole suite green.
func TestNoExportedGatedHeadMint(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() || fn.Type.Results == nil {
				continue
			}
			// A method on an unexported receiver is unreachable from
			// outside the package, so it cannot hand anything out.
			if fn.Recv != nil && !exportedReceiver(fn.Recv) {
				continue
			}
			for _, res := range fn.Type.Results.List {
				if mentionsGatedHead(res.Type) {
					t.Errorf(
						"%s: exported %s returns a GatedHead; the capability must be mintable only on the gated publication path",
						fset.Position(fn.Pos()), fn.Name.Name,
					)
				}
			}
		}
	}
	// A silent zero-file scan would make this test vacuously pass.
	if scanned == 0 {
		t.Fatal("scanned no production sources")
	}
}

// exportedReceiver reports whether a method's receiver type is reachable
// from outside the package.
func exportedReceiver(recv *ast.FieldList) bool {
	if len(recv.List) == 0 {
		return false
	}
	found := false
	ast.Inspect(recv.List[0].Type, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			found = id.IsExported()
		}
		return !found
	})
	return found
}

// mentionsGatedHead reports whether a result type names GatedHead
// anywhere in it, so a pointer, slice, map, or channel of the capability
// counts as handing it out.
func mentionsGatedHead(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "GatedHead" {
			found = true
		}
		return !found
	})
	return found
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
	if _, err := remote.transport.PushHead(t.Context(), Checkout{dir: stray, baseSHA: strayHead}, testGatedHead(t, remote.transport, testIdentityInput(remote.repo, strayHead))); err == nil {
		t.Error("PushHead accepted a checkout the transport never minted")
	}
	forged := Checkout{dir: stray, baseSHA: strayHead, baseRef: "main", repo: remote.repo, owner: remote.transport}
	if _, err := remote.transport.PushHead(t.Context(), forged, testGatedHead(t, remote.transport, testIdentityInput(remote.repo, strayHead))); err == nil {
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
	if _, err := remote.transport.PushHead(t.Context(), co, testGatedHead(t, remote.transport, testIdentityInput("owner/other", head))); err == nil {
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
	if _, err := remote.transport.PushHead(t.Context(), co, testGatedHead(t, remote.transport, in)); err == nil {
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
	if _, err := remote.transport.PushHead(t.Context(), forged, testGatedHead(t, remote.transport, testIdentityInput(remote.repo, head))); err == nil {
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
	if _, err := remote.transport.PushHead(t.Context(), co, testGatedHead(t, remote.transport, testIdentityInput(remote.repo, orphan))); err == nil {
		t.Error("PushHead accepted a head that does not descend from the enforced base")
	}
}
