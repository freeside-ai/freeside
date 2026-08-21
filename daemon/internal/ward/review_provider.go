package ward

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
