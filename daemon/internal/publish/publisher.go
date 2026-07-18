package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Candidate is one publication's input: the verified candidate
// revision, the evidence artifacts backing it, and the invocation
// publishing it.
type Candidate struct {
	// Repo is the target repository ("owner/name").
	Repo string
	// BaseRef is the base branch the publication PR targets.
	BaseRef string
	// HeadSHA is the candidate commit; it must already exist in the
	// repository (the publisher creates refs, it does not upload
	// objects).
	HeadSHA string
	// Title and Body are the PR's human-facing content. The identity
	// marker is appended to Body deterministically; neither enters the
	// publication identity, so wording fixes converge onto the same
	// branch and PR instead of minting new ones.
	Title string
	Body  string
	// Artifacts are the evidence artifacts being published. Each is
	// re-gated against the approved-recipe set before any external
	// effect.
	Artifacts []domain.Artifact
	// RecipeDigest is the trusted verification recipe the candidate
	// was verified under; part of the publication identity.
	RecipeDigest *domain.Digest
	// InvocationID is the publishing invocation: the attempt axis the
	// outbox intent is keyed by.
	InvocationID domain.InvocationID
	// AuthorizationID and TrustProfileDigest bind the candidate to its
	// daemon-authored authorization and the automation trust profile it
	// was authorized under (#172). Carried, not yet enforced: the
	// fail-closed gate that requires an authorizing record (#168) and
	// the profile drift re-audit (#169) consume them; until those land,
	// nil means an as-yet-unbound caller.
	AuthorizationID    *domain.Digest
	TrustProfileDigest *domain.Digest
}

// Result reports the converged publication: the one branch and PR the
// identity names, and whether this call created them or found them.
type Result struct {
	Identity      Identity
	Branch        string
	PRNumber      int
	BranchCreated bool
	PRCreated     bool
}

// Publisher drives effectively-once candidate publication (plan §5.9,
// §5.15 rule 4): every external effect is check-before-create under a
// deterministic identity, and the intent is recorded through the
// outbox ledger before anything is dispatched.
type Publisher struct {
	forge  *forge
	ledger IntentLedger
}

// NewPublisher wires a Publisher. baseURL is the GitHub API root
// (real: https://api.github.com; tests: an httptest server).
func NewPublisher(ts TokenSource, client *http.Client, baseURL string, ledger IntentLedger) *Publisher {
	return &Publisher{forge: newForge(ts, client, baseURL), ledger: ledger}
}

// Publish converges the candidate onto its one intended result: the
// deterministic branch at the candidate head and the marker-bound PR.
// The order is fixed: gate the artifacts, derive the identity, record
// the intent, and only then touch GitHub — an interrupted publication
// retried at any point finds what the previous attempt created and
// continues instead of duplicating (issue #81 acceptance 2, 4).
func (p *Publisher) Publish(ctx context.Context, c Candidate, approvedRecipes map[domain.Digest]bool) (Result, error) {
	repo, err := parseRepo(c.Repo)
	if err != nil {
		return Result{}, fmt.Errorf("publish: %w", err)
	}
	if c.Title == "" {
		return Result{}, errors.New("publish: empty title")
	}

	// Trust gate before any external effect (§5.15 rule 2): every
	// artifact is re-gated against the current approved-recipe set —
	// the decoded PublishEligible bit is never trusted — every
	// head-bound artifact must describe exactly the candidate head,
	// and every artifact's recipe must be the candidate's recipe, so
	// the identity records the provenance the evidence was actually
	// produced under.
	digests := make([]domain.Digest, len(c.Artifacts))
	for i, a := range c.Artifacts {
		if err := domain.EligibleForEvidenceSnapshot(a, approvedRecipes); err != nil {
			return Result{}, fmt.Errorf("publish: %w", err)
		}
		if a.Provenance.HeadBinding == domain.HeadBound && a.Provenance.SourceHeadSHA != c.HeadSHA {
			return Result{}, fmt.Errorf("publish: artifact %s bound to a different head: %w", a.ID, ErrHeadMismatch)
		}
		// The gate above guarantees a recipe digest is present.
		if c.RecipeDigest == nil || *a.Provenance.VerificationRecipeDigest != *c.RecipeDigest {
			return Result{}, fmt.Errorf("publish: artifact %s verified under a recipe other than the candidate's: %w", a.ID, ErrPublicationConflict)
		}
		digests[i] = a.Digest
	}

	identity, err := DeriveIdentity(IdentityInput{
		Repo:            c.Repo,
		BaseRef:         c.BaseRef,
		SourceHeadSHA:   c.HeadSHA,
		ArtifactDigests: digests,
		RecipeDigest:    c.RecipeDigest,
	})
	if err != nil {
		return Result{}, fmt.Errorf("publish: %w", err)
	}

	// The composed PR content must parse back to exactly this identity,
	// or the publisher's own PR would later be classified as foreign and
	// convergence would deadlock: prose carrying a marker-shaped line
	// (quoted from another PR, say) fails here, before any effect.
	title, body := desiredPRContent(identity, c)
	if parsed, ok := ParseMarker(body); !ok || parsed != identity.Digest() {
		return Result{}, errors.New("publish: candidate body would not parse back to the publication identity marker")
	}

	if err := p.recordIntent(ctx, c, identity); err != nil {
		return Result{}, err
	}

	branch := identity.BranchName()
	result := Result{Identity: identity, Branch: branch}

	// Branch: check before create. An existing branch at the candidate
	// head is the converged state; at any other commit it is unknown
	// external state this publisher never overwrites.
	ref, err := p.forge.getRef(ctx, repo, branch, "")
	if err != nil {
		return Result{}, fmt.Errorf("publish: %w", err)
	}
	switch {
	case ref.Exists && ref.SHA == c.HeadSHA:
		// Converged already (a prior attempt created it).
	case ref.Exists:
		return Result{}, fmt.Errorf("publish: branch %s exists at a different commit: %w", branch, ErrPublicationConflict)
	default:
		if err := p.forge.createRef(ctx, repo, branch, c.HeadSHA); err != nil {
			return Result{}, fmt.Errorf("publish: %w", err)
		}
		result.BranchCreated = true
	}

	// PR: check before create, bound by the identity marker.
	pr, created, err := p.convergePR(ctx, repo, identity, c, title, body)
	if err != nil {
		return Result{}, err
	}
	result.PRNumber = pr
	result.PRCreated = created
	return result, nil
}

// recordIntent commits the publication intent through the outbox
// ledger before dispatch. A retry of the same invocation converges on
// the recorded row; a recorded intent naming a different identity
// means the invocation ID was reused for different content, which
// fails closed rather than publishing under a stale identity.
func (p *Publisher) recordIntent(ctx context.Context, c Candidate, identity Identity) error {
	intent := Intent{
		Identity:      identity.Digest(),
		InvocationID:  c.InvocationID,
		Repo:          c.Repo,
		BaseRef:       c.BaseRef,
		SourceHeadSHA: c.HeadSHA,
	}
	payload, err := intent.Encode()
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	key, err := IntentKey(c.InvocationID, IntentKindPublication)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	prior, recorded, err := p.ledger.Record(ctx, key, IntentKindPublication, payload)
	if err != nil {
		return fmt.Errorf("publish: record intent: %w", err)
	}
	if !recorded {
		committed, err := DecodeIntent(prior)
		if err != nil {
			return fmt.Errorf("publish: recorded intent for %s: %w", key, err)
		}
		if committed != intent {
			return fmt.Errorf("publish: invocation %s already committed a different intent: %w", c.InvocationID, ErrPublicationConflict)
		}
	}
	return nil
}

// convergePR finds or creates the identity's pull request. Exactly one
// open PR carrying the identity marker at the candidate head converges
// (its title and body are patched back if drifted); a marker-less PR
// on the branch is foreign; a closed marked PR, a marked PR at another
// head, or more than one marked PR is a conflict a human resolves.
func (p *Publisher) convergePR(ctx context.Context, repo repoRef, identity Identity, c Candidate, title, body string) (number int, created bool, err error) {
	prs, err := p.forge.listPRsByHead(ctx, repo, identity.BranchName())
	if err != nil {
		return 0, false, fmt.Errorf("publish: %w", err)
	}

	var ours []prState
	for _, pr := range prs {
		parsed, ok := ParseMarker(pr.Body)
		if !ok || parsed != identity.Digest() {
			return 0, false, fmt.Errorf("publish: pull request #%d occupies branch %s: %w", pr.Number, identity.BranchName(), ErrForeignResource)
		}
		ours = append(ours, pr)
	}
	switch {
	case len(ours) > 1:
		return 0, false, fmt.Errorf("publish: %d pull requests carry identity %s: %w", len(ours), identity.Digest(), ErrPublicationConflict)
	case len(ours) == 1:
		pr := ours[0]
		if pr.State != "open" {
			// A closed publication PR is a human decision; recreating or
			// reopening it would override that decision silently.
			return 0, false, fmt.Errorf("publish: pull request #%d for identity %s is closed: %w", pr.Number, identity.Digest(), ErrPublicationConflict)
		}
		// This is the decision point, so the state acted on carries its
		// own proof: a head that disagrees (the branch moved between
		// checks, or resolved into a fork) or a base a human retargeted
		// away from the candidate's would publish under coordinates the
		// identity does not name.
		if !prMatchesCandidate(pr, repo, identity, c) {
			return 0, false, fmt.Errorf("publish: pull request #%d head or base does not match the candidate: %w", pr.Number, ErrPublicationConflict)
		}
		if pr.Title != title || pr.Body != body {
			patched, err := p.forge.updatePR(ctx, repo, pr.Number, title, body)
			if err != nil {
				return 0, false, fmt.Errorf("publish: %w", err)
			}
			// The PATCH races the same external writers as everything
			// else: its returned object gets the same verification, so a
			// PR moved or retargeted between the list and the patch never
			// returns as a success.
			if !prMatchesCandidate(patched, repo, identity, c) {
				return 0, false, fmt.Errorf("publish: pull request #%d moved while converging: %w", pr.Number, ErrPublicationConflict)
			}
			// Stored content must be what was sent (the pre-check above
			// tolerates drift only because this patch repairs it): a
			// store that normalized or truncated the content would
			// otherwise report converged and silently re-patch on every
			// later publication.
			if patched.Title != title || patched.Body != body {
				return 0, false, fmt.Errorf("publish: pull request #%d content was not stored as sent: %w", pr.Number, ErrPublicationConflict)
			}
		}
		return pr.Number, false, nil
	}

	pr, err := p.forge.createPR(ctx, repo, identity.BranchName(), c.BaseRef, title, body)
	if err != nil {
		return 0, false, fmt.Errorf("publish: %w", err)
	}
	// Same returned-object check as the converge path: GitHub opens the
	// PR from the branch's tip at creation time, so a branch moved
	// after the ref check — or a head or base resolved anywhere other
	// than the coordinates the identity names — must not yield a
	// success whose PR the evidence was not produced for.
	if !prMatchesCandidate(pr, repo, identity, c) {
		return 0, false, fmt.Errorf("publish: created pull request #%d head or base does not match the candidate: %w", pr.Number, ErrPublicationConflict)
	}
	// Same stored-as-sent check as the patch path.
	if pr.Title != title || pr.Body != body {
		return 0, false, fmt.Errorf("publish: created pull request #%d content was not stored as sent: %w", pr.Number, ErrPublicationConflict)
	}
	return pr.Number, true, nil
}

// prMatchesCandidate is the complete success predicate over a returned
// pull-request object: it must be open (a closed publication PR is a
// human decision, never silently converged past), every coordinate the
// identity binds must match — head ref, head commit, head repository,
// base ref, base repository — and the body must parse back to exactly
// this identity's marker. Every decision point (the converge check,
// the created-PR response, the patched-PR response) runs this same
// predicate, so no field is checked on one path and dropped on
// another.
func prMatchesCandidate(pr prState, repo repoRef, identity Identity, c Candidate) bool {
	parsed, ok := ParseMarker(pr.Body)
	return pr.State == "open" &&
		pr.HeadRef == identity.BranchName() &&
		pr.HeadSHA == c.HeadSHA &&
		pr.HeadRepo == repo.path() &&
		pr.BaseRef == c.BaseRef &&
		pr.BaseRepo == repo.path() &&
		ok && parsed == identity.Digest()
}

// desiredPRContent is the deterministic PR content for a candidate:
// the prose body followed by the identity marker as the final line
// (plan §5.15 rule 4's deterministic PR-section marker).
func desiredPRContent(identity Identity, c Candidate) (title, body string) {
	prose := strings.TrimRight(c.Body, "\n")
	if prose == "" {
		return c.Title, identity.Marker()
	}
	return c.Title, prose + "\n\n" + identity.Marker()
}
