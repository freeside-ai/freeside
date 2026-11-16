package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// projectReadinessEvaluator evaluates one project's readiness to run
// unattended work (domain.ProjectReadiness) under the settings revisions the
// daemon was composed from, records it, and converges the project's card.
//
// It has two entry points. A full evaluation is split so that no lock or
// store transaction waits on GitHub: observe runs the three checks that read
// GitHub or the base tree (repository_base, publication_authority,
// writer_image) and holds nothing, and commit then runs the two store-local
// checks, writes the record, and converges the card. refresh re-runs only
// the store-local checks, before every unattended dispatch, and writes only
// when a result changed.
//
// The GitHub reads go through the daemon's own App authority: the ref read
// and base fetch use the publication token, so they send an installation
// token to GitHub. That is why this file is on the Tier A path list. The
// record it writes grants nothing; see projectReadinessGate.
type projectReadinessEvaluator struct {
	store                     *store.Store
	project                   domain.ProjectID
	settings                  *domain.SettingsRevisions
	repo                      string
	repositoryID              int64
	baseRef                   string
	agentImage                domain.ImageRef
	approvedRecipe            domain.Digest
	reviewConfigurationDigest domain.Digest
	github                    projectReadinessGitHub
	now                       func() time.Time

	// observeMu serializes observations, which the doctor pass and a
	// dispatch pass's retry can both start. It guards verifiedTip and
	// lastObserved and is never held with the doctor's convergence lock.
	observeMu sync.Mutex
	// verifiedTip is the last tip whose fetch carried the configured
	// repository id. A commit's repository cannot change, so a tip already
	// verified is not fetched again.
	verifiedTip string
	// lastObserved is when the last observation started.
	lastObserved time.Time
}

// projectReadinessRetryInterval is how often a dispatch pass re-observes a
// project whose record holds a GitHub-side check that did not pass, so a
// start that could not reach GitHub, or a repair made outside the daemon,
// is not held until the next doctor pass, which runs daily.
const projectReadinessRetryInterval = time.Minute

// projectReadinessGitHub is what the GitHub-side checks read.
type projectReadinessGitHub struct {
	// observeTip reads the base branch's tip through the publish reconciler.
	observeTip func(ctx context.Context, repo, branch string) (string, error)
	// fetchTip fetches the base branch at a tip and returns the repository
	// id the checkout carries.
	fetchTip func(ctx context.Context, tipSHA, dir string) (int64, error)
	// appBotIdentity resolves the App bot on the publication token. Minting
	// that token selects the one registered App whose installation grants
	// the repository and refuses none or several (publish's installation
	// resolver), so a resolution is the publication-authority check.
	appBotIdentity appBotIdentitySource
	// baseInputs reads what a base commit holds for an image input layout.
	baseInputs func(ctx context.Context, baseSHA string, layout domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error)
}

// projectReadinessObservation is what observe saw: the three GitHub-side
// results, in check order.
type projectReadinessObservation struct {
	results []domain.ProjectReadinessResult
}

// checkOutcome is one check's result before it is stamped: unavailable is
// a check GitHub could not answer, which keeps an earlier pass.
type checkOutcome struct {
	status domain.ProjectReadinessStatus
	detail string
}

func passed() checkOutcome { return checkOutcome{status: domain.ProjectReadinessPassed} }

func failed(format string, args ...any) checkOutcome {
	return checkOutcome{status: domain.ProjectReadinessFailed, detail: boundDetail(fmt.Sprintf(format, args...))}
}

func unavailable(format string, args ...any) checkOutcome {
	return checkOutcome{status: domain.ProjectReadinessUnavailable, detail: boundDetail(fmt.Sprintf(format, args...))}
}

// observe runs the GitHub-side checks. It holds no lock and opens no write
// transaction; it reads the prior record so a check GitHub could not answer
// keeps an earlier pass for the same key.
func (e *projectReadinessEvaluator) observe(ctx context.Context) (projectReadinessObservation, error) {
	e.observeMu.Lock()
	defer e.observeMu.Unlock()
	prior, found, err := e.read(ctx)
	if err != nil {
		return projectReadinessObservation{}, err
	}
	return e.observeLocked(ctx, prior, found)
}

// retryRemote observes again when the stored record is missing or holds a
// GitHub-side check that did not pass, at most once per
// projectReadinessRetryInterval, and reports whether it did. A failed check
// is retried as well as an unavailable one, so a repair outside the daemon
// (an App reinstalled, an image rebuilt) concludes the card within the
// interval instead of at the next daily doctor pass. It skips rather than
// waits while another observation runs. Like observe, it holds no lock the
// engine waits on.
func (e *projectReadinessEvaluator) retryRemote(ctx context.Context) (projectReadinessObservation, bool, error) {
	if !e.observeMu.TryLock() {
		return projectReadinessObservation{}, false, nil
	}
	defer e.observeMu.Unlock()
	if e.now().Sub(e.lastObserved) < projectReadinessRetryInterval {
		return projectReadinessObservation{}, false, nil
	}
	prior, found, err := e.read(ctx)
	if err != nil {
		return projectReadinessObservation{}, false, err
	}
	if found && !remoteNotPassed(prior) {
		return projectReadinessObservation{}, false, nil
	}
	observation, err := e.observeLocked(ctx, prior, found)
	return observation, err == nil, err
}

// remoteNotPassed reports whether a GitHub-side check in the record did
// not pass.
func remoteNotPassed(readiness domain.ProjectReadiness) bool {
	for _, result := range readiness.Results {
		if !result.Check.StoreLocal() && result.Status != domain.ProjectReadinessPassed {
			return true
		}
	}
	return false
}

func (e *projectReadinessEvaluator) observeLocked(
	ctx context.Context, prior domain.ProjectReadiness, found bool,
) (projectReadinessObservation, error) {
	now := e.now().UTC()
	e.lastObserved = now
	tip, base := e.repositoryBase(ctx)
	authority := e.publicationAuthority(ctx)
	image, err := e.writerImage(ctx, tip)
	if err != nil {
		return projectReadinessObservation{}, err
	}
	stamp := func(check domain.ProjectReadinessCheck, outcome checkOutcome) domain.ProjectReadinessResult {
		if outcome.status == domain.ProjectReadinessUnavailable && found {
			if earlier, ok := prior.Result(check); ok && earlier.Status == domain.ProjectReadinessPassed {
				return earlier
			}
		}
		return domain.ProjectReadinessResult{Check: check, Status: outcome.status, Detail: outcome.detail, CheckedAt: now}
	}
	return projectReadinessObservation{results: []domain.ProjectReadinessResult{
		stamp(domain.ProjectReadinessRepositoryBase, base),
		stamp(domain.ProjectReadinessPublicationAuthority, authority),
		stamp(domain.ProjectReadinessWriterImage, image),
	}}, nil
}

// commit runs the store-local checks, writes the record observation
// completes, and converges the card. The caller holds the doctor's
// convergence lock.
func (e *projectReadinessEvaluator) commit(
	ctx context.Context, observation projectReadinessObservation, doctor operations.Doctor,
) (domain.ProjectReadiness, error) {
	now := e.now().UTC()
	var local []domain.ProjectReadinessResult
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		local, err = projectReadinessLocalResults(ctx, tx, e.repo, e.repositoryID, e.reviewConfigurationDigest, now)
		return err
	}); err != nil {
		return domain.ProjectReadiness{}, fmt.Errorf("project %s readiness: %w", e.project, err)
	}
	readiness := e.record(local, observation.results, now)
	if err := e.write(ctx, readiness); err != nil {
		return domain.ProjectReadiness{}, err
	}
	return readiness, doctor.ConvergeProjectReadiness(ctx, readiness)
}

// refresh re-runs the store-local checks against the stored record and keeps
// its other results. With no record yet (none evaluated under these
// revisions), the other checks are unavailable, so the project is not
// ready. It writes and converges only when a result changed. The caller
// holds the doctor's convergence lock.
func (e *projectReadinessEvaluator) refresh(ctx context.Context, doctor operations.Doctor) error {
	now := e.now().UTC()
	var (
		prior domain.ProjectReadiness
		found bool
		local []domain.ProjectReadinessResult
	)
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		prior, found, err = e.readIn(ctx, tx)
		if err != nil {
			return err
		}
		local, err = projectReadinessLocalResults(ctx, tx, e.repo, e.repositoryID, e.reviewConfigurationDigest, now)
		return err
	}); err != nil {
		return fmt.Errorf("project %s readiness: %w", e.project, err)
	}
	remote := make([]domain.ProjectReadinessResult, 0, len(domain.AllProjectReadinessChecks)-len(local))
	for _, check := range domain.AllProjectReadinessChecks {
		if check.StoreLocal() {
			continue
		}
		if result, ok := prior.Result(check); found && ok {
			remote = append(remote, result)
			continue
		}
		remote = append(remote, domain.ProjectReadinessResult{
			Check: check, Status: domain.ProjectReadinessUnavailable,
			Detail: "not yet evaluated under these settings", CheckedAt: now,
		})
	}
	readiness := e.record(local, remote, now)
	if found && sameOutcomes(prior, readiness) {
		return nil
	}
	if err := e.write(ctx, readiness); err != nil {
		return err
	}
	return doctor.ConvergeProjectReadiness(ctx, readiness)
}

// record assembles local and remote results in check order.
func (e *projectReadinessEvaluator) record(
	local, remote []domain.ProjectReadinessResult, now time.Time,
) domain.ProjectReadiness {
	byCheck := make(map[domain.ProjectReadinessCheck]domain.ProjectReadinessResult, len(local)+len(remote))
	for _, result := range append(append([]domain.ProjectReadinessResult{}, local...), remote...) {
		byCheck[result.Check] = result
	}
	results := make([]domain.ProjectReadinessResult, 0, len(domain.AllProjectReadinessChecks))
	for _, check := range domain.AllProjectReadinessChecks {
		results = append(results, byCheck[check])
	}
	return domain.ProjectReadiness{
		ProjectID: e.project, Settings: e.settings, Results: results, EvaluatedAt: now,
	}
}

// sameOutcomes reports whether two records hold the same status and detail
// for every check; the times are not compared.
func sameOutcomes(a, b domain.ProjectReadiness) bool {
	if len(a.Results) != len(b.Results) {
		return false
	}
	for i := range a.Results {
		if a.Results[i].Check != b.Results[i].Check || a.Results[i].Status != b.Results[i].Status ||
			a.Results[i].Detail != b.Results[i].Detail {
			return false
		}
	}
	return true
}

func (e *projectReadinessEvaluator) read(ctx context.Context) (domain.ProjectReadiness, bool, error) {
	var (
		readiness domain.ProjectReadiness
		found     bool
	)
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		readiness, found, err = e.readIn(ctx, tx)
		return err
	})
	if err != nil {
		return domain.ProjectReadiness{}, false, fmt.Errorf("project %s readiness: %w", e.project, err)
	}
	return readiness, found, nil
}

func (e *projectReadinessEvaluator) readIn(ctx context.Context, tx *store.ReadTx) (domain.ProjectReadiness, bool, error) {
	readiness, err := tx.ProjectReadiness(ctx, e.project, e.settings)
	if errors.Is(err, store.ErrNotFound) {
		return domain.ProjectReadiness{}, false, nil
	}
	if err != nil {
		return domain.ProjectReadiness{}, false, err
	}
	return readiness, true, nil
}

func (e *projectReadinessEvaluator) write(ctx context.Context, readiness domain.ProjectReadiness) error {
	if err := e.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordProjectReadiness(ctx, readiness)
	}); err != nil {
		return fmt.Errorf("record project %s readiness: %w", e.project, err)
	}
	return nil
}

// projectReadinessLocalResults runs the two store-local checks in tx:
// trust_profile under the rule every unattended admission is gated by
// (store.ReadTx.TrustedProfile), and review_configuration, whether that
// profile approves the review configuration this daemon runs, as admission
// and Doctor.ConvergeReviewConfiguration compare them. A store error that
// is not the rule's refusal is returned, not recorded.
func projectReadinessLocalResults(
	ctx context.Context, tx *store.ReadTx, repo string, repositoryID int64, reviewDigest domain.Digest, now time.Time,
) ([]domain.ProjectReadinessResult, error) {
	stamp := func(check domain.ProjectReadinessCheck, outcome checkOutcome) domain.ProjectReadinessResult {
		return domain.ProjectReadinessResult{Check: check, Status: outcome.status, Detail: outcome.detail, CheckedAt: now}
	}
	profile, err := tx.TrustedProfile(ctx, repo, repositoryID)
	switch {
	case errors.Is(err, store.ErrRepositoryUntrusted), errors.Is(err, domain.ErrRepositoryIdentityMismatch):
		return []domain.ProjectReadinessResult{
			stamp(domain.ProjectReadinessTrustProfile, failed("%v", err)),
			stamp(domain.ProjectReadinessReviewConfiguration, failed("no trusted profile to approve it")),
		}, nil
	case err != nil:
		return nil, err
	}
	review := passed()
	if profile.Review.ConfigDigest != reviewDigest {
		review = failed("trust profile %s approves review configuration %s; the daemon runs %s",
			profile.ProfileDigest, profile.Review.ConfigDigest, reviewDigest)
	}
	return []domain.ProjectReadinessResult{
		stamp(domain.ProjectReadinessTrustProfile, passed()),
		stamp(domain.ProjectReadinessReviewConfiguration, review),
	}, nil
}

// repositoryBase checks that the base branch resolves on GitHub and that
// the repository at the configured name is the one the settings name, by
// the repository id a fetch of the tip carries, the rule the offline
// preflight applies. It returns the tip it read, empty when it read none.
func (e *projectReadinessEvaluator) repositoryBase(ctx context.Context) (string, checkOutcome) {
	tip, err := e.github.observeTip(ctx, e.repo, e.baseRef)
	if err != nil {
		return "", githubOutcome(err, "read the tip of %s@%s", e.repo, e.baseRef)
	}
	if tip == e.verifiedTip {
		return tip, passed()
	}
	scratch, err := os.MkdirTemp("", "freesided-project-readiness-")
	if err != nil {
		return tip, unavailable("create fetch scratch: %v", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort cleanup of an evaluator-owned checkout
	fetched, err := e.github.fetchTip(ctx, tip, filepath.Join(scratch, "checkout"))
	if err != nil {
		return tip, githubOutcome(err, "fetch %s@%s at %s", e.repo, e.baseRef, tip)
	}
	if fetched != e.repositoryID {
		return tip, failed("%s carries repository id %d, the settings name %d", e.repo, fetched, e.repositoryID)
	}
	e.verifiedTip = tip
	return tip, passed()
}

// publicationAuthority checks that exactly one registered App holds
// installation authority over the repository, by resolving its bot on the
// publication token (projectReadinessGitHub.appBotIdentity).
func (e *projectReadinessEvaluator) publicationAuthority(ctx context.Context) checkOutcome {
	if _, err := e.github.appBotIdentity.Resolve(ctx, e.repo); err != nil {
		return githubOutcome(err, "resolve the publication authority for %s", e.repo)
	}
	return passed()
}

// writerImage runs CheckWriterImage at tip, the tip repositoryBase read, or
// the stored tip when it read none. A base it cannot read, or no tip at all,
// is unavailable; any other refusal is failed. No image serving the base is
// a pass: attempts hold on that under image_upkeep_pending.
func (e *projectReadinessEvaluator) writerImage(ctx context.Context, tip string) (checkOutcome, error) {
	var in WriterImageCheck
	var stored domain.RepositoryTipObservation
	var storedFound bool
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if in.Seed, in.SeedFound, err = tx.GetProjectImageByRef(ctx, e.agentImage); err != nil {
			return err
		}
		if _, in.Pointer, in.PointerFound, err = tx.GetAdmittedImagePointer(
			ctx, e.repositoryID, domain.HarnessClientClaudeCode); err != nil {
			return err
		}
		if in.Images, err = tx.ListProjectImages(ctx, e.repositoryID); err != nil {
			return err
		}
		stored, storedFound, err = tx.GetRepositoryTipObservation(ctx, e.repositoryID)
		return err
	}); err != nil {
		return checkOutcome{}, fmt.Errorf("project %s readiness: read images: %w", e.project, err)
	}
	if tip == "" && storedFound && stored.Repository == e.repo && stored.Branch == e.baseRef {
		tip = stored.TipSHA
	}
	if tip == "" {
		return unavailable("no observed tip of %s@%s", e.repo, e.baseRef), nil
	}
	in.AgentImage, in.Repo, in.RepositoryID, in.BaseSHA, in.ApprovedRecipe =
		e.agentImage, e.repo, e.repositoryID, tip, e.approvedRecipe
	in.BaseInputs = func(layout domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error) {
		return e.github.baseInputs(ctx, tip, layout)
	}
	if _, _, err := CheckWriterImage(in); err != nil {
		if errors.Is(err, exec.ErrInputUnavailable) {
			return unavailable("read the base at %s: %v", tip, err), nil
		}
		return failed("%v", err), nil
	}
	return passed(), nil
}

// githubOutcome classifies a GitHub-side error: one that never reached an
// answer (githubUnreachable) is unavailable, any other is GitHub saying the
// authority or base is wrong, so failed.
func githubOutcome(err error, format string, args ...any) checkOutcome {
	what := fmt.Sprintf(format, args...)
	if githubUnreachable(err) {
		return unavailable("%s: %v", what, err)
	}
	return failed("%s: %v", what, err)
}

// githubUnreachable reports whether err means GitHub gave no answer: a
// transport or timeout failure, a server error or rate limit, the
// installation janitor not yet covering the registration, or a git fetch
// refused for any reason but authentication. Anything else, a 404 or a
// refused authority, is an answer.
func githubUnreachable(err error) bool {
	var apiErr *publish.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= http.StatusInternalServerError || apiErr.Status == http.StatusTooManyRequests
	}
	var gitErr *publish.TransportGitError
	if errors.As(err, &gitErr) {
		return gitErr.Refusal != publish.RefusalAuth
	}
	var urlErr *url.Error
	var netErr net.Error
	return errors.As(err, &urlErr) || errors.As(err, &netErr) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, publish.ErrJanitorInactive) || errors.Is(err, publish.ErrJanitorMintHeld) ||
		errors.Is(err, exec.ErrInputUnavailable)
}

// maxReadinessDetail keeps a detail inside the record's bound with room for
// the ellipsis.
const maxReadinessDetail = 1000

// boundDetail cuts a detail to the record's bound on a rune boundary.
func boundDetail(detail string) string {
	if len(detail) <= maxReadinessDetail {
		return detail
	}
	cut := maxReadinessDetail
	for cut > 0 && !utf8.RuneStart(detail[cut]) {
		cut--
	}
	return detail[:cut] + "..."
}
