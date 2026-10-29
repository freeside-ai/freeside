package agentbaseline

import "github.com/freeside-ai/freeside/daemon/internal/domain"

// The harness builds the agent images pin (images/agent-claude and
// images/agent-codex Containerfiles). A test holds these to the
// Containerfiles, so an image bump that forgets the adapter fails there: a
// new harness build is a new adapter digest with no conformance record.
const (
	ClaudeCodeHarnessBuild = "claude-code 2.1.282"
	CodexHarnessBuild      = "codex-cli 0.162.1"
)

// ClaudeCallHarnessBuild is the Claude CLI build the judgment call launch was
// hand-audited on (devlog/2026-09-09-2145-subscription-judgments.md). It is
// not an image pin: a call runs the operator's host binary, which the
// -judgment-claude-sha256 flag pins by content. It differs from the ward
// build on purpose. The audit is evidence about the build it ran against, so
// the call adapters name that build and write their sendable efforts out,
// and neither a ward image bump nor a change to the claude_code effort table
// moves a call adapter digest.
const ClaudeCallHarnessBuild = "claude-code 2.1.267"

// Adapter build ids: the Freeside launch code each adapter fragment names.
// Bump one when the launch it describes changes what it honours. The call
// launch has two: v1 sends no effort, and v2 sends each effort the
// claude_code table can. v1 stays so a line a deployment adopted on it keeps
// its digest and its audit record. The review launch has two for the same
// reason: v1 sends no effort, and v2 sends each effort the codex_cli table
// can.
const (
	ClaudeWardAdapterBuild    = "claude_code_ward_v1"
	ClaudeCallAdapterBuild    = "claude_code_call_v2"
	ClaudeCallAdapterV1Build  = "claude_code_call_v1"
	CodexReviewAdapterBuild   = "codex_review_v2"
	CodexReviewAdapterV1Build = "codex_review_v1"
)

// Tree names of the baseline documents. Names enter no digest. The writer
// agents are named by WriterAgentName. ClaudeAgentName and ClaudeOfferName
// are the writer agent and native-default offer of a tree adopted before the
// writer roles named a model; the baseline no longer emits them, so a
// re-adopted tree has no such agent for a line to name.
const (
	ClaudeAgentName             = "claude-code-default"
	ClaudeCallAgentName         = "claude-code-call-default"
	ClaudeShadowReviewAgentName = "claude-shadow-review-default"
	ClaudeReviewAgentName       = "claude-review-default"
	CodexReviewAgentName        = "codex-review-default"
	ClaudeAdapterName           = ClaudeWardAdapterBuild
	ClaudeCallAdapterName       = ClaudeCallAdapterBuild
	CodexAdapterName            = CodexReviewAdapterBuild
	ClaudeOfferName             = domain.ClaudeCodeNativeDefaultRouteModelID
	ClaudeCallOfferName         = "claude-code-call-default"
	ClaudeShadowReviewOfferName = "claude-shadow-review-default"
	ClaudeReviewOfferName       = "claude-review-default"
	CodexOfferName              = "codex-review-default"
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
// from a mounted store). Runner conformance has no part in a call, so this
// adapter is in no conformance record: wardless admission proves it by the
// audit record that names its digest and build.
//
// It sends every effort the claude_code table can send today. The list is
// written out, not read from the table, because the audit covered exactly
// these efforts: when the table gains one, sending it takes a new adapter
// and a new audit, and a test holds this list to the table so the gap shows.
func ClaudeCallAdapter() (domain.AdapterFragment, error) {
	return claudeCallAdapter(ClaudeCallAdapterBuild, []domain.EffortLevel{
		domain.EffortHarnessDefault, domain.EffortLow, domain.EffortMedium, domain.EffortHigh, domain.EffortMax,
	})
}

// ClaudeCallAdapterV1 is the call adapter the baseline carried before the
// call launch could send an effort: the same launch, declaring
// harness_default as its only sendable effort. The baseline tree no longer
// names it. It stays defined because a deployment that adopted it holds its
// digest in a committed tree, and the daemon's audit record for that digest
// is held to this definition.
func ClaudeCallAdapterV1() (domain.AdapterFragment, error) {
	return claudeCallAdapter(ClaudeCallAdapterV1Build, []domain.EffortLevel{domain.EffortHarnessDefault})
}

func claudeCallAdapter(build string, sendable []domain.EffortLevel) (domain.AdapterFragment, error) {
	return sealAdapter(domain.AdapterFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		AdapterBuild:    build, HarnessBuild: ClaudeCallHarnessBuild,
		ClientKind: domain.HarnessClientClaudeCode, Vendor: domain.AgentVendorClaude,
		LaunchCapabilities: domain.NewLaunchCapabilitySet(
			domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput,
			domain.LaunchCapContextSeverance,
		),
		SendableEfforts: sendable,
	})
}

// CodexReviewAdapter is the adapter for the Codex review launch
// (internal/ward/codex_review.go): a read-only sandbox, an ephemeral session
// that ignores user configuration, the review instructions, an output
// schema, and the sanitized auth snapshot. It has no mutation tools and, like
// the Claude adapter, no auxiliary-inference control, so the review launch's
// `forbidden` policy is not covered until an adapter proves that control
// (#1980). The launch always passes a model and an effort, so the adapter
// sends every effort the codex_cli table can.
func CodexReviewAdapter() (domain.AdapterFragment, error) {
	return codexReviewAdapter(CodexReviewAdapterBuild, domain.CodexCLISendableEfforts())
}

// CodexReviewAdapterV1 is the review adapter the baseline carried while the
// review launch took its model and effort from daemon flags: the same
// launch, declaring harness_default as its only sendable effort. The
// baseline tree no longer names it. It stays defined because a deployment
// that adopted it holds its digest in a committed tree, and the conformance
// record for that digest is held to this definition.
func CodexReviewAdapterV1() (domain.AdapterFragment, error) {
	return codexReviewAdapter(CodexReviewAdapterV1Build, []domain.EffortLevel{domain.EffortHarnessDefault})
}

func codexReviewAdapter(build string, sendable []domain.EffortLevel) (domain.AdapterFragment, error) {
	return sealAdapter(domain.AdapterFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		AdapterBuild:    build, HarnessBuild: CodexHarnessBuild,
		ClientKind: domain.HarnessClientCodexCLI, Vendor: domain.AgentVendorCodex,
		LaunchCapabilities: domain.NewLaunchCapabilitySet(
			domain.LaunchCapReadTools, domain.LaunchCapInstructionDelivery,
			domain.LaunchCapStructuredOutput, domain.LaunchCapContextSeverance,
			domain.LaunchCapRouteStoreContract,
		),
		SendableEfforts: sendable,
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

// Adapters returns the baseline ward adapter declarations, including the
// retained review build. Declarations are not conformance observations.
func Adapters() ([]domain.AdapterFragment, error) {
	claude, err := ClaudeWardAdapter()
	if err != nil {
		return nil, err
	}
	codex, err := CodexReviewAdapter()
	if err != nil {
		return nil, err
	}
	codexV1, err := CodexReviewAdapterV1()
	if err != nil {
		return nil, err
	}
	return []domain.AdapterFragment{claude, codex, codexV1}, nil
}
