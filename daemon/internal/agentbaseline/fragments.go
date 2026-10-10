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

// ClaudeCallHarnessBuild is the Claude CLI build the judgment call launch was
// hand-audited on (devlog/2026-09-09-2145-subscription-judgments.md). It is
// not an image pin: a call runs the operator's host binary, which the
// -judgment-claude-sha256 flag pins by content. It differs from the ward
// build on purpose. The audit is evidence about the build it ran against, so
// the call adapter names that build, and a ward image bump moves no call
// adapter digest.
const ClaudeCallHarnessBuild = "claude-code 2.1.267"

// Adapter build ids: the Freeside launch code each adapter fragment names.
// Bump one when the launch it describes changes what it honours.
const (
	ClaudeWardAdapterBuild  = "claude_code_ward_v1"
	ClaudeCallAdapterBuild  = "claude_code_call_v1"
	CodexReviewAdapterBuild = "codex_review_v1"
)

// Tree names of the baseline documents. Names enter no digest.
const (
	ClaudeAgentName       = "claude-code-default"
	ClaudeCallAgentName   = "claude-code-call-default"
	CodexReviewAgentName  = "codex-review-default"
	ClaudeAdapterName     = ClaudeWardAdapterBuild
	ClaudeCallAdapterName = ClaudeCallAdapterBuild
	CodexAdapterName      = CodexReviewAdapterBuild
	ClaudeOfferName       = domain.ClaudeCodeNativeDefaultRouteModelID
	CodexOfferName        = "codex-review-default"
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

// ClaudeCallAdapter is the adapter for the Claude Code judgment call launch
// (internal/claudeinference): one print-mode turn with every tool disabled,
// the site's instruction as the system prompt, a JSON completion decoded
// under the site's output contract, and a harness severed from user and
// project settings. It declares no tool capability and no route-store
// contract (the call's credential arrives in the driver's environment, not
// from a mounted store), and it sends no effort. Runner conformance has no
// part in a call, so this adapter is in no conformance record: wardless
// admission proves it by the audit record that names its digest and build.
func ClaudeCallAdapter() (domain.AdapterFragment, error) {
	return sealAdapter(domain.AdapterFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		AdapterBuild:    ClaudeCallAdapterBuild, HarnessBuild: ClaudeCallHarnessBuild,
		ClientKind: domain.HarnessClientClaudeCode, Vendor: domain.AgentVendorClaude,
		LaunchCapabilities: domain.NewLaunchCapabilitySet(
			domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput,
			domain.LaunchCapContextSeverance,
		),
		SendableEfforts: []domain.EffortLevel{domain.EffortHarnessDefault},
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

// Adapters returns the baseline ward adapters, the builds whose capability
// sets a conformance record proves. The call adapter is not among them.
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
