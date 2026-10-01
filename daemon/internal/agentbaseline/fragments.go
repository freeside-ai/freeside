package agentbaseline

import (
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The harness builds the agent images pin (images/agent-claude and
// images/agent-codex Containerfiles). A test holds these to the
// Containerfiles, so an image bump that forgets the adapter fails there: a
// new harness build is a new adapter digest with no conformance record.
const (
	ClaudeCodeHarnessBuild = "claude-code 2.1.220"
	CodexHarnessBuild      = "codex-cli 0.147.0"
)

// Adapter build ids: the Freeside launch code each adapter fragment names.
// Bump one when the launch it describes changes what it honours.
const (
	ClaudeWardAdapterBuild  = "claude_code_ward_v1"
	CodexReviewAdapterBuild = "codex_review_v1"
)

// Tree names of the baseline documents. Names enter no digest.
const (
	ClaudeAgentName      = "claude-code-default"
	CodexReviewAgentName = "codex-review-default"
	ClaudeAdapterName    = ClaudeWardAdapterBuild
	CodexAdapterName     = CodexReviewAdapterBuild
	ClaudeOfferName      = domain.ClaudeCodeNativeDefaultRouteModelID
	CodexOfferName       = "codex-review-default"
	// DefaultClaudeRoute and DefaultCodexRoute name the route of an identity
	// adoption enrolls. An identity `auth add` already enrolled keeps the
	// route name its enrollment carries.
	DefaultClaudeRoute = "anthropic_claude_subscription"
	DefaultCodexRoute  = "openai_chatgpt_codex"
)

// ClaudeWardAdapter is the adapter for the Claude Code ward launch
// (internal/exec/claude): a writable or read-only workspace by mount, the
// instruction file appended to the system prompt, outcomes validated from
// fixed-path files, a fresh container home with no user configuration, and
// the sanitized setup-token store. It declares neither exact resume (every
// launch is a new session) nor auxiliary-inference control (the pinned build
// has no switch for its side requests).
func ClaudeWardAdapter() (domain.AdapterFragment, error) {
	return sealAdapter(domain.AdapterFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		AdapterBuild:    ClaudeWardAdapterBuild, HarnessBuild: ClaudeCodeHarnessBuild,
		ClientKind: domain.HarnessClientClaudeCode, Vendor: domain.AgentVendorClaude,
		LaunchCapabilities: domain.NewLaunchCapabilitySet(
			domain.LaunchCapReadTools, domain.LaunchCapMutationTools,
			domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput,
			domain.LaunchCapContextSeverance, domain.LaunchCapRouteStoreContract,
		),
		SendableEfforts: domain.ClaudeCodeSendableEfforts(),
	})
}

// CodexReviewAdapter is the adapter for the Codex review launch
// (internal/ward/codex_review.go): a read-only sandbox, an ephemeral session
// that ignores user configuration, the review instructions, an output
// schema, and the sanitized auth snapshot. It has no mutation tools and, like
// the Claude adapter, no auxiliary-inference control, so the review launch's
// `forbidden` policy is not covered until an adapter proves that control
// (#898). The review launch still takes its model and effort from the review
// flags, so the adapter sends no effort of its own.
func CodexReviewAdapter() (domain.AdapterFragment, error) {
	return sealAdapter(domain.AdapterFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		AdapterBuild:    CodexReviewAdapterBuild, HarnessBuild: CodexHarnessBuild,
		ClientKind: domain.HarnessClientCodexCLI, Vendor: domain.AgentVendorCodex,
		LaunchCapabilities: domain.NewLaunchCapabilitySet(
			domain.LaunchCapReadTools, domain.LaunchCapInstructionDelivery,
			domain.LaunchCapStructuredOutput, domain.LaunchCapContextSeverance,
			domain.LaunchCapRouteStoreContract,
		),
		SendableEfforts: []domain.EffortLevel{domain.EffortHarnessDefault},
	})
}

func sealAdapter(adapter domain.AdapterFragment) (domain.AdapterFragment, error) {
	digest, err := adapter.ComputeDigest()
	if err != nil {
		return domain.AdapterFragment{}, err
	}
	adapter.Digest = digest
	return adapter, adapter.Validate()
}

// Adapters returns the baseline adapters, the builds whose capability sets
// this package declares.
func Adapters() ([]domain.AdapterFragment, error) {
	claude, err := ClaudeWardAdapter()
	if err != nil {
		return nil, err
	}
	codex, err := CodexReviewAdapter()
	if err != nil {
		return nil, err
	}
	return []domain.AdapterFragment{claude, codex}, nil
}

// ConformanceRecords returns one passed adapter conformance record for each
// baseline adapter, proving exactly its declared set. The caller records
// them only after the ward suite passed for the running configuration: the
// record says the launch code for this build honours the set, and the ward
// pass says the runner it launches into conforms.
func ConformanceRecords(provedAt time.Time) ([]domain.AdapterConformance, error) {
	adapters, err := Adapters()
	if err != nil {
		return nil, err
	}
	records := make([]domain.AdapterConformance, 0, len(adapters))
	for _, adapter := range adapters {
		record, err := domain.NewAdapterConformance(domain.AdapterConformanceInput{
			AdapterDigest: adapter.Digest, Outcome: domain.ConformancePassed,
			ProvedCapabilities: adapter.LaunchCapabilities, ProvedAt: provedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("adapter %s conformance: %w", adapter.AdapterBuild, err)
		}
		records = append(records, record)
	}
	return records, nil
}
