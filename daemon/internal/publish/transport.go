package publish

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
// working-tree content, the base anchored against gc.
func (t *Transport) FetchBase(ctx context.Context, repo, baseRef, baseSHA, dir string) (Checkout, error) {
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
	// rejected call never causes an audited mint or a live credential.
	tok, err := t.tokens.Token(ctx, repo)
	if err != nil {
		return Checkout{}, err
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
	// this checkout was materialized from.
	if _, _, err := r.run(ctx, nil, "config", transportRepoKey, repo); err != nil {
		return Checkout{}, err
	}
	materialized = true
	return Checkout{dir: dir, baseSHA: baseSHA, baseRef: baseRef, repo: repo, owner: t}, nil
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
// from one IdentityInput: the repository is in.Repo, the head is
// in.SourceHeadSHA, and the branch is the derived identity's
// BranchName, so a branch belonging to some other candidate is
// unrepresentable rather than merely checked. The identity's
// repository and base ref must be the ones the Checkout capability
// was actually fetched from, so a publication cannot target a branch
// the enforced base was never reachable from.
//
// PushHead is a transport, not an authorization boundary: it proves
// what it can see (checkout provenance, repository, base, ancestry),
// never that the candidate is publishable. Publisher.Publish owns the
// authorization, artifact, and drift gates, and the engine composes
// the two in that order (#236); calling PushHead for a candidate that
// has not passed Publish's gates creates a ref those gates would have
// refused.
func (t *Transport) PushHead(ctx context.Context, co Checkout, in IdentityInput) (PushResult, error) {
	id, err := DeriveIdentity(in)
	if err != nil {
		return PushResult{}, err
	}
	repo := in.Repo
	headSHA := in.SourceHeadSHA
	ref, err := parseTransportRepo(repo)
	if err != nil {
		return PushResult{}, err
	}
	if co.owner != t {
		return PushResult{}, errors.New("push: checkout was not materialized by this transport instance; run FetchBase on it")
	}
	if in.Repo != co.repo {
		return PushResult{}, fmt.Errorf("identity targets repository %q, checkout was fetched from %q: %w", in.Repo, co.repo, ErrGitTransport)
	}
	if in.BaseRef != co.baseRef {
		return PushResult{}, fmt.Errorf("identity targets base ref %q, checkout was fetched from %q: %w", in.BaseRef, co.baseRef, ErrGitTransport)
	}
	branch := id.BranchName()
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
	// The token is minted only after every checkout re-gate has
	// passed, so a rejected checkout never causes an audited mint or a
	// live credential.
	tok, err := t.tokens.Token(ctx, repo)
	if err != nil {
		return PushResult{}, err
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
