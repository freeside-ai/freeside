package ward

import (
	"context"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// reviewFindingsJSONSchema is the provider-neutral structured-output contract
// every review CLI is constrained to: a findings array over the native P0–P3
// severity scale, each carrying a file location and an explanation. A location
// is one of two variants (a JSON-Schema anyOf): the concrete new-side line
// range {path, start_line≥1, end_line≥1}, or the whole-file marker
// {path, whole_file:true} for a finding on a candidate-deleted file — which has
// no new-side line — or an otherwise wholly file-level finding (§7, #855). The
// two variants are mutually exclusive: additionalProperties:false lets each
// object carry only its own fields. anyOf (not oneOf) is used because it is the
// union form OpenAI structured outputs supports; the range variant is byte-for-
// byte the pre-#855 shape, so the common line-range case is unchanged. The
// schema steers the model; ward normalization is the hard fail-closed gate and
// admits the domain whole-file (0,0) location only under the explicit marker.
// Codex passes the schema via --output-schema (from a file); Claude passes it
// inline via --json-schema. Both providers share this single literal so the two
// runtimes can never drift on the severity scale or the location shape.
const reviewFindingsJSONSchema = `{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object","properties":{"severity":{"type":"string","enum":["P0","P1","P2","P3"]},"location":{"anyOf":[{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}},"required":["path","start_line","end_line"],"additionalProperties":false},{"type":"object","properties":{"path":{"type":"string"},"whole_file":{"type":"boolean","enum":[true],"description":"Set true only for a finding on a candidate-deleted file (no new-side line) or an otherwise wholly file-level finding; use the start_line/end_line range for every finding with concrete new-side lines."}},"required":["path","whole_file"],"additionalProperties":false}]},"explanation":{"type":"string"}},"required":["severity","location","explanation"],"additionalProperties":false}}},"required":["findings"],"additionalProperties":false}`

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
	// vendor is the agent vendor whose native instruction mechanism this
	// provider's review invocation consumes (Codex: domain.AgentVendorCodex).
	vendor() domain.AgentVendor
	// reviewPrompt builds the production review prompt for one request.
	reviewPrompt(req exec.ReviewRequest) string
	// terminalFailureMessage extracts the provider's final structured CLI error
	// message. Unstructured stderr and non-error envelopes are never authority
	// for failure classification.
	terminalFailureMessage(events []byte) string
	// reviewContainerSuffix is the provider-specific review-container name suffix
	// appended to the shared per-run prefix (Codex: "-codex").
	reviewContainerSuffix() string
	// legacyTopologyVersions lists prior topology versions this provider still
	// accepts for teardown authentication only, never for launch (Codex: the v2
	// snapshot shape; a provider with no history returns nil).
	legacyTopologyVersions() []string
	// homeTarget and configHomeTarget are the container HOME and the writable
	// per-invocation agent config root the launcher environment binds (Codex:
	// CodexContainerHomeTarget and CodexHomeTarget). They are recorded in the
	// durable binding and re-derived at every reconstruction.
	homeTarget() string
	configHomeTarget() string
	// containerEnv is the fixed, non-secret launcher environment (before the
	// proxy variables) the review container runs under. It never carries a
	// credential: the token reaches the CLI from the read-only snapshot file,
	// never through the environment.
	containerEnv() []string
	// snapshotCredentialName and snapshotInstructionName are the two basenames
	// the credential and instruction bytes carry on the read-only snapshot
	// volume (Codex: "auth.json" and "AGENTS.md").
	snapshotCredentialName() string
	snapshotInstructionName() string
	// requiresExpiringCredential reports whether this provider's credential
	// carries an access-token lifetime the config floor and runtime checks gate
	// (Codex: true; the setup-token strategy: false).
	requiresExpiringCredential() bool
	// acceptsAuthMode reports whether the given auth mode is one this provider
	// delivers.
	acceptsAuthMode(mode CodexAuthMode) bool
	// inspectAgentAuthSnapshot derives the container-facing credential bytes and
	// any access-token expiry from the host credential body. A no-expiry
	// credential returns a nil expiry.
	inspectAgentAuthSnapshot(mode CodexAuthMode, body []byte) ([]byte, *time.Time, error)
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

// allReviewProviders is the closed set of review providers this runtime knows.
// Teardown and recovery authenticate persisted resource names against each
// provider's current topology, and outcome validation resolves a persisted
// result's provider label against this set. Adding a provider registers it in
// one place.
func allReviewProviders() []reviewProvider {
	return []reviewProvider{codexReviewProvider{}, claudeReviewProvider{}}
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

func (codexReviewProvider) vendor() domain.AgentVendor { return domain.AgentVendorCodex }

func (codexReviewProvider) reviewPrompt(req exec.ReviewRequest) string {
	return codexProductionReviewPrompt(req)
}

func (codexReviewProvider) terminalFailureMessage(events []byte) string {
	return codexTerminalFailureMessage(events)
}

func (codexReviewProvider) reviewContainerSuffix() string { return "-codex" }

func (codexReviewProvider) legacyTopologyVersions() []string {
	return []string{codexReviewTopologyVersionV2}
}

func (codexReviewProvider) homeTarget() string       { return CodexContainerHomeTarget }
func (codexReviewProvider) configHomeTarget() string { return CodexHomeTarget }

func (codexReviewProvider) containerEnv() []string {
	return []string{"HOME=" + CodexContainerHomeTarget, "CODEX_HOME=" + CodexHomeTarget}
}

func (codexReviewProvider) snapshotCredentialName() string  { return codexReviewSnapshotAuthName }
func (codexReviewProvider) snapshotInstructionName() string { return codexReviewSnapshotInstrName }

func (codexReviewProvider) requiresExpiringCredential() bool { return true }

func (codexReviewProvider) acceptsAuthMode(mode CodexAuthMode) bool {
	return mode == CodexAuthSubscription || mode == CodexAuthAPIKey
}

func (codexReviewProvider) inspectAgentAuthSnapshot(
	mode CodexAuthMode, body []byte,
) ([]byte, *time.Time, error) {
	return codexReviewAgentAuthSnapshot(mode, body)
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
