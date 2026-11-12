package admissionauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Authority binds private driver state back to the durable admission and
// the trust profile its mode requires at import. The driver's state file is
// replay data, not an authorization source, so every call re-checks it
// against durable and current state. Every entry point first re-checks the
// admission itself, in this order:
//
//   - The recorded admission. Start and import read it under current
//     admission policy (store.ReadTx.GetExecutionAdmission); terminal replay
//     (AuthenticateAdmission, ImportOptionsRecord) authenticates the immutable
//     record without it.
//   - Current backend conformance, before a start or recovery only
//     (AuthenticateStart): an intent admitted under one backend configuration
//     cannot start under another.
//   - The resolved policy's digest against the admission and, under current
//     policy, its path allowlist against this daemon's configured
//     AllowedPaths (ResolvedPathAllowlist).
//   - The start spec against the admission.
//
// AuthenticateStart and AuthenticateImport then re-check the invocation's
// marker in the outbox: a specification invocation must carry an
// authenticated specification transition, and an invocation shaped like one
// without its marker fails closed. Any other invocation's commit-author
// claim must match the live GitHub App identity from CommitAuthors, resolved
// at start and cached per invocation in StartAuthors.
//
// ImportOptions and ImportOptionsRecord instead re-check, in order: the trust
// profile (the admission-bound revision, or for attended_dev the
// repository's currently active profile) and its repository against the
// admitted base; the run's specification digest against the admission and
// the specification bytes against that digest, for the fallback commit
// message; the specification marker, which selects the finding profile; and
// the commit author. ImportOptions revalidates the author against the live
// App identity; ImportOptionsRecord reads the durable claim without live
// GitHub authority, so terminal replay survives a restart.
type Authority struct {
	Store *store.Store
	// Blobs serves the approved specification the fallback commit message
	// derives from; import refuses without it.
	Blobs *signet.BlobStore
	// AllowedPaths is this daemon's configured path allowlist, which a run's
	// resolved paths policy must equal exactly.
	AllowedPaths []string
	// CommitAuthors resolves the live GitHub App identity; without it a
	// production commit-author claim is refused.
	CommitAuthors CommitAuthorResolver
	// StartAuthors caches start-time commit-author authentication per
	// invocation; nil authenticates on every start.
	StartAuthors *CommitAuthorCache
}

// CommitAuthorResolver resolves the GitHub App identity selected for a
// repository. Both methods authenticate the current App binding; the
// authority calls Resolve at start and Revalidate at the import boundary.
type CommitAuthorResolver interface {
	Resolve(context.Context, string) (publish.AppBotIdentity, error)
	Revalidate(context.Context, string) (publish.AppBotIdentity, error)
}

const maxProductionCommitAuthorAuthentications = 1024

type productionCommitAuthorAuthenticationBinding struct {
	repo      string
	appSlug   string
	botUserID int64
}

// CommitAuthorCache remembers which invocations authenticated their commit
// author at start, keyed by the repository and App identity they
// authenticated against; import forgets the entry. Its zero value is not
// usable; use NewCommitAuthorCache.
type CommitAuthorCache struct {
	mu      sync.Mutex
	entries map[domain.InvocationID]productionCommitAuthorAuthenticationBinding
}

// NewCommitAuthorCache returns an empty cache, bounded to
// maxProductionCommitAuthorAuthentications entries.
func NewCommitAuthorCache() *CommitAuthorCache {
	return &CommitAuthorCache{
		entries: map[domain.InvocationID]productionCommitAuthorAuthenticationBinding{},
	}
}

func (c *CommitAuthorCache) authenticate(
	id domain.InvocationID,
	binding productionCommitAuthorAuthenticationBinding,
	authenticate func() error,
) error {
	if c == nil {
		return authenticate()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.entries[id]; ok && cached == binding {
		return nil
	}
	if err := authenticate(); err != nil {
		return err
	}
	if _, exists := c.entries[id]; !exists && len(c.entries) >= maxProductionCommitAuthorAuthentications {
		clear(c.entries)
	}
	c.entries[id] = binding
	return nil
}

func (c *CommitAuthorCache) forget(id domain.InvocationID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, id)
}

type admissionReadKind string

const (
	admissionRecorded     admissionReadKind = "recorded"
	admissionForExecution admissionReadKind = "execution"
	admissionForImport    admissionReadKind = "import"
)

var AllAdmissionReadKinds = []admissionReadKind{admissionRecorded, admissionForExecution, admissionForImport}

func (k admissionReadKind) valid() bool {
	switch k {
	case admissionRecorded, admissionForExecution, admissionForImport:
		return true
	default:
		return false
	}
}

func (a Authority) admission(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec, kind admissionReadKind,
) (domain.ExecutionAdmission, []string, error) {
	if !kind.valid() {
		return domain.ExecutionAdmission{}, nil, fmt.Errorf("invalid admission read kind %q", kind)
	}
	requireCurrent := kind != admissionRecorded
	var admission domain.ExecutionAdmission
	var allowedPaths []string
	err := a.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if requireCurrent {
			admission, err = tx.GetExecutionAdmission(ctx, id)
		} else {
			admission, err = tx.GetExecutionAdmissionRecord(ctx, id)
		}
		if err != nil {
			return err
		}
		if kind == admissionForExecution {
			// Existing attempts bypass fresh admission on replay. Re-run the
			// recording-time conformance check here so an intent admitted under
			// backend configuration A cannot start or recover after this daemon
			// has restarted under a current proof for B. Authenticated released
			// output can only be imported, so its current-policy read omits this
			// launch gate while retaining the admission and path checks.
			// The authenticate gate tolerates a same-configuration recheck in
			// progress (a supersession marker for A) so this daemon's startup
			// re-proof cannot permanently reject an in-flight admission (#761); a
			// proof for a different configuration B still refuses.
			if err := tx.AuthenticateBackendConformant(ctx, admission); err != nil {
				return err
			}
		}
		policy, err := tx.GetResolvedPolicy(ctx, admission.RunID)
		if err != nil {
			return err
		}
		if requireCurrent {
			allowedPaths, err = ResolvedPathAllowlist(
				policy, admission.PolicyDigest, a.AllowedPaths,
			)
		} else {
			allowedPaths, err = recordedPathAllowlist(policy, admission.PolicyDigest)
		}
		return err
	})
	if err != nil {
		return domain.ExecutionAdmission{}, nil, err
	}
	if !reflect.DeepEqual(exec.StartSpecFromAdmission(admission), spec) {
		return domain.ExecutionAdmission{}, nil, fmt.Errorf(
			"start spec disagrees with admission %s: %w",
			admission.ID, domain.ErrParentKeyMismatch)
	}
	return admission, allowedPaths, nil
}

// ResolvedPathAllowlist binds the submitted, digest-addressed policy to the
// manually configured Phase 1A.2 containment boundary. Until per-run ward
// configuration exists, accepting a different path set would make the
// durable run policy an audit label rather than the policy import enforces.
func ResolvedPathAllowlist(
	policy domain.ResolvedPolicy, admittedDigest domain.Digest, configured []string,
) ([]string, error) {
	// A digest disagreement is record corruption and stays fatal. Every other
	// verdict here compares the durable policy against this daemon's current
	// -allowed-paths, so it is a configuration verdict: it must fail closed
	// without starting the writer, and it must not end the reconcile loop,
	// because an operator reconfiguring the boundary resolves it without
	// touching the recorded attempt (engine.MutableAdmissionPolicyRefusal).
	if policy.Digest != admittedDigest {
		return nil, fmt.Errorf(
			"resolved policy digest %q disagrees with admitted %q: %w",
			policy.Digest, admittedDigest, domain.ErrParentKeyMismatch,
		)
	}
	for _, key := range policy.Keys {
		if key.Key != "paths" {
			continue
		}
		paths := engine.SplitNonEmpty(key.Value)
		if !engine.ExplicitAllowedPaths(paths) {
			return nil, fmt.Errorf(
				"resolved paths policy is not an explicit allowlist: %w",
				domain.ErrPathBoundaryMismatch,
			)
		}
		if !slices.Equal(paths, configured) {
			return nil, fmt.Errorf(
				"resolved paths %q disagree with configured %q: %w",
				paths, configured, domain.ErrPathBoundaryMismatch,
			)
		}
		return slices.Clone(paths), nil
	}
	return nil, fmt.Errorf(
		"resolved policy has no paths key: %w", domain.ErrPathBoundaryMismatch,
	)
}

func recordedPathAllowlist(
	policy domain.ResolvedPolicy,
	admittedDigest domain.Digest,
) ([]string, error) {
	if policy.Digest != admittedDigest {
		return nil, fmt.Errorf(
			"resolved policy digest %q disagrees with admitted %q: %w",
			policy.Digest, admittedDigest, domain.ErrParentKeyMismatch,
		)
	}
	for _, key := range policy.Keys {
		if key.Key != "paths" {
			continue
		}
		paths := engine.SplitNonEmpty(key.Value)
		if !engine.ExplicitAllowedPaths(paths) {
			return nil, fmt.Errorf(
				"recorded resolved paths are not an explicit allowlist: %w",
				domain.ErrPathBoundaryMismatch,
			)
		}
		return slices.Clone(paths), nil
	}
	return nil, fmt.Errorf(
		"resolved policy has no paths key: %w", domain.ErrPathBoundaryMismatch,
	)
}

func (a Authority) AuthenticateAdmission(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	_, _, err := a.admission(ctx, id, spec, admissionRecorded)
	return err
}

func (a Authority) AuthenticateStart(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	admission, _, err := a.admission(ctx, id, spec, admissionForExecution)
	if err != nil {
		return err
	}
	return a.authenticateInvocationStart(ctx, id, admission, spec.Base.Repo)
}

func (a Authority) AuthenticateImport(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	admission, _, err := a.admission(ctx, id, spec, admissionForImport)
	if err != nil {
		return err
	}
	return a.authenticateInvocationStart(ctx, id, admission, spec.Base.Repo)
}

func (a Authority) authenticateInvocationStart(
	ctx context.Context, id domain.InvocationID, admission domain.ExecutionAdmission, repo string,
) error {
	specification, err := a.authenticateSpecificationInvocation(ctx, id, admission)
	if err != nil || specification {
		return err
	}
	_, _, err = a.authenticateInvocationCommitAuthorForStart(
		ctx, id, admission, repo,
	)
	return err
}

func (a Authority) authenticateSpecificationInvocation(
	ctx context.Context, id domain.InvocationID, admission domain.ExecutionAdmission,
) (bool, error) {
	specification := false
	err := a.Store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(ctx, string(id))
		if err != nil {
			return err
		}
		if entry.Kind != engine.KindSpecificationInvocationRequested &&
			entry.Kind != engine.KindSpecificationDiscussionRequested {
			return nil
		}
		specification = true
		if entry.Kind == engine.KindSpecificationDiscussionRequested {
			return engine.AuthenticateSpecificationDiscussionTransition(
				ctx, tx, entry, admission.RunID, admission.StageID,
			)
		}
		return engine.AuthenticateSpecificationInvocationTransition(
			ctx, tx, entry, admission.RunID, admission.StageID,
		)
	})
	if errors.Is(err, store.ErrNotFound) {
		if specification || engine.IsSpecificationInvocationIdentity(
			id, admission.RunID, admission.StageID,
		) {
			return true, fmt.Errorf("authenticate specification invocation marker: %w", err)
		}
		return false, nil
	}
	if err != nil {
		return specification, fmt.Errorf("authenticate specification invocation marker: %w", err)
	}
	return specification, nil
}

func (a Authority) ImportOptions(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec, opts importer.Options,
) (importer.Options, error) {
	admission, allowedPaths, err := a.admission(ctx, id, spec, admissionForImport)
	if err != nil {
		return importer.Options{}, err
	}
	profile, err := a.importTrustProfile(ctx, admission)
	if err != nil {
		return importer.Options{}, err
	}
	if profile.Repo != spec.Base.Repo || profile.RepositoryID != spec.Base.RepositoryID {
		return importer.Options{}, fmt.Errorf(
			"trust profile repository disagrees with admitted base: %w",
			domain.ErrParentKeyMismatch)
	}
	opts.Policy, err = opts.Policy.WithProtectedPaths(profile)
	if err != nil {
		return importer.Options{}, err
	}
	opts.Policy.Allowlist = allowedPaths
	opts.CommitMessage, err = a.fallbackCommitMessage(ctx, admission, opts.Policy)
	if err != nil {
		return importer.Options{}, err
	}
	// A specification run imports under the specification finding profile: it
	// publishes a typed JSON result, never workspace content, so incidental
	// investigation debris must not definitively fail the invocation (#768).
	if err := a.applySpecificationFindingProfile(ctx, id, admission, &opts); err != nil {
		return importer.Options{}, err
	}
	author, production, err := a.invocationImportAuthor(ctx, id, admission, spec.Base.Repo)
	if err != nil {
		return importer.Options{}, err
	}
	if production {
		opts.AuthorName, opts.AuthorEmail = author.Name(), author.Email()
	}
	a.StartAuthors.forget(id)
	return opts, nil
}

// ProtectedPatterns lists the patterns whose edit would fail the invocation
// at import under publish_strict, from the trust profile ImportOptions would
// apply. It informs the writer's prompt only: it authorizes nothing, and
// ImportOptions derives the import's own patterns again at import. An attended
// admission reads the repository's latest profile, so an owner re-approval
// during the run can leave the writer's list stale; the import gate decides.
func (a Authority) ProtectedPatterns(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) ([]string, error) {
	admission, _, err := a.admission(ctx, id, spec, admissionForExecution)
	if err != nil {
		return nil, err
	}
	return a.protectedPatterns(ctx, admission, spec)
}

// protectedPatterns derives the list for an authenticated admission. An
// attended admission binds no profile, and its repository may have none
// approved yet; import then waits for one, and start must not refuse a run
// that admission accepted. The built-in patterns apply under every profile,
// so they are the list until one is approved.
func (a Authority) protectedPatterns(
	ctx context.Context, admission domain.ExecutionAdmission, spec exec.StartSpec,
) ([]string, error) {
	profile, err := a.importTrustProfile(ctx, admission)
	if admission.TrustProfileDigest == nil && errors.Is(err, store.ErrNotFound) {
		return importer.Policy{}.PublishBlockingPatterns(), nil
	}
	if err != nil {
		return nil, err
	}
	if profile.Repo != spec.Base.Repo || profile.RepositoryID != spec.Base.RepositoryID {
		return nil, fmt.Errorf(
			"trust profile repository disagrees with admitted base: %w",
			domain.ErrParentKeyMismatch)
	}
	policy, err := importer.Policy{}.WithProtectedPaths(profile)
	if err != nil {
		return nil, err
	}
	return policy.PublishBlockingPatterns(), nil
}

// applySpecificationFindingProfile sets the specification finding profile when
// the invocation is an authenticated specification one, so both the live import
// and its terminal replay reconstruct the same profile from the same durable
// marker (the omitempty field keeps a non-specification policy byte-identical).
func (a Authority) applySpecificationFindingProfile(
	ctx context.Context, id domain.InvocationID, admission domain.ExecutionAdmission, opts *importer.Options,
) error {
	specification, err := a.authenticateSpecificationInvocation(ctx, id, admission)
	if err != nil {
		return err
	}
	if specification {
		profile := importer.FindingProfileSpecification
		opts.Policy.FindingProfile = &profile
	}
	return nil
}

func (a Authority) ImportOptionsRecord(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec, opts importer.Options,
) (importer.Options, error) {
	admission, allowedPaths, err := a.admission(ctx, id, spec, admissionRecorded)
	if err != nil {
		return importer.Options{}, err
	}
	profile, err := a.importTrustProfile(ctx, admission)
	if err != nil {
		return importer.Options{}, err
	}
	if profile.Repo != spec.Base.Repo || profile.RepositoryID != spec.Base.RepositoryID {
		return importer.Options{}, fmt.Errorf(
			"trust profile repository disagrees with admitted base: %w",
			domain.ErrParentKeyMismatch)
	}
	opts.Policy, err = opts.Policy.WithProtectedPaths(profile)
	if err != nil {
		return importer.Options{}, err
	}
	opts.Policy.Allowlist = allowedPaths
	opts.CommitMessage, err = a.fallbackCommitMessage(ctx, admission, opts.Policy)
	if err != nil {
		return importer.Options{}, err
	}
	// Reconstruct the same specification profile the live import used, from the
	// same durable specification marker, so the replayed ImportOptions match.
	if err := a.applySpecificationFindingProfile(ctx, id, admission, &opts); err != nil {
		return importer.Options{}, err
	}
	// This reconstructs an already-completed import from immutable records.
	// App attribution was authenticated before the actual import; requiring
	// live GitHub authority here would strand terminal replay after a restart.
	author, production, err := a.invocationImportRecordAuthor(ctx, id, admission)
	if err != nil {
		return importer.Options{}, err
	}
	if production {
		opts.AuthorName, opts.AuthorEmail = author.Name(), author.Email()
	}
	return opts, nil
}

func (a Authority) invocationImportAuthor(
	ctx context.Context, id domain.InvocationID, admission domain.ExecutionAdmission, repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	specification, err := a.authenticateSpecificationInvocation(ctx, id, admission)
	if err != nil || specification {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return a.authenticateInvocationCommitAuthorRevalidated(
		ctx, id, admission, repo,
	)
}

func (a Authority) invocationImportRecordAuthor(
	ctx context.Context, id domain.InvocationID, admission domain.ExecutionAdmission,
) (engine.ProductionCommitAuthor, bool, error) {
	specification, err := a.authenticateSpecificationInvocation(ctx, id, admission)
	if err != nil || specification {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return a.invocationCommitAuthor(ctx, id, admission)
}

func (a Authority) fallbackCommitMessage(
	ctx context.Context,
	admission domain.ExecutionAdmission,
	policy importer.Policy,
) (string, error) {
	if a.Blobs == nil {
		return "", errors.New("derive fallback commit message: nil blob store")
	}
	var (
		run        domain.Run
		boundIssue *int
	)
	if err := a.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, admission.RunID)
		if err != nil {
			return err
		}
		declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, admission.RunID)
		switch {
		case err == nil:
			boundIssue = declaration.BoundIssue
			return nil
		case errors.Is(err, store.ErrNotFound):
			return nil
		default:
			return err
		}
	}); err != nil {
		return "", fmt.Errorf("derive fallback commit message authority: %w", err)
	}
	if run.SpecDigest != admission.SpecDigest {
		return "", fmt.Errorf(
			"run specification digest %q disagrees with admission %q: %w",
			run.SpecDigest, admission.SpecDigest, domain.ErrParentKeyMismatch,
		)
	}
	spec, err := readVerifiedBlob(ctx, a.Blobs, run.SpecDigest)
	if err != nil {
		return "", fmt.Errorf("read approved specification: %w", err)
	}
	return engine.FallbackCommitMessage(engine.FallbackCommitMessageInput{
		Spec: spec, BoundIssue: boundIssue, RunID: run.ID,
		SpecDigest: run.SpecDigest, Policy: policy,
	}), nil
}

func readVerifiedBlob(
	ctx context.Context,
	blobs *signet.BlobStore,
	digest domain.Digest,
) ([]byte, error) {
	body, err := blobs.OpenContext(ctx, digest)
	if err != nil {
		return nil, err
	}
	hasher := sha256.New()
	var content bytes.Buffer
	_, copyErr := io.Copy(io.MultiWriter(&content, hasher), body)
	closeErr := body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return nil, err
	}
	got := domain.Digest(contentaddr.Format(hasher.Sum(nil)))
	if got != digest {
		return nil, fmt.Errorf("body hashes to %s, want %s", got, digest)
	}
	return content.Bytes(), nil
}

func (a Authority) productionCommitAuthor(
	ctx context.Context,
	id domain.InvocationID,
	mode domain.OperatingMode,
) (engine.ProductionCommitAuthor, bool, error) {
	var entry store.QueueEntry
	err := a.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(id))
		return err
	})
	if mode != domain.ModeUnattended && errors.Is(err, store.ErrNotFound) {
		return engine.ProductionCommitAuthor{}, false, nil
	}
	if err != nil {
		return engine.ProductionCommitAuthor{}, false, fmt.Errorf("load production commit author: %w", err)
	}
	if entry.Kind != engine.KindProductionInvocationRequested {
		if mode != domain.ModeUnattended {
			return engine.ProductionCommitAuthor{}, false, nil
		}
		return engine.ProductionCommitAuthor{}, false, fmt.Errorf(
			"unattended invocation %q has no production ownership marker: %w",
			id, domain.ErrParentKeyMismatch,
		)
	}
	publication, present, err := engine.ProductionInvocationPublication(entry)
	if err != nil {
		return engine.ProductionCommitAuthor{}, false, fmt.Errorf("authenticate production commit author: %w", err)
	}
	if !present {
		return engine.ProductionCommitAuthor{}, false, nil
	}
	return publication.CommitAuthor, true, nil
}

// invocationCommitAuthor authenticates the durable marker for either an
// initial production invocation or a remediation invocation. Remediation
// inherits only the original production request's commit-author claim after
// the complete review/adjudication transition is reconstructed in this same
// store snapshot.
func (a Authority) invocationCommitAuthor(
	ctx context.Context,
	id domain.InvocationID,
	admission domain.ExecutionAdmission,
) (engine.ProductionCommitAuthor, bool, error) {
	var (
		entry       store.QueueEntry
		publication engine.ProductionPublication
		remediation bool
		feedback    bool
	)
	err := a.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(id))
		if err != nil {
			return err
		}
		if entry.Kind == engine.KindOperatorFeedbackInvocationRequested {
			feedback = true
			publication, err = engine.AuthenticateOperatorFeedbackInvocationTransition(
				ctx, tx, entry, admission.RunID, admission.StageID,
			)
			return err
		}
		if entry.Kind != engine.KindRemediationInvocationRequested {
			return nil
		}
		remediation = true
		publication, err = engine.AuthenticateRemediationInvocationTransition(
			ctx, tx, entry, admission.RunID, admission.StageID,
		)
		return err
	})
	if feedback {
		if err != nil {
			return engine.ProductionCommitAuthor{}, false,
				fmt.Errorf("authenticate operator-feedback commit author: %w", err)
		}
		return publication.CommitAuthor, true, nil
	}
	if remediation {
		if err != nil {
			return engine.ProductionCommitAuthor{}, false,
				fmt.Errorf("authenticate remediation commit author: %w", err)
		}
		return publication.CommitAuthor, true, nil
	}
	if err != nil && (admission.OperatingMode == domain.ModeUnattended ||
		!errors.Is(err, store.ErrNotFound)) {
		return engine.ProductionCommitAuthor{}, false, fmt.Errorf(
			"load invocation commit author: %w", err)
	}
	return a.productionCommitAuthor(ctx, id, admission.OperatingMode)
}

func (a Authority) authenticateInvocationCommitAuthorForStart(
	ctx context.Context,
	id domain.InvocationID,
	admission domain.ExecutionAdmission,
	repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	claimed, production, err := a.invocationCommitAuthor(ctx, id, admission)
	if err != nil || !production {
		return claimed, production, err
	}
	binding := productionCommitAuthorAuthenticationBinding{
		repo: repo, appSlug: claimed.AppSlug, botUserID: claimed.BotUserID,
	}
	err = a.StartAuthors.authenticate(id, binding, func() error {
		return a.authenticateProductionCommitAuthorClaim(ctx, repo, claimed, false)
	})
	if err != nil {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return claimed, true, nil
}

func (a Authority) authenticateInvocationCommitAuthorRevalidated(
	ctx context.Context,
	id domain.InvocationID,
	admission domain.ExecutionAdmission,
	repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	claimed, production, err := a.invocationCommitAuthor(ctx, id, admission)
	if err != nil || !production {
		return claimed, production, err
	}
	if err := a.authenticateProductionCommitAuthorClaim(ctx, repo, claimed, true); err != nil {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return claimed, true, nil
}

func (a Authority) authenticateProductionCommitAuthor(
	ctx context.Context,
	id domain.InvocationID,
	mode domain.OperatingMode,
	repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	return a.authenticateProductionCommitAuthorWith(
		ctx, id, mode, repo, false,
	)
}

func (a Authority) authenticateProductionCommitAuthorForStart(
	ctx context.Context,
	id domain.InvocationID,
	mode domain.OperatingMode,
	repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	claimed, production, err := a.productionCommitAuthor(ctx, id, mode)
	if err != nil || !production {
		return claimed, production, err
	}
	binding := productionCommitAuthorAuthenticationBinding{
		repo: repo, appSlug: claimed.AppSlug, botUserID: claimed.BotUserID,
	}
	err = a.StartAuthors.authenticate(id, binding, func() error {
		return a.authenticateProductionCommitAuthorClaim(ctx, repo, claimed, false)
	})
	if err != nil {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return claimed, true, nil
}

func (a Authority) authenticateProductionCommitAuthorRevalidated(
	ctx context.Context,
	id domain.InvocationID,
	mode domain.OperatingMode,
	repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	return a.authenticateProductionCommitAuthorWith(
		ctx, id, mode, repo, true,
	)
}

func (a Authority) authenticateProductionCommitAuthorWith(
	ctx context.Context,
	id domain.InvocationID,
	mode domain.OperatingMode,
	repo string,
	revalidate bool,
) (engine.ProductionCommitAuthor, bool, error) {
	claimed, production, err := a.productionCommitAuthor(ctx, id, mode)
	if err != nil || !production {
		return claimed, production, err
	}
	if err := a.authenticateProductionCommitAuthorClaim(ctx, repo, claimed, revalidate); err != nil {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return claimed, true, nil
}

func (a Authority) authenticateProductionCommitAuthorClaim(
	ctx context.Context,
	repo string,
	claimed engine.ProductionCommitAuthor,
	revalidate bool,
) error {
	if a.CommitAuthors == nil {
		return fmt.Errorf(
			"authenticate production commit author: no App identity resolver: %w",
			publish.ErrAppBotIdentityMismatch,
		)
	}
	var resolved publish.AppBotIdentity
	var err error
	if revalidate {
		resolved, err = a.CommitAuthors.Revalidate(ctx, repo)
	} else {
		resolved, err = a.CommitAuthors.Resolve(ctx, repo)
	}
	if err != nil {
		return fmt.Errorf(
			"authenticate production commit author: %w", err,
		)
	}
	if claimed.AppSlug != resolved.AppSlug || claimed.BotUserID != resolved.BotUserID {
		return fmt.Errorf(
			"authenticate production commit author: durable attribution disagrees with selected App: %w",
			publish.ErrAppBotIdentityMismatch,
		)
	}
	return nil
}

func (a Authority) importTrustProfile(
	ctx context.Context,
	admission domain.ExecutionAdmission,
) (domain.AutomationTrustProfile, error) {
	var profile domain.AutomationTrustProfile
	err := a.Store.Read(ctx, func(tx *store.ReadTx) error {
		if admission.TrustProfileDigest != nil {
			var err error
			profile, err = tx.GetTrustProfile(ctx, *admission.TrustProfileDigest)
			return err
		}
		if admission.OperatingMode != domain.ModeAttendedDev {
			return fmt.Errorf(
				"admission %s under mode %q carries no trust profile: %w",
				admission.ID, admission.OperatingMode, domain.ErrEmptyField,
			)
		}
		var err error
		profile, err = tx.LatestTrustProfile(ctx, admission.Base.Repo)
		return err
	})
	return profile, err
}
