package ward

import "context"

// review_provider.go carries the provider-neutral seam of the review runtime
// (#872). The Codex ReviewSource and the forthcoming Claude shadow ReviewSource
// (#865) share one audited container topology in this package; every
// vendor-varying decision is read through reviewProvider so the neutral core
// stays provider-agnostic. Codex is the first implementation, and every value
// it returns matches the pre-#872 hardcoded constant, so migrating the core
// onto this seam is byte-for-byte behavior-preserving.

// reviewProvider supplies the vendor-varying values of one review runtime: the
// finding-source and result-provider labels, the content-addressed envelope
// version tags, the review prompt protocol, and the in-container review command.
// The neutral core never hardcodes a vendor value; it reads each one here.
type reviewProvider interface {
	// sourceLabel is the domain.Finding.Source tag for findings this provider
	// produces (Codex: "codex_local").
	sourceLabel() string
	// providerLabel is the exec.ReviewResult.Provider tag (Codex: "openai").
	providerLabel() string
	// topologyVersion binds the read-only container-topology generation into the
	// durable binding and the configuration envelope (Codex:
	// "codex_review_read_only_v3").
	topologyVersion() string
	// completionEvidenceVersion prefixes the bounded raw-collection evidence
	// digest (Codex: "codex-review-completion-v1").
	completionEvidenceVersion() string
	// resultEvidenceVersion tags the normalized-result completion-evidence
	// envelope (Codex: "codex-review-result-v3").
	resultEvidenceVersion() string
	// configurationVersion tags the trust-profile configuration envelope (Codex:
	// "codex-review-configuration-v3").
	configurationVersion() string
	// promptProtocol identifies the review prompt contract carried in the
	// configuration envelope (Codex: "codex-production-review-prompt-v3").
	promptProtocol() string
	// reviewCommand builds the in-container review argv from the read-only
	// workspace target and the deployment-pinned model configuration.
	reviewCommand(workspaceTarget, model, reasoningEffort, prompt string) []string
	// newCredentialStrategy binds this provider's credential-delivery strategy
	// to the lifecycle that will invoke it during launch.
	newCredentialStrategy(lc *CodexReviewLifecycle) credentialStrategy
}

// credentialStrategy is the provider's credential-delivery seam, invoked by the
// neutral launch skeleton at two points before any container starts: a
// pre-intent launch-admission check, and acquisition of any host-side
// credential-mutation lease held across the atomic container start. Codex's
// subscription mode delivers a two-file OAuth auth.json snapshot backed by a
// host-side refresh lease; its api-key mode, and the forthcoming Claude
// setup-token strategy (#865), deliver a static credential with no refresh.
type credentialStrategy interface {
	// checkLaunchAdmission runs once before the launch intent opens.
	checkLaunchAdmission(ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec) error
	// acquireLease reserves any host-side credential-mutation lease that must be
	// held across container start. It always returns a non-nil lease; a mode
	// without host-side refresh returns a no-op lease.
	acquireLease(ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec) (reviewAuthLease, error)
}

// reviewAuthLease is one held host-side credential-mutation lease. The launch
// skeleton reserves the start-admission window through it (Codex bounds the
// window to the OAuth refresh lease; a no-refresh mode returns a plain child
// context) and releases it after the atomic start transfers the container.
type reviewAuthLease interface {
	// verifyStillAdmissible re-checks, mid-launch, that the held lease remains
	// valid and no re-enrollment intervened. It is a no-op for a no-refresh lease.
	verifyStillAdmissible(ctx context.Context) error
	// reserveStartAdmission returns the context the atomic container start runs
	// under, plus its cancel func.
	reserveStartAdmission(ctx context.Context) (context.Context, context.CancelFunc, error)
	// release ends the lease. It is idempotent and safe to call on a no-op lease.
	release(ctx context.Context) error
}

// codexReviewProvider is the first reviewProvider: the OpenAI Codex CLI review
// runtime. Each returned value is the exact constant the core used before the
// #872 extraction, so driving the neutral core with this provider preserves
// Codex behavior.
type codexReviewProvider struct{}

var _ reviewProvider = codexReviewProvider{}

func (codexReviewProvider) sourceLabel() string   { return "codex_local" }
func (codexReviewProvider) providerLabel() string { return "openai" }

func (codexReviewProvider) topologyVersion() string { return codexReviewTopologyVersion }

func (codexReviewProvider) completionEvidenceVersion() string { return "codex-review-completion-v1" }
func (codexReviewProvider) resultEvidenceVersion() string     { return "codex-review-result-v3" }
func (codexReviewProvider) configurationVersion() string      { return "codex-review-configuration-v3" }

func (codexReviewProvider) promptProtocol() string { return codexProductionReviewPromptVersion }

func (codexReviewProvider) reviewCommand(
	workspaceTarget, model, reasoningEffort, prompt string,
) []string {
	return codexReviewCommand(workspaceTarget, model, reasoningEffort, prompt)
}

func (codexReviewProvider) newCredentialStrategy(lc *CodexReviewLifecycle) credentialStrategy {
	return codexCredentialStrategy{lc: lc}
}

// codexCredentialStrategy delivers Codex credentials: subscription mode holds
// the OAuth host-refresh lease across start, api-key mode holds none. It is a
// thin adapter over the existing audited auth-lease functions, so the extraction
// changes no credential-isolation behavior.
type codexCredentialStrategy struct {
	lc *CodexReviewLifecycle
}

var _ credentialStrategy = codexCredentialStrategy{}

func (s codexCredentialStrategy) checkLaunchAdmission(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
) error {
	return checkCodexAuthReenrollment(ctx, cfg, launch)
}

func (s codexCredentialStrategy) acquireLease(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
) (reviewAuthLease, error) {
	guard, err := s.lc.acquireCodexReviewAuth(ctx, cfg, launch)
	if err != nil {
		return nil, err
	}
	// guard is nil for api-key mode; codexAuthLease's methods delegate to the
	// existing nil-tolerant functions, so a nil guard is a no-op lease.
	return codexAuthLease{lc: s.lc, cfg: cfg, launch: launch, guard: guard}, nil
}

// codexAuthLease wraps the optional Codex OAuth refresh guard. Both delegates
// tolerate a nil guard, reproducing the pre-extraction api-key behavior (a
// plain start context and a no-op release).
type codexAuthLease struct {
	lc     *CodexReviewLifecycle
	cfg    CodexReviewConfig
	launch CodexReviewLaunchSpec
	guard  *codexAuthLeaseGuard
}

var _ reviewAuthLease = codexAuthLease{}

func (l codexAuthLease) verifyStillAdmissible(ctx context.Context) error {
	return verifyCodexAuthLaunchAdmission(ctx, l.cfg, l.launch, l.guard)
}

func (l codexAuthLease) reserveStartAdmission(
	ctx context.Context,
) (context.Context, context.CancelFunc, error) {
	return reserveCodexAuthStartAdmission(ctx, l.cfg, l.launch, l.guard)
}

func (l codexAuthLease) release(ctx context.Context) error {
	return l.lc.releaseCodexReviewAuthLease(ctx, l.guard)
}
