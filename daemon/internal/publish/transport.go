package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrRemoteMissingBase is returned when the managed repository does
// not hold the requested base commit reachable from the requested base
// ref, so no daemon-owned checkout can be materialized at it.
var ErrRemoteMissingBase = errors.New("remote does not hold the requested base commit")

// transportBaseRef is where FetchBase lands the remote base branch,
// and transportAnchorRef what it points at the enforced base commit so
// gc can never collect it out from under the importer.
const (
	transportBaseRef   = "refs/freeside/base"
	transportAnchorRef = "refs/freeside/import-base"
)

// Transport is the publish lane's git network surface: it materializes
// a daemon-owned base checkout from the managed repository and places
// the re-authored candidate head on it, authenticated per invocation
// with a short-lived installation token from the TokenSource. It is
// deliberately create-only on exactly one ref; every other remote
// effect is refused (plan §5.9's daemon-side push). The transport
// never retries internally: each operation is one attempt (plus
// PushHead's single lease re-observation) that fails loud, and the
// publication recovery drain owns retry.
type Transport struct {
	tokens     TokenSource
	gitPath    string
	remoteBase string
	scheme     string
	// publisher is the one authority whose gate verdicts this transport
	// honours, claimed once by AuthorizePublisher and never replaceable.
	// It lives here, on the credential-bearing side, precisely so that
	// holding the transport does not confer the ability to nominate the
	// authority: a caller who could set this could nominate a publisher
	// of its own making, which is the whole bypass this closes.
	publisher *Publisher
}

// TransportOptions carries the two injectable transport inputs.
// RemoteBase exists so live tests can point at a forge fixture; it
// must be an https URL — the runner's protocol policy admits nothing
// else in production, and tests inside the package construct their
// file-scheme Transport directly.
type TransportOptions struct {
	GitPath    string // git binary; default "git" from PATH
	RemoteBase string // base URL repositories hang off; default "https://github.com"
}

// NewTransport validates and wires the production (https-only)
// transport.
func NewTransport(ts TokenSource, opts TransportOptions) (*Transport, error) {
	if ts == nil {
		return nil, errors.New("transport: nil token source")
	}
	gitPath := opts.GitPath
	if gitPath == "" {
		gitPath = "git"
	}
	remoteBase := opts.RemoteBase
	if remoteBase == "" {
		remoteBase = "https://github.com"
	}
	if err := validRemoteBase(remoteBase); err != nil {
		return nil, err
	}
	return &Transport{
		tokens:     ts,
		gitPath:    gitPath,
		remoteBase: strings.TrimSuffix(remoteBase, "/"),
		scheme:     "https",
	}, nil
}

// validRemoteBase admits only a plain https origin (optionally with a
// path prefix). The parse, rather than a prefix test, is what keeps
// the transport's guarantee that a repository URL is safe argv and
// error material: userinfo in the base would put a credential on
// every network git process's argv and into TransportGitError.Args,
// and a query or fragment would smuggle uninterpreted material into a
// URL the daemon claims to have composed itself.
//
// No message here renders any part of the rejected value. A credential
// can ride any URL component — userinfo, an opaque body, a query, a
// fragment — so redacting the component a leak was last found in only
// moves the problem to the next one; two rounds of that established
// the real boundary. The operator supplied this value and does not
// need it echoed to know which one it is, so the defect is named and
// the value is dropped entirely, including the url.Parse error, whose
// url.Error wrapper embeds the input.
func validRemoteBase(remoteBase string) error {
	parsed, err := url.Parse(remoteBase)
	if err != nil {
		return errors.New("transport remote base is not a URL")
	}
	switch {
	case parsed.Scheme != "https":
		return errors.New("transport remote base is not https")
	case parsed.User != nil:
		return errors.New("transport remote base carries userinfo credentials")
	case parsed.Host == "":
		return errors.New("transport remote base has no host")
	case parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "":
		return errors.New("transport remote base carries a query or fragment")
	case parsed.Opaque != "":
		return errors.New("transport remote base is not a hierarchical URL")
	}
	return nil
}

// AuthorizePublisher claims this transport's single publication
// authority for p: from here on, PushHead honours a GatedHead only if p
// issued it. The claim is one-shot by design. A caller holding the
// transport must not be able to nominate its own authority — Publisher's
// collaborators are exported interfaces and its approved-recipe set is a
// caller argument, so a nominable authority is no authority at all — and
// a re-bindable one is nominable by whoever binds last. First claim wins,
// every later claim fails loudly, so a second publisher cannot displace
// the daemon's own, and an attempt to claim it out from under the wiring
// surfaces as a startup error rather than a silent takeover.
//
// Call it once, at wiring, before either party is used. A transport with
// no claimed authority publishes nothing: PushHead refuses every
// capability, so a wiring that forgets fails closed.
func (t *Transport) AuthorizePublisher(p *Publisher) error {
	if p == nil {
		return errors.New("transport: nil publication authority")
	}
	if t.publisher != nil {
		return fmt.Errorf("transport already serves a publication authority: %w", ErrTransportAuthorityClaimed)
	}
	if p.transport != nil {
		return fmt.Errorf("publisher already gates for another transport: %w", ErrTransportAuthorityClaimed)
	}
	t.publisher = p
	p.transport = t
	return nil
}

// Checkout is a capability, not a record: it says "this transport
// materialized this directory from this repository at this base
// commit on this base ref", and only FetchBase can say it. Every
// field is unexported and read-only through accessors, so a caller
// can neither construct one nor alter one it holds — a copy is
// necessarily identical, and no combination of caller state can make
// PushHead operate on a directory, repository, base, or base ref
// other than the ones actually fetched. A Checkout does not survive
// the process; after a restart, re-run FetchBase.
type Checkout struct {
	dir     string
	baseSHA string
	baseRef string
	repo    string
	// repositoryID is the canonical numeric identity the trusted binding
	// held for repo when the checkout was materialized. The owner/name
	// alone is not identity: a name can be transferred and reused, so a
	// policy decision keyed on it can silently follow the name onto a
	// different repository (domain.BaseRevision records the same rule).
	repositoryID int64
	// owner is the Transport that minted this checkout. Provenance is
	// per-instance, not merely "some transport": two Transports can
	// point at different endpoints, and a checkout fetched from one
	// must never be pushed by the other, however alike their repo and
	// base-ref labels look.
	owner *Transport
}

// Dir is the daemon-owned checkout directory (the importer's
// local-checkout path).
func (c Checkout) Dir() string { return c.dir }

// BaseSHA is the exact base commit the checkout is bound to.
func (c Checkout) BaseSHA() string { return c.baseSHA }

// BaseRef is the remote branch the base was fetched from.
func (c Checkout) BaseRef() string { return c.baseRef }

// Repo is the managed repository the checkout was materialized from.
func (c Checkout) Repo() string { return c.repo }

// RepositoryID is the canonical numeric repository identity the trusted
// binding held for Repo when the checkout was materialized.
func (c Checkout) RepositoryID() int64 { return c.repositoryID }

// transportRepoIDPath is where FetchBase stamps the canonical numeric
// repository ID, beside the owner/name mark in the checkout's config.
// The ID is a separate file rather than a config key because every
// consumer's config gate is an exact-key allowlist (assertPristineConfig
// here, and independently the ward seeding gate, which deliberately
// duplicates these literals rather than importing this package); the
// file influences no git behavior, so its integrity is proved by the
// explicit re-gates that read it, not by config hygiene.
const transportRepoIDPath = ".git/freeside-repository-id"

// maxTransportRepoIDBytes bounds the ID binding read: an int64's
// canonical decimal fits well inside it, so anything larger is not a
// daemon-authored stamp.
const maxTransportRepoIDBytes = 64

// stampRepoIDBinding writes the canonical decimal form the re-gates
// (and ward's seeding gate) parse back: one line, one trailing newline.
func stampRepoIDBinding(dir string, id int64) error {
	path := filepath.Join(dir, filepath.FromSlash(transportRepoIDPath))
	if err := os.WriteFile(path, []byte(strconv.FormatInt(id, 10)+"\n"), 0o600); err != nil {
		return fmt.Errorf("stamp repository id binding: %w", err)
	}
	return nil
}

// readRepoIDBinding re-reads a checkout's stamped repository ID,
// failing closed on an absent, oversized, or non-canonical binding. A
// checkout materialized before the stamp existed therefore cannot pass
// on its name alone. No refusal echoes the file's content or the parse
// error (whose prose embeds the input): the file lives in a directory
// later pipeline stages write into, so rejected bytes are not safe
// error material.
func readRepoIDBinding(dir string) (int64, error) {
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(transportRepoIDPath))) //nolint:gosec // daemon-owned checkout path
	if err != nil {
		return 0, fmt.Errorf("checkout %s carries no repository id binding: %w", dir, ErrGitTransport)
	}
	defer f.Close() //nolint:errcheck // read-only handle
	data, err := io.ReadAll(io.LimitReader(f, maxTransportRepoIDBytes+1))
	if err != nil || len(data) > maxTransportRepoIDBytes {
		return 0, fmt.Errorf("checkout %s repository id binding could not be read: %w", dir, ErrGitTransport)
	}
	text := strings.TrimSuffix(string(data), "\n")
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 || text != strconv.FormatInt(id, 10) {
		return 0, fmt.Errorf("checkout %s carries a malformed repository id binding: %w", dir, ErrGitTransport)
	}
	return id, nil
}

// PushResult reports one PushHead outcome. Created is false when the
// remote already held the identical head, so the call had no remote
// effect (the effectively-once property the publication drain
// depends on).
type PushResult struct {
	Created bool
}

// FetchBase materializes a daemon-owned checkout of the managed
// repository at exactly baseSHA, failing closed with
// ErrRemoteMissingBase unless the remote's baseRef branch holds that
// commit. dir must not exist yet: FetchBase claims it atomically, so
// concurrent calls on one path cannot interleave and a failed call
// removes only what it claimed. The result has the shape the
// importer's base enforcement expects: HEAD detached at the base, no
// working-tree content, the base anchored against gc — stamped with the
// managed repository's name and canonical numeric identity.
func (t *Transport) FetchBase(ctx context.Context, repo, baseRef, baseSHA, dir string) (Checkout, error) {
	return t.fetchBase(ctx, repo, baseRef, baseSHA, dir, false)
}

// FetchBaseWorktree is FetchBase plus the materialized working tree, for the
// one consumer that needs the files rather than the objects: ward seeds a
// writer's workspace from this directory and its observer proves the raw
// worktree byte-for-byte against HEAD, so the repository-only shape FetchBase
// produces is rejected as dirty (every tracked path missing) before any writer
// starts. The import lane keeps the lighter shape: it applies an export over
// the checkout and must not inherit files nobody put there.
func (t *Transport) FetchBaseWorktree(ctx context.Context, repo, baseRef, baseSHA, dir string) (Checkout, error) {
	return t.fetchBase(ctx, repo, baseRef, baseSHA, dir, true)
}

// RetainWorktree claims a fresh dest and copies source's bounded repository
// before materializing commitSHA's raw tree there. It never modifies source and
// refuses an existing dest, so a caller-nominated path cannot turn raw
// materialization into a recursive deletion primitive. The commit must already
// exist in source; this method performs no fetch and never consults a remote.
// Only regular 100644/100755 tree entries are admitted.
func (t *Transport) RetainWorktree(ctx context.Context, source, dest, commitSHA string) (retainedErr error) {
	if !validCommitSHA(commitSHA) {
		return fmt.Errorf("commit %q is not a full commit SHA", commitSHA)
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve source checkout: %w", err)
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve physical source checkout: %w", err)
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("resolve retained checkout: %w", err)
	}
	destParent, err := filepath.EvalSymlinks(filepath.Dir(dest))
	if err != nil {
		return fmt.Errorf("resolve physical retained-checkout parent: %w", err)
	}
	dest = filepath.Join(destParent, filepath.Base(dest))
	if within, relErr := filepath.Rel(source, dest); relErr != nil {
		return fmt.Errorf("compare retained checkout paths: %w", relErr)
	} else if within == "." || (within != ".." && !strings.HasPrefix(within, ".."+string(filepath.Separator))) {
		return fmt.Errorf("retained checkout %q is inside source %q: %w", dest, source, ErrGitTransport)
	}
	scratch, err := os.MkdirTemp("", "freeside-transport-")
	if err != nil {
		return fmt.Errorf("create transport scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort scratch cleanup
	r, err := newNetRunner(t.gitPath, scratch, t.scheme)
	if err != nil {
		return err
	}
	if err := r.pinRepo(ctx, source); err != nil {
		return err
	}
	if err := r.assertPristineConfig(ctx); err != nil {
		return err
	}
	if _, err := validatedMaterializationTree(ctx, r, commitSHA); err != nil {
		return err
	}
	if err := validateRetainedRepository(ctx, source); err != nil {
		return err
	}
	if err := os.Mkdir(dest, 0o700); err != nil {
		return fmt.Errorf("claim retained checkout: %w", err)
	}
	retained := false
	defer func() {
		if !retained {
			if cleanupErr := os.RemoveAll(dest); retainedErr == nil && cleanupErr != nil {
				retainedErr = fmt.Errorf("clean failed retained checkout: %w", cleanupErr)
			}
		}
	}()
	if err := copyRetainedRepository(ctx, source, dest); err != nil {
		return fmt.Errorf("copy retained checkout: %w", err)
	}
	destRunner, err := newNetRunner(t.gitPath, scratch, t.scheme)
	if err != nil {
		return err
	}
	if err := destRunner.pinRepo(ctx, dest); err != nil {
		return err
	}
	if err := destRunner.assertPristineConfig(ctx); err != nil {
		return err
	}
	if err := materializeWorktree(ctx, destRunner, dest, commitSHA); err != nil {
		return err
	}
	retained = true
	return nil
}

// The retained repository lands once on the host and then twice more under
// Ward's seed budgets. A source object database is not constrained by the
// candidate tree's blob budget, so copying it with os.CopyFS lets unrelated
// history consume unbounded disk before Ward can inspect it. Match the
// materializer's established ceilings and fail before destination claim when
// the repository alone cannot fit within them. The copy pass applies the same
// accounting again so a changed source cannot turn the preflight into a stale
// promise.
const (
	maxRetainedRepositoryBytes   = maxMaterializationBytes
	maxRetainedRepositoryEntries = maxMaterializationEntries
)

func validateRetainedRepository(ctx context.Context, source string) error {
	return walkRetainedRepository(ctx, source, nil)
}

func copyRetainedRepository(ctx context.Context, source, dest string) error {
	return walkRetainedRepository(ctx, source, func(
		path, rel string, info os.FileInfo,
	) error {
		target := filepath.Join(dest, ".git", rel)
		if info.IsDir() {
			if err := os.Mkdir(target, info.Mode().Perm()); err != nil {
				return err
			}
			return nil
		}
		return copyRetainedRepositoryFile(ctx, path, target, info)
	})
}

func walkRetainedRepository(
	ctx context.Context,
	source string,
	visit func(path, rel string, info os.FileInfo) error,
) error {
	root := filepath.Join(source, ".git")
	remaining := maxRetainedRepositoryBytes
	entries := 0
	return filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > maxRetainedRepositoryEntries {
			return fmt.Errorf(
				"repository exceeds %d retained entries: %w",
				maxRetainedRepositoryEntries, ErrMaterializationRefused,
			)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("repository carries an irregular entry: %w", ErrMaterializationRefused)
		}
		if info.Mode().IsRegular() {
			if info.Size() < 0 || info.Size() > remaining {
				return fmt.Errorf(
					"repository exceeds %d retained bytes: %w",
					maxRetainedRepositoryBytes, ErrMaterializationRefused,
				)
			}
			remaining -= info.Size()
		}
		if visit == nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return visit(path, rel, info)
	})
}

type retainedRepositoryReader struct {
	ctx context.Context
	r   io.Reader
}

func (r retainedRepositoryReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func copyRetainedRepositoryFile(
	ctx context.Context, source, dest string, expected os.FileInfo,
) (retErr error) {
	in, err := os.Open(source) //nolint:gosec // path is rooted in the validated daemon-owned repository walk
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only handle
	actual, err := in.Stat()
	if err != nil {
		return err
	}
	if !actual.Mode().IsRegular() || actual.Size() != expected.Size() {
		return fmt.Errorf("repository changed during retention: %w", ErrMaterializationRefused)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, expected.Mode().Perm()) //nolint:gosec // destination is under a fresh daemon-owned checkout
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); retErr == nil && closeErr != nil {
			retErr = closeErr
		}
	}()
	written, err := io.Copy(out, io.LimitReader(
		retainedRepositoryReader{ctx: ctx, r: in}, expected.Size()+1,
	))
	if err != nil {
		return err
	}
	if written != expected.Size() {
		return fmt.Errorf("repository changed during retention: %w", ErrMaterializationRefused)
	}
	return nil
}

// withoutGitIndexFile drops every GIT_INDEX_FILE entry so a replacement is
// unambiguous: duplicate keys in one environment resolve differently across
// libc implementations, and the wrong index here would leave the seeded
// worktree looking dirty to the writer.
func withoutGitIndexFile(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_INDEX_FILE=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (t *Transport) fetchBase(
	ctx context.Context, repo, baseRef, baseSHA, dir string, withWorktree bool,
) (Checkout, error) {
	ref, err := parseTransportRepo(repo)
	if err != nil {
		return Checkout{}, err
	}
	if err := ValidateBranchName(baseRef); err != nil {
		return Checkout{}, fmt.Errorf("base ref %q: %w", baseRef, err)
	}
	if !validCommitSHA(baseSHA) {
		return Checkout{}, fmt.Errorf("base %q is not a full commit SHA", baseSHA)
	}
	// Every invocation runs with the transport scratch as its working
	// directory, so a relative checkout path would be claimed here but
	// created under the scratch. Resolve once, up front, and let the
	// capability carry the absolute path.
	dir, err = filepath.Abs(dir)
	if err != nil {
		return Checkout{}, fmt.Errorf("resolve checkout dir: %w", err)
	}
	// Atomically claim the checkout directory: Mkdir succeeds for
	// exactly one caller, so a concurrent FetchBase on the same path
	// cannot interleave with this one, and the failure cleanup below
	// only ever removes a directory this call created — never a
	// repository some other attempt materialized.
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return Checkout{}, fmt.Errorf("create checkout parent: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Checkout{}, fmt.Errorf("claim checkout dir: %w", err)
	}
	// On any later failure, remove the claimed directory so a
	// transient fetch failure does not wedge every retry of the same
	// checkout path.
	materialized := false
	defer func() {
		if !materialized {
			os.RemoveAll(dir) //nolint:errcheck,gosec // best-effort unwedge of a failed materialization this call claimed
		}
	}()
	scratch, err := os.MkdirTemp("", "freeside-transport-")
	if err != nil {
		return Checkout{}, fmt.Errorf("create transport scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort scratch cleanup
	r, err := newNetRunner(t.gitPath, scratch, t.scheme)
	if err != nil {
		return Checkout{}, err
	}
	if _, _, err := r.run(ctx, nil, "init", "--object-format=sha1", dir); err != nil {
		return Checkout{}, err
	}
	if err := r.pinRepo(ctx, dir); err != nil {
		return Checkout{}, err
	}
	if err := r.assertPristineConfig(ctx); err != nil {
		return Checkout{}, err
	}
	// The token is minted only after every local gate has passed, so a
	// call rejected on its own arguments never causes an audited mint or
	// a live credential. The identity check below necessarily follows
	// the mint (the trusted binding arrives with the token); its refusal
	// still precedes every authenticated operation.
	tok, err := t.tokens.Token(ctx, repo)
	if err != nil {
		return Checkout{}, err
	}
	// The trusted binding must name the repository's canonical identity;
	// a checkout stamped without one would later be refused anyway, so
	// fail before the network fetch spends anything.
	if tok.RepositoryID <= 0 {
		return Checkout{}, fmt.Errorf("trusted binding for %s carries no canonical repository id: %w", repo, ErrGitTransport)
	}
	url := t.repoURL(ref)
	remoteRef := "refs/heads/" + baseRef
	if _, _, err := r.runAuthed(ctx, tok, fetchArgs(url, baseRef)...); err != nil {
		// ErrRemoteMissingBase is a definitive verdict a caller may act
		// on (the base drifted; do not retry), so it is never inferred
		// from the failure's prose, which a hostile remote can write.
		// One bounded structured observation answers the question
		// authoritatively; anything else stays a plain transport
		// failure, which the drain may retry.
		head, lsErr := lsRemoteHead(ctx, r, tok, url, remoteRef)
		if lsErr == nil && head == "" {
			return Checkout{}, fmt.Errorf("remote %s has no branch %s: %w: %w", repo, baseRef, ErrRemoteMissingBase, err)
		}
		return Checkout{}, err
	}
	// The exact-base binding: the requested commit must be present and
	// reachable from the fetched base branch. "The remote holds it" is
	// defined as reachable-from-baseRef, never as whatever a bare
	// SHA-in-want request would happen to surface.
	out, _, err := r.run(ctx, nil, "rev-parse", "--verify", baseSHA+"^{commit}")
	if err != nil {
		return Checkout{}, fmt.Errorf("base %s is not on remote branch %s: %w: %w", baseSHA, baseRef, ErrRemoteMissingBase, err)
	}
	if got := strings.TrimSpace(string(out)); got != baseSHA {
		return Checkout{}, fmt.Errorf("base %s resolved to %s: %w", baseSHA, got, ErrRemoteMissingBase)
	}
	if _, _, err := r.run(ctx, nil, "merge-base", "--is-ancestor", baseSHA, transportBaseRef); err != nil {
		return Checkout{}, fmt.Errorf("base %s is not reachable from remote branch %s: %w: %w", baseSHA, baseRef, ErrRemoteMissingBase, err)
	}
	if _, _, err := r.run(ctx, nil, "update-ref", transportAnchorRef, baseSHA); err != nil {
		return Checkout{}, err
	}
	if _, _, err := r.run(ctx, nil, "update-ref", "--no-deref", "HEAD", baseSHA); err != nil {
		return Checkout{}, err
	}
	// The repo binding PushHead re-gates against: a daemon-authored
	// mark in the checkout's own config naming the managed repository
	// this checkout was materialized from, plus the canonical numeric
	// identity the trusted binding resolved that name to, so a name
	// later transferred onto a different repository cannot satisfy the
	// re-gates on the name alone.
	if _, _, err := r.run(ctx, nil, "config", transportRepoKey, repo); err != nil {
		return Checkout{}, err
	}
	if err := stampRepoIDBinding(dir, tok.RepositoryID); err != nil {
		return Checkout{}, err
	}
	if withWorktree {
		if err := materializeWorktree(ctx, r, dir, baseSHA); err != nil {
			return Checkout{}, err
		}
	}
	materialized = true
	return Checkout{dir: dir, baseSHA: baseSHA, baseRef: baseRef, repo: repo, repositoryID: tok.RepositoryID, owner: t}, nil
}

// PushHead places the candidate head on the managed repository's
// derived publication branch, create-only and effectively once: an
// absent branch is created, the identical existing head is a
// converged no-op with no remote effect, and any other remote state
// is ErrPublicationConflict. The create-only lease means the push can
// never move an existing ref — not even fast-forward — and --atomic
// with the single fully qualified refspec means no other ref can
// change.
//
// The target repository, the pushed commit, and the branch all come
// from one GatedHead: the repository is its Repo, the head is its
// SourceHeadSHA, and the branch is its identity's BranchName, so a
// branch belonging to some other candidate is unrepresentable rather
// than merely checked. That repository and base ref must be the ones
// the Checkout capability was actually fetched from, so a publication
// cannot target a branch the enforced base was never reachable from.
//
// PushHead is a transport, not an authorization boundary: it proves
// what it can see (checkout provenance, repository, base, ancestry),
// never that the candidate is publishable. Publisher owns the
// authorization, artifact, and drift gates and evaluates them exactly
// once; this call requires the GatedHead that path mints, so a
// candidate those gates would have refused cannot reach a ref (#288).
// Requiring the capability is not re-running the gates: PushHead reads
// no policy and reaches no authorization state.
//
// Two capabilities, two distinct claims, both per-instance: the Checkout
// says this transport fetched this base, the GatedHead says the publisher
// bound to this transport cleared this head. Neither substitutes for the
// other.
func (t *Transport) PushHead(ctx context.Context, co Checkout, gh GatedHead) (PushResult, error) {
	if !gh.gated {
		return PushResult{}, ErrUngatedPublication
	}
	// Per-instance, exactly like the Checkout's provenance below: the only
	// gate this transport honours is the one its claimed authority issued.
	// Sealing the type alone would prove no more than "some Publisher
	// gated this", and Publisher's collaborators are exported interfaces,
	// so a caller could assemble a permissive one and have its verdict
	// travel here. The authority is read from this transport, never from
	// the capability, so a forged or foreign issuer cannot vouch for
	// itself.
	if t.publisher == nil {
		return PushResult{}, fmt.Errorf(
			"transport has no claimed publication authority: %w", ErrUngatedPublication,
		)
	}
	if gh.issuer != t.publisher || gh.owner != t {
		return PushResult{}, fmt.Errorf(
			"publication gate was issued by a publisher this transport does not serve: %w",
			ErrUngatedPublication,
		)
	}
	repo := gh.repo
	headSHA := gh.headSHA
	ref, err := parseTransportRepo(repo)
	if err != nil {
		return PushResult{}, err
	}
	if co.owner != t {
		return PushResult{}, errors.New("push: checkout was not materialized by this transport instance; run FetchBase on it")
	}
	if repo != co.repo {
		return PushResult{}, fmt.Errorf("identity targets repository %q, checkout was fetched from %q: %w", repo, co.repo, ErrGitTransport)
	}
	if gh.baseRef != co.baseRef {
		return PushResult{}, fmt.Errorf("identity targets base ref %q, checkout was fetched from %q: %w", gh.baseRef, co.baseRef, ErrGitTransport)
	}
	branch := gh.identity.BranchName()
	if !validBranchName(branch) {
		return PushResult{}, fmt.Errorf("branch %q is not a valid branch name", branch)
	}
	if !validCommitSHA(headSHA) {
		return PushResult{}, fmt.Errorf("head %q is not a full commit SHA", headSHA)
	}
	if !validCommitSHA(co.baseSHA) {
		return PushResult{}, fmt.Errorf("checkout base %q is not a full commit SHA", co.baseSHA)
	}
	scratch, err := os.MkdirTemp("", "freeside-transport-")
	if err != nil {
		return PushResult{}, fmt.Errorf("create transport scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort scratch cleanup
	r, err := newNetRunner(t.gitPath, scratch, t.scheme)
	if err != nil {
		return PushResult{}, err
	}
	if err := r.pinRepo(ctx, co.dir); err != nil {
		return PushResult{}, err
	}
	bound, _, err := r.run(ctx, nil, "config", "--get", transportRepoKey)
	if err != nil {
		return PushResult{}, fmt.Errorf("checkout %s carries no transport repo binding: %w", co.dir, err)
	}
	if got := strings.TrimSpace(string(bound)); got != repo {
		return PushResult{}, fmt.Errorf("checkout is bound to repository %q, push targets %q: %w", got, repo, ErrGitTransport)
	}
	// Stamp integrity: the ID on disk must be the one this transport
	// stamped into the capability. Disk alone would trust whatever a
	// later pipeline stage wrote there; the capability alone would leave
	// the artifact ward's seeding gate consumes unchecked.
	stampedID, err := readRepoIDBinding(co.dir)
	if err != nil {
		return PushResult{}, err
	}
	if stampedID != co.repositoryID {
		return PushResult{}, fmt.Errorf(
			"checkout is stamped with repository id %d, transport fetched it as %d: %w",
			stampedID, co.repositoryID, ErrGitTransport,
		)
	}
	head, _, err := r.run(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		return PushResult{}, err
	}
	if got := strings.TrimSpace(string(head)); got != co.baseSHA {
		return PushResult{}, fmt.Errorf("checkout HEAD %s is not the enforced base %s: %w", got, co.baseSHA, ErrGitTransport)
	}
	out, _, err := r.run(ctx, nil, "rev-parse", "--verify", headSHA+"^{commit}")
	if err != nil {
		return PushResult{}, fmt.Errorf("head %s is not a commit in the checkout: %w", headSHA, err)
	}
	if got := strings.TrimSpace(string(out)); got != headSHA {
		return PushResult{}, fmt.Errorf("head %s resolved to %s: %w", headSHA, got, ErrGitTransport)
	}
	if _, _, err := r.run(ctx, nil, "merge-base", "--is-ancestor", co.baseSHA, headSHA); err != nil {
		return PushResult{}, fmt.Errorf("head %s does not descend from the enforced base %s: %w", headSHA, co.baseSHA, err)
	}
	if err := r.assertPristineConfig(ctx); err != nil {
		return PushResult{}, err
	}
	// The token is acquired only after every local checkout re-gate has
	// passed, so a checkout rejected on its own state never causes an
	// audited mint or a live credential. The one re-gate after it is
	// trust continuity below, which cannot run earlier: the trusted
	// binding arrives with the token. A refusal there still precedes
	// every authenticated network operation, so the credential is never
	// exposed for a refused push.
	tok, err := t.tokens.Token(ctx, repo)
	if err != nil {
		return PushResult{}, err
	}
	// Trust continuity: the identity the checkout was fetched under must
	// still be the one the trusted binding resolves the name to. A name
	// transferred and reused between fetch and push resolves to a
	// different repository's ID and is refused here, instead of the push
	// silently following the name onto that repository.
	if tok.RepositoryID <= 0 {
		return PushResult{}, fmt.Errorf("trusted binding for %s carries no canonical repository id: %w", repo, ErrGitTransport)
	}
	if co.repositoryID != tok.RepositoryID {
		return PushResult{}, fmt.Errorf(
			"checkout is bound to repository id %d, trusted binding for %q is %d: %w",
			co.repositoryID, repo, tok.RepositoryID, ErrGitTransport,
		)
	}
	url := t.repoURL(ref)
	remoteRef := "refs/heads/" + branch
	remote, err := lsRemoteHead(ctx, r, tok, url, remoteRef)
	if err != nil {
		return PushResult{}, err
	}
	switch remote {
	case "":
		// Absent: this push creates it.
	case headSHA:
		return PushResult{Created: false}, nil
	default:
		return PushResult{}, fmt.Errorf("branch %s exists at %s, candidate is %s: %w", branch, remote, headSHA, ErrPublicationConflict)
	}
	if _, _, err := r.runAuthed(ctx, tok, pushArgs(url, headSHA, branch)...); err != nil {
		var tge *TransportGitError
		if errors.As(err, &tge) && tge.Refusal == RefusalStaleLease {
			// The ref appeared between observation and push. One
			// bounded re-observation distinguishes a concurrent
			// identical publication (converged) from foreign state; a
			// failed re-observation stays a transport failure, never a
			// conflict verdict the state was not observed to support.
			remote, lsErr := lsRemoteHead(ctx, r, tok, url, remoteRef)
			if lsErr != nil {
				return PushResult{}, fmt.Errorf("branch %s: push lease refused and re-observation failed: %w", branch, lsErr)
			}
			if remote == headSHA {
				return PushResult{Created: false}, nil
			}
			return PushResult{}, fmt.Errorf("branch %s changed during push: %w: %w", branch, ErrPublicationConflict, err)
		}
		return PushResult{}, err
	}
	return PushResult{Created: true}, nil
}

// repoURL renders the managed repository's remote URL. It never
// carries credentials: authentication rides the per-invocation header
// config, so the URL is safe argv and error material.
func (t *Transport) repoURL(ref repoRef) string {
	return t.remoteBase + "/" + ref.path() + ".git"
}

// fetchArgs is the fixed fetch argument vector: single branch, no
// tags, landing on the private base ref. Pure so the golden test pins
// it byte-for-byte.
func fetchArgs(url, baseRef string) []string {
	return []string{"fetch", "--no-tags", url, "refs/heads/" + baseRef + ":" + transportBaseRef}
}

// pushArgs is the fixed push argument vector. The empty
// --force-with-lease expectation means the remote ref must not exist,
// so the push can only create at the protocol level, closing the
// observe/push race; --atomic plus the single fully qualified refspec
// pins the blast radius to exactly one ref. Pure so the golden test
// pins it byte-for-byte.
func pushArgs(url, headSHA, branch string) []string {
	remoteRef := "refs/heads/" + branch
	return []string{
		"push", "--atomic", "--porcelain", "--no-follow-tags",
		"--force-with-lease=" + remoteRef + ":",
		url, headSHA + ":" + remoteRef,
	}
}

// lsRemoteHead observes one remote branch head, returning "" when the
// branch is absent. The returned object name is remote-supplied and is
// validated before it is compared or rendered anywhere.
func lsRemoteHead(ctx context.Context, r *netRunner, tok InstallationToken, url, remoteRef string) (string, error) {
	out, _, err := r.runAuthed(ctx, tok, "ls-remote", url, remoteRef)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == remoteRef {
			if !validCommitSHA(fields[0]) {
				return "", fmt.Errorf("ls-remote returned invalid object name for %s: %w", remoteRef, ErrGitTransport)
			}
			return fields[0], nil
		}
	}
	return "", nil
}

// parseTransportRepo narrows parseRepo to the character set safe as
// URL and argument material: GitHub owner and repository names, with
// no segment starting with "-" or "." (nothing the transport builds
// can then ever begin with an option or traversal shape).
func parseTransportRepo(repo string) (repoRef, error) {
	ref, err := parseRepo(repo)
	if err != nil {
		return repoRef{}, err
	}
	for _, segment := range []string{ref.owner, ref.name} {
		if strings.HasPrefix(segment, "-") || strings.HasPrefix(segment, ".") {
			return repoRef{}, fmt.Errorf("repository %q is not transport-safe", repo)
		}
		for _, c := range segment {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			default:
				return repoRef{}, fmt.Errorf("repository %q is not transport-safe", repo)
			}
		}
	}
	return ref, nil
}

// ValidateRepository applies the exact transport repository grammar before a
// caller commits durable work that will later require FetchBase.
func ValidateRepository(repo string) error {
	_, err := parseTransportRepo(repo)
	return err
}

// validCommitSHA reports whether s is a full lowercase 40-hex sha1
// commit name, the only object-name form the transport ever puts on
// an argument vector.
func validCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ValidateCommitSHA applies the exact full lowercase SHA-1 grammar before a
// caller commits durable work that will later require FetchBase.
func ValidateCommitSHA(sha string) error {
	if !validCommitSHA(sha) {
		return errors.New("not a full lowercase commit SHA")
	}
	return nil
}

// validBranchName is the transport's refname gate: argument-vector
// safety (nothing option-shaped, no second refspec smuggled through a
// colon or whitespace, no traversal) plus git's own check-ref-format
// grammar, so a name this accepts is one git will accept too. Getting
// that second part wrong is not cosmetic: a name that passes here and
// fails in git has already spent an audited token mint by the time
// git rejects it.
//
// The component rules (no empty, dot-leading, dot-trailing, or
// .lock-suffixed segment) apply per slash-separated component, not
// only to the whole name: "release/.candidate" and
// "release/candidate.lock" are invalid refs whose whole-name form
// looks fine.
func validBranchName(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.Contains(name, "@{") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, c := range name {
		if c <= ' ' || c == 0x7f {
			return false
		}
		switch c {
		case ':', '?', '*', '[', '\\', '~', '^':
			return false
		}
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" ||
			strings.HasPrefix(component, ".") ||
			strings.HasSuffix(component, ".") ||
			strings.HasSuffix(component, ".lock") ||
			strings.Contains(component, "..") {
			return false
		}
	}
	return true
}

// ValidateBranchName applies the exact transport refname grammar before a
// caller commits durable work that will later require FetchBase.
func ValidateBranchName(name string) error {
	if !validBranchName(name) {
		return errors.New("not a valid branch name")
	}
	return nil
}
