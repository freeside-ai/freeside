package engine

// This file holds the publication core that production publication and
// the fake lane share: the git publication transport, the artifact store
// boundary, the publication lock, candidate identity, and the helpers both
// paths call.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"golang.org/x/sys/unix"
)

// ErrForeignPublicationCheckout marks a checkout capability that did not
// originate at the configured transport adapter.
var ErrForeignPublicationCheckout = errors.New("publication checkout belongs to a different transport")

// PublicationCheckout is the narrow capability the engine needs from a
// daemon-owned base checkout. The publish package's sealed Checkout and test
// fakes both satisfy it without exposing mutable transport bindings.
type PublicationCheckout interface {
	Dir() string
	BaseSHA() string
	BaseRef() string
	Repo() string
}

// PublicationTransport is the engine-facing composition seam over the
// publish lane's sealed git transport. PushHead takes a publish.GatedHead
// rather than a bare identity: only Publisher's post-gate callback can
// produce one, so the seam itself cannot express an ungated push (#288).
//
// FetchHead and ProspectiveMerge serve a re-entered cycle (issue #502), which
// evaluates a head already on the pull request instead of an export it can
// replay: FetchHead brings that exact commit into the checkout from the
// pull request's branch, and ProspectiveMerge builds the local, never-pushed
// merge of it into the checkout's base.
type PublicationTransport interface {
	FetchBase(ctx context.Context, repo, baseRef, baseSHA, dir string) (PublicationCheckout, error)
	RetainWorktree(ctx context.Context, checkout PublicationCheckout, dest, headSHA string) error
	PushHead(ctx context.Context, checkout PublicationCheckout, gated publish.GatedHead) (publish.PushResult, error)
	FetchHead(ctx context.Context, checkout PublicationCheckout, branch, headSHA string) (publish.HeadFetch, error)
	ProspectiveMerge(ctx context.Context, checkout PublicationCheckout, headSHA string) (string, error)
}

// GitPublicationTransport adapts publish.Transport's sealed Checkout to the
// engine-facing interface while preserving its provenance check.
type GitPublicationTransport struct {
	transport *publish.Transport
}

type gitPublicationCheckout struct {
	checkout publish.Checkout
	owner    *GitPublicationTransport
}

func (c gitPublicationCheckout) Dir() string     { return c.checkout.Dir() }
func (c gitPublicationCheckout) BaseSHA() string { return c.checkout.BaseSHA() }
func (c gitPublicationCheckout) BaseRef() string { return c.checkout.BaseRef() }
func (c gitPublicationCheckout) Repo() string    { return c.checkout.Repo() }

// NewGitPublicationTransport constructs the production transport adapter.
func NewGitPublicationTransport(transport *publish.Transport) (*GitPublicationTransport, error) {
	if transport == nil {
		return nil, errors.New("publication transport: nil git transport")
	}
	return &GitPublicationTransport{transport: transport}, nil
}

func (t *GitPublicationTransport) FetchBase(
	ctx context.Context,
	repo, baseRef, baseSHA, dir string,
) (PublicationCheckout, error) {
	if t == nil || t.transport == nil {
		return nil, errors.New("publication transport: nil git transport")
	}
	checkout, err := t.transport.FetchBase(ctx, repo, baseRef, baseSHA, dir)
	if err != nil {
		return nil, err
	}
	return gitPublicationCheckout{checkout: checkout, owner: t}, nil
}

func (t *GitPublicationTransport) RetainWorktree(
	ctx context.Context, checkout PublicationCheckout, dest, headSHA string,
) error {
	if t == nil || t.transport == nil {
		return errors.New("publication transport: nil git transport")
	}
	sealed, ok := checkout.(gitPublicationCheckout)
	if !ok || sealed.owner != t {
		return ErrForeignPublicationCheckout
	}
	return t.transport.RetainWorktree(ctx, sealed.checkout.Dir(), dest, headSHA)
}

// PushHead forwards both capabilities to the sealed transport: the
// adapter's own owner check keeps a foreign checkout out, and the
// publish.GatedHead it passes through is the Publisher's gate proof, which
// this adapter can neither mint nor weaken.
func (t *GitPublicationTransport) PushHead(
	ctx context.Context,
	checkout PublicationCheckout,
	gated publish.GatedHead,
) (publish.PushResult, error) {
	sealed, ok := checkout.(gitPublicationCheckout)
	if !ok || sealed.owner != t {
		return publish.PushResult{}, ErrForeignPublicationCheckout
	}
	return t.transport.PushHead(ctx, sealed.checkout, gated)
}

// FetchHead forwards to the sealed transport, which re-gates the checkout and
// proves the branch holds the requested commit before reporting it.
func (t *GitPublicationTransport) FetchHead(
	ctx context.Context, checkout PublicationCheckout, branch, headSHA string,
) (publish.HeadFetch, error) {
	sealed, ok := checkout.(gitPublicationCheckout)
	if !ok || sealed.owner != t {
		return publish.HeadFetch{}, ErrForeignPublicationCheckout
	}
	return t.transport.FetchHead(ctx, sealed.checkout, branch, headSHA)
}

// ProspectiveMerge forwards to the sealed transport. The merge it builds
// stays in the checkout: this adapter offers no way to push it.
func (t *GitPublicationTransport) ProspectiveMerge(
	ctx context.Context, checkout PublicationCheckout, headSHA string,
) (string, error) {
	sealed, ok := checkout.(gitPublicationCheckout)
	if !ok || sealed.owner != t {
		return "", ErrForeignPublicationCheckout
	}
	return t.transport.ProspectiveMerge(ctx, sealed.checkout, headSHA)
}

// ProspectiveMergeOnto forwards to the sealed transport. Like UpdateHead it
// is an optional seam the workflow asserts for: only a run whose base advanced
// before its first publication needs a merge into a base other than the
// checkout's own.
func (t *GitPublicationTransport) ProspectiveMergeOnto(
	ctx context.Context, checkout PublicationCheckout, baseSHA, headSHA string,
) (string, error) {
	sealed, ok := checkout.(gitPublicationCheckout)
	if !ok || sealed.owner != t {
		return "", ErrForeignPublicationCheckout
	}
	return t.transport.ProspectiveMergeOnto(ctx, sealed.checkout, baseSHA, headSHA)
}

// RebaseProofTransport is the optional seam a head_changed re-entry asserts
// for (issue #2129): it fetches a head someone else pushed and the reviewed
// head it may be a rebase of, then decides whether it is. A transport
// without it gets no proof, so the cycle reviews the head as it did before.
type RebaseProofTransport interface {
	FetchPullHead(ctx context.Context, checkout PublicationCheckout, prNumber int, headSHA, reviewedHeadSHA string) error
	ProveRebase(ctx context.Context, checkout PublicationCheckout, reviewedBase, reviewedHead, newHead string) (publish.RebaseProof, error)
}

var _ RebaseProofTransport = (*GitPublicationTransport)(nil)

// FetchPullHead forwards to the sealed transport, which re-gates the checkout
// and proves the pull request's ref holds headSHA.
func (t *GitPublicationTransport) FetchPullHead(
	ctx context.Context, checkout PublicationCheckout, prNumber int, headSHA, reviewedHeadSHA string,
) error {
	sealed, ok := checkout.(gitPublicationCheckout)
	if !ok || sealed.owner != t {
		return ErrForeignPublicationCheckout
	}
	return t.transport.FetchPullHead(ctx, sealed.checkout, prNumber, headSHA, reviewedHeadSHA)
}

// ProveRebase forwards to the sealed transport. Like ProspectiveMerge it is
// local and leaves nothing to push.
func (t *GitPublicationTransport) ProveRebase(
	ctx context.Context, checkout PublicationCheckout, reviewedBase, reviewedHead, newHead string,
) (publish.RebaseProof, error) {
	sealed, ok := checkout.(gitPublicationCheckout)
	if !ok || sealed.owner != t {
		return publish.RebaseProof{}, ErrForeignPublicationCheckout
	}
	return t.transport.ProveRebase(ctx, sealed.checkout, reviewedBase, reviewedHead, newHead)
}

// UpdateHead is a separate capability-bearing successor effect. Ordinary
// publication transports remain create-only unless they implement this seam.
func (t *GitPublicationTransport) UpdateHead(ctx context.Context, checkout PublicationCheckout, gated publish.GatedUpdate) (publish.PushResult, error) {
	sealed, ok := checkout.(gitPublicationCheckout)
	if !ok || sealed.owner != t {
		return publish.PushResult{}, ErrForeignPublicationCheckout
	}
	return t.transport.UpdateHead(ctx, sealed.checkout, gated)
}

type attentionService interface {
	PutItem(context.Context, domain.AttentionItem) error
	// OpenEffectProposalItem opens (or supersedes and reopens) the human-gate
	// effect_proposal card for a source-issue-closure instance and the merge its
	// pull request has now. OpenEffectProposalNotice opens the non-holding
	// default-policy fallback notice for a daemon_fallback closure instead
	// (#1419 Part D). *signet.Service implements both.
	OpenEffectProposalItem(context.Context, domain.ProposalInstanceID, domain.ProspectiveMerge) (domain.AttentionItem, error)
	OpenEffectProposalNotice(context.Context, domain.ProposalInstanceID, domain.ProspectiveMerge) (domain.AttentionItem, error)
}

// ArtifactStore is the durable content-addressed boundary for the trusted
// recipe and verifier-authored report and transcript bytes. Metadata and tasks
// never commit before the bytes they require.
type ArtifactStore interface {
	Put(domain.Digest, io.Reader) (bool, error)
	Open(domain.Digest) (io.ReadCloser, error)
}

func validatePublicationCheckoutBinding(
	checkout PublicationCheckout,
	repo, baseRef, baseSHA, requestedDir string,
	requestedParent fs.FileInfo,
) (string, error) {
	value := reflect.ValueOf(checkout)
	if checkout == nil ||
		((value.Kind() == reflect.Chan || value.Kind() == reflect.Func ||
			value.Kind() == reflect.Interface || value.Kind() == reflect.Map ||
			value.Kind() == reflect.Pointer || value.Kind() == reflect.Slice) &&
			value.IsNil()) {
		return "", fmt.Errorf("transport returned a nil checkout: %w", domain.ErrParentKeyMismatch)
	}
	if checkout.Repo() != repo || checkout.BaseRef() != baseRef ||
		checkout.BaseSHA() != baseSHA {
		return "", fmt.Errorf("transport checkout disagrees with task: %w", domain.ErrParentKeyMismatch)
	}
	parent, err := os.Stat(filepath.Dir(requestedDir))
	if err != nil {
		return "", fmt.Errorf("stat requested checkout parent: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if requestedParent == nil || !os.SameFile(requestedParent, parent) {
		return "", fmt.Errorf(
			"transport replaced requested checkout parent: %w", domain.ErrParentKeyMismatch,
		)
	}
	requestedInfo, err := os.Lstat(requestedDir)
	if err != nil {
		return "", fmt.Errorf("stat requested checkout directory: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if requestedInfo.Mode()&os.ModeSymlink != 0 || !requestedInfo.IsDir() {
		return "", fmt.Errorf(
			"transport checkout path is not a real directory: %w", domain.ErrParentKeyMismatch,
		)
	}
	requested, err := filepath.EvalSymlinks(requestedDir)
	if err != nil {
		return "", fmt.Errorf("resolve requested checkout directory: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	returned, err := filepath.EvalSymlinks(checkout.Dir())
	if err != nil {
		return "", fmt.Errorf("resolve returned checkout directory: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if filepath.Clean(returned) != filepath.Clean(requested) {
		return "", fmt.Errorf(
			"transport checkout directory %q, want %q: %w",
			returned, requested, domain.ErrParentKeyMismatch,
		)
	}
	return requested, nil
}

// publicationBaseFetchError marks an error that came from FetchBase itself,
// so a caller can classify it as a transport failure. It keeps the inner
// error's text and errors.Is behavior.
type publicationBaseFetchError struct{ err error }

func (e *publicationBaseFetchError) Error() string { return e.err.Error() }
func (e *publicationBaseFetchError) Unwrap() error { return e.err }

// fetchAdmittedBase fetches base into scratch/checkout and refuses a checkout
// that validatePublicationCheckoutBinding does not bind to base, returning the
// checkout and its validated directory. Only a FetchBase failure comes back
// as a *publicationBaseFetchError; a binding refusal, which wraps
// domain.ErrParentKeyMismatch, is never marked, so no caller can treat a
// substituted checkout as a transport failure to hold or retry. The marker is
// a type rather than the absence of ErrParentKeyMismatch because a transport
// error may wrap that sentinel too.
//
// The caller owns scratch: later steps write beside the checkout, so the
// helper neither creates nor removes it.
func (w *productionPublicationWorkflow) fetchAdmittedBase(
	ctx context.Context, scratch string, base domain.BaseRevision,
) (PublicationCheckout, string, error) {
	parentInfo, err := os.Stat(scratch)
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Join(scratch, "checkout")
	checkout, err := w.transport.FetchBase(ctx, base.Repo, base.BaseRef, base.BaseSHA, dir)
	if err != nil {
		return nil, "", &publicationBaseFetchError{err: err}
	}
	checkoutDir, err := validatePublicationCheckoutBinding(
		checkout, base.Repo, base.BaseRef, base.BaseSHA, dir, parentInfo,
	)
	if err != nil {
		return nil, "", err
	}
	return checkout, checkoutDir, nil
}

func acquirePublicationLock(
	ctx context.Context,
	lockDir, key string,
) (func() error, error) {
	digest := sha256.Sum256([]byte(key))
	lockPath := filepath.Join(lockDir, hex.EncodeToString(digest[:])+".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // fixed digest name under daemon-owned work directory
	if err != nil {
		return nil, fmt.Errorf("open publication lock: %w", err)
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() error {
				return errors.Join(unix.Flock(int(lock.Fd()), unix.LOCK_UN), lock.Close())
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = lock.Close()
			return nil, fmt.Errorf("lock publication: %w", err)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = lock.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func publicationIdentity(candidate publish.Candidate) (publish.Identity, error) {
	digests := make([]domain.Digest, len(candidate.Artifacts))
	for i, artifact := range candidate.Artifacts {
		digests[i] = artifact.Digest
	}
	return publish.DeriveIdentity(publish.IdentityInput{
		Repo: candidate.Repo, BaseRef: candidate.BaseRef,
		SourceHeadSHA: candidate.HeadSHA, ArtifactDigests: digests,
		RecipeDigest: candidate.RecipeDigest,
	})
}

func compatibleTerminalItem(expected, current domain.AttentionItem) bool {
	// The persisted subject's task was authenticated against its run. It is
	// derived identity absent from older constructors, not a new replay input.
	if expected.Subject.TaskID == nil {
		expected.Subject.TaskID = current.Subject.TaskID
	}
	// Creation time is stamped when the item first lands. Recovery rebuilds
	// the terminal shape, so compare it using the durable original stamp.
	expected.CreatedAt = current.CreatedAt
	normalized := current
	legacyReadiness := current.Readiness == nil && expected.Readiness != nil
	if legacyReadiness {
		// Ready items created before the readiness summary existed cannot be
		// upgraded in place: the field is creation-immutable. Compare that one
		// legacy absence against the freshly reconstructed daemon value, then
		// keep nil as the transition baseline below.
		normalized.Readiness = expected.Readiness
	}
	legacyReadinessDetail := current.ReadinessDetail == nil && expected.ReadinessDetail != nil
	if legacyReadinessDetail {
		normalized.ReadinessDetail = expected.ReadinessDetail
	}
	// A detail stored before #1735 names no handoff tip, and the detail is
	// creation-immutable, so compare it as if it named the rebuilt tip and
	// merge, then keep the stored shape as the transition baseline below.
	legacyHandoff := !legacyReadinessDetail &&
		current.ReadinessDetail != nil && expected.ReadinessDetail != nil &&
		current.ReadinessDetail.HandoffTip == "" && expected.ReadinessDetail.HandoffTip != ""
	if legacyHandoff {
		detail := *current.ReadinessDetail
		detail.HandoffTip = expected.ReadinessDetail.HandoffTip
		detail.HandoffMerge = expected.ReadinessDetail.HandoffMerge
		normalized.ReadinessDetail = &detail
	}
	legacyYieldHistory := current.YieldHistory == nil && expected.YieldHistory != nil
	if legacyYieldHistory {
		normalized.YieldHistory = expected.YieldHistory
	}
	legacyDisplayNames := current.DisplayNames == nil && expected.DisplayNames != nil
	// Display names are mutable presentation state. Recovery derives the
	// current project and work-unit names, while the terminal item may retain
	// an older label from before a rename.
	normalized.DisplayNames = expected.DisplayNames
	legacyPublishBlock := current.PublishBlock == nil && expected.PublishBlock != nil
	if legacyPublishBlock {
		normalized.PublishBlock = expected.PublishBlock
	}
	normalized.ItemVersion = expected.ItemVersion
	normalized.Status = expected.Status
	normalized.DecidedAt = expected.DecidedAt
	normalized.Conclusion = expected.Conclusion
	normalized.Timing = expected.Timing
	// Readiness invalidation is recorded after creation, in the same
	// transition that supersedes the item (§7, issue #496): the
	// active-resource reconciler can set it during a crash seam before
	// finishTask runs. Recovery must accept the resulting terminal item as
	// compatible with the freshly derived ready shape rather than failing with
	// "disagrees" and stranding the publication task; the transition check below
	// still enforces that open -> superseded is a legal move.
	normalized.ReadinessInvalidation = expected.ReadinessInvalidation
	// Elapsed deadlines are appended after creation by the scheduler's
	// deadline handler (#2752), which can fire while a publication task sits
	// between arming its watches and recording its terminal result, or across
	// a restart. Recovery rebuilds the item without them, so they must not
	// make the stored ready item read as foreign. The rebuilt item carries no
	// list, so the transition check below admits any stored one; the list's
	// own rules hold because the stored row passed Validate on decode.
	normalized.ElapsedDeadlines = expected.ElapsedDeadlines
	// These two fields are daemon projections added at the item persistence
	// boundary. Recovery reconstructs the source shape, so neither may make an
	// otherwise identical terminal item look foreign or stale.
	normalized.Recommendation = expected.Recommendation
	normalized.DecisionSurface = expected.DecisionSurface
	if !reflect.DeepEqual(normalized, expected) {
		return false
	}
	if legacyReadiness {
		expected.Readiness = nil
	}
	if legacyReadinessDetail {
		expected.ReadinessDetail = nil
	}
	if legacyHandoff {
		detail := *expected.ReadinessDetail
		detail.HandoffTip = ""
		detail.HandoffMerge = nil
		expected.ReadinessDetail = &detail
	}
	if legacyYieldHistory {
		expected.YieldHistory = nil
	}
	if legacyDisplayNames {
		expected.DisplayNames = nil
	}
	if legacyPublishBlock {
		expected.PublishBlock = nil
	}
	// Persistence projections do not constitute an item transition. Normalize
	// them before the exact-replay check and before lifecycle validation so an
	// otherwise identical item at the same version still converges. Timing is
	// one of them: a delivery changes it and leaves item_version alone (#1946),
	// so a delivered item is not a later version of the rebuilt one.
	current.Recommendation = expected.Recommendation
	current.DecisionSurface = expected.DecisionSurface
	current.Timing = expected.Timing
	current.DisplayNames = expected.DisplayNames
	current.PublishBlock = expected.PublishBlock
	if reflect.DeepEqual(current, expected) {
		return true
	}
	// A row closed before conclusions existed (#2097) carries none. Recovery
	// compares it rather than writing it, so the missing record, which the
	// validator checks only after every other rule, does not make it foreign.
	err := domain.ValidateAttentionItemTransition(expected, current)
	return err == nil || (current.Conclusion == nil && errors.Is(err, domain.ErrConclusionMissing))
}

// readyItemID and blockedItemID keep production's ready and blocked items in
// the fake-publication-ready- and fake-publication-blocked- namespaces the
// retired fake lane introduced: every stored production item already carries
// those IDs, so changing the derivation would orphan them.
func readyItemID(runID domain.RunID) domain.ItemID {
	return fakepublication.ReadyItemID(runID)
}

func blockedItemID(runID domain.RunID) domain.ItemID {
	return fakepublication.BlockedItemID(runID)
}

// candidateFindings lifts a candidate's import and verification findings into
// the authorization's findings. stance is the bound trust profile's
// automation-change stance, which decides whether an automation-control
// import finding lifts as advisory; it is never read from a stored finding.
func candidateFindings(
	importFindings []importer.Finding,
	verifyFindings []verify.Finding,
	stance domain.AutomationChangePolicy,
) []domain.CandidateFinding {
	out := make([]domain.CandidateFinding, 0, len(importFindings)+len(verifyFindings))
	for _, finding := range importFindings {
		out = append(out, finding.Candidate(stance))
	}
	return append(out, verificationCandidateFindings(verifyFindings)...)
}

// verificationCandidateFindings lifts verification findings, which are always
// blocking verification-recipe findings under every profile stance.
func verificationCandidateFindings(verifyFindings []verify.Finding) []domain.CandidateFinding {
	out := make([]domain.CandidateFinding, 0, len(verifyFindings))
	for _, finding := range verifyFindings {
		category := domain.ControlPlaneVerificationRecipes
		out = append(out, domain.CandidateFinding{
			Class: domain.FindingClassControlPlane, Category: &category,
			Origin: domain.FindingOriginVerification, Kind: string(finding.Kind),
			Path: finding.Path, PathHex: finding.PathHex, Detail: finding.Detail,
			Disposition: domain.DispositionBlocking,
		})
	}
	return out
}

func digestJSON(value any) (domain.Digest, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digestBytes(payload), nil
}

func digestBytes(payload []byte) domain.Digest {
	sum := sha256.Sum256(payload)
	return domain.Digest(contentaddr.Format(sum[:]))
}

func validCommitSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, r := range sha {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func verifyArtifactBlob(store ArtifactStore, artifact domain.Artifact) error {
	body, err := store.Open(artifact.Digest)
	if err != nil {
		return fmt.Errorf("open candidate checkpoint artifact %s: %w", artifact.ID, err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, body)
	closeErr := body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("read candidate checkpoint artifact %s: %w", artifact.ID, err)
	}
	got := domain.Digest(contentaddr.Format(hasher.Sum(nil)))
	if got != artifact.Digest {
		return fmt.Errorf(
			"candidate checkpoint artifact %s body hashes to %s, want %s",
			artifact.ID, got, artifact.Digest,
		)
	}
	return nil
}

func loadArtifactBlob(
	store ArtifactStore,
	digest domain.Digest,
) ([]byte, error) {
	body, err := store.Open(digest)
	if err != nil {
		return nil, fmt.Errorf("open body: %w", err)
	}
	hasher := sha256.New()
	var content bytes.Buffer
	_, copyErr := io.Copy(io.MultiWriter(&content, hasher), body)
	closeErr := body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	got := domain.Digest(contentaddr.Format(hasher.Sum(nil)))
	if got != digest {
		return nil, fmt.Errorf(
			"body hashes to %s, want %s: %w", got, digest, domain.ErrParentKeyMismatch)
	}
	return content.Bytes(), nil
}

func isDefinitiveTrustRefusal(err error) bool {
	return errors.Is(err, publish.ErrTrustProfileDrift) ||
		errors.Is(err, publish.ErrHandoffCheckUnsatisfied)
}

func boolCount(v bool) int {
	if v {
		return 1
	}
	return 0
}
