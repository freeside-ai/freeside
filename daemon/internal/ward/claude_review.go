package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// claude_review.go is the thin Claude provider (#865) on the shared,
// provider-neutral review-runtime core (#872). It reuses the audited container
// topology unchanged (read-only workspace snapshot under an exclusive volume
// lease, a networkless read-only observer, provider-only egress) and varies
// only the vendor-specific decisions: a setup-token credential with no
// host-side refresh, the Claude CLI review command, the Anthropic provider
// labels, and a distinct topology and evidence version namespace. It copies
// none of the Codex topology; the Codex API keeps its signatures and the
// Claude runtime is reached through the NewClaudeReview* constructors below.

const (
	// claudeReviewTopologyVersion is the Claude read-only review topology
	// generation, distinct from any Codex version so a binding can never be
	// validated against the wrong provider's expectations.
	claudeReviewTopologyVersion = "claude_review_read_only_v1"

	// claudeReviewConfigTarget is the writable per-invocation CLAUDE_CONFIG_DIR
	// on the fresh container rootfs; claudeReviewHome is a clean per-invocation
	// HOME. Both are created by the review command. The credential and
	// instruction bytes live on the separate read-only snapshot volume.
	claudeReviewConfigTarget = "/var/lib/freeside/claude-review-config"
	claudeReviewHome         = "/var/lib/freeside/claude-review-home"

	// The two snapshot basenames a Claude review reads read-only: the raw setup
	// token and the review instruction the CLI appends as a system prompt.
	claudeReviewSnapshotTokenName = "token"
	claudeReviewSnapshotInstrName = "CLAUDE.md"

	claudeReviewSnapshotTokenSource = codexReviewSnapshotTarget + "/" + claudeReviewSnapshotTokenName
	claudeReviewSnapshotInstrSource = codexReviewSnapshotTarget + "/" + claudeReviewSnapshotInstrName
)

// claudeReviewProvider is the Claude shadow ReviewSource's vendor seam. Every
// value it returns is a Claude-native constant; the neutral core reads each one
// so no Codex value leaks into a Claude review.
type claudeReviewProvider struct{}

var _ reviewProvider = claudeReviewProvider{}

func (claudeReviewProvider) sourceLabel() string   { return "claude_local" }
func (claudeReviewProvider) providerLabel() string { return "anthropic" }

func (claudeReviewProvider) topologyVersion() string { return claudeReviewTopologyVersion }

func (claudeReviewProvider) completionEvidenceVersion() string { return "claude-review-completion-v1" }
func (claudeReviewProvider) resultEvidenceVersion() string     { return "claude-review-result-v1" }
func (claudeReviewProvider) configurationVersion() string      { return "claude-review-configuration-v1" }

func (claudeReviewProvider) promptProtocol() string { return "claude-production-review-prompt-v1" }

func (claudeReviewProvider) reviewCommand(
	workspaceTarget, model, reasoningEffort, prompt string,
) []string {
	return claudeReviewCommand(workspaceTarget, model, reasoningEffort, prompt)
}

func (claudeReviewProvider) vendor() domain.AgentVendor { return domain.AgentVendorClaude }

// reviewPrompt reuses the shared production review prompt text: the P0–P3
// severity definitions and the daemon-owned Freeside rules are identical across
// providers, and the prompt protocol version namespaces it as Claude's.
func (claudeReviewProvider) reviewPrompt(req exec.ReviewRequest) string {
	return codexProductionReviewPrompt(req)
}

func (claudeReviewProvider) terminalFailureMessage(events []byte) string {
	var message string
	for line := range bytes.SplitSeq(events, []byte("\n")) {
		var terminal struct {
			Type    string `json:"type"`
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
		}
		if err := RejectDuplicateJSONKeys(line); err != nil {
			continue
		}
		if err := json.Unmarshal(line, &terminal); err != nil ||
			terminal.Type != "result" || !terminal.IsError {
			continue
		}
		message = terminal.Result
	}
	return message
}

func (claudeReviewProvider) reviewContainerSuffix() string { return "-claude" }

// legacyTopologyVersions is nil: the Claude review topology has no prior
// generation to reap.
func (claudeReviewProvider) legacyTopologyVersions() []string { return nil }

func (claudeReviewProvider) homeTarget() string       { return claudeReviewHome }
func (claudeReviewProvider) configHomeTarget() string { return claudeReviewConfigTarget }

// containerEnv carries only non-secret launcher configuration. The setup token
// never appears here: the review command reads it from the read-only snapshot
// file and passes it to the CLI inline, so it never lands in the persisted,
// digested launcher environment.
func (claudeReviewProvider) containerEnv() []string {
	return []string{
		"HOME=" + claudeReviewHome,
		"CLAUDE_CONFIG_DIR=" + claudeReviewConfigTarget,
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"IS_SANDBOX=1",
	}
}

func (claudeReviewProvider) snapshotCredentialName() string  { return claudeReviewSnapshotTokenName }
func (claudeReviewProvider) snapshotInstructionName() string { return claudeReviewSnapshotInstrName }

// requiresExpiringCredential is false: a setup token has no access-token
// lifetime, so the config lifetime-floor and refresh-threshold requirements do
// not apply and the binding carries no access-token expiry.
func (claudeReviewProvider) requiresExpiringCredential() bool { return false }

func (claudeReviewProvider) acceptsAuthMode(mode CodexAuthMode) bool {
	return mode == CodexAuthSetupToken
}

func (claudeReviewProvider) inspectAgentAuthSnapshot(
	mode CodexAuthMode, body []byte,
) ([]byte, *time.Time, error) {
	if mode != CodexAuthSetupToken {
		return nil, nil, errors.New("claude review requires the setup-token auth mode")
	}
	token, err := inspectClaudeSetupToken(body)
	if err != nil {
		return nil, nil, err
	}
	return token, nil, nil
}

func (claudeReviewProvider) newCredentialStrategy(*CodexReviewLifecycle) credentialStrategy {
	return claudeCredentialStrategy{}
}

// inspectClaudeSetupToken derives the container-facing setup token from the host
// snapshot body. It trims exactly one trailing newline (the shape a keychain or
// file export commonly carries), then requires a non-empty, control-character-
// free token within the shared snapshot size bound. It fails closed on anything
// else so a malformed credential never reaches the container.
func inspectClaudeSetupToken(body []byte) ([]byte, error) {
	token := body
	if n := len(token); n > 0 && token[n-1] == '\n' {
		token = token[:n-1]
	}
	switch {
	case len(token) == 0:
		return nil, errors.New("setup-token snapshot is empty")
	case len(token) > maxCodexAuthSnapshotBytes:
		return nil, errors.New("setup-token snapshot exceeds the maximum size")
	}
	for _, b := range token {
		if b < 0x20 || b == 0x7f {
			return nil, errors.New("setup-token snapshot carries a control character")
		}
	}
	return bytes.Clone(token), nil
}

// claudeCredentialStrategy delivers the static setup token: there is no
// host-side refresh, so launch admission is unconditional and the held lease is
// a no-op. The credential bytes are seeded onto the read-only snapshot volume by
// the shared seeder; this strategy adds no host-side mutation to hold across
// container start.
type claudeCredentialStrategy struct{}

var _ credentialStrategy = claudeCredentialStrategy{}

func (claudeCredentialStrategy) checkLaunchAdmission(
	context.Context, CodexReviewConfig, CodexReviewLaunchSpec,
) error {
	return nil
}

func (claudeCredentialStrategy) acquireLease(
	context.Context, CodexReviewConfig, CodexReviewLaunchSpec,
) (reviewAuthLease, error) {
	return claudeAuthLease{}, nil
}

// claudeAuthLease is the no-op host-credential lease for a non-refreshing
// credential: nothing to re-verify mid-launch, a plain child context for the
// atomic start, and an idempotent release.
type claudeAuthLease struct{}

var _ reviewAuthLease = claudeAuthLease{}

func (claudeAuthLease) verifyStillAdmissible(context.Context) error { return nil }

func (claudeAuthLease) reserveStartAdmission(
	ctx context.Context,
) (context.Context, context.CancelFunc, error) {
	startCtx, cancel := context.WithCancel(ctx)
	return startCtx, cancel, nil
}

func (claudeAuthLease) release(context.Context) error { return nil }

// claudeReviewCommand builds the in-container Claude review argv. It mirrors
// codexReviewCommand's fail-closed shape: the setup prologue runs under `set -e`
// so a failure aborts the container before the CLI runs, and `set +e` is relaxed
// only immediately before `claude -p` so a nonzero CLI exit is still captured
// into the shared status file. The prompt is a dedicated positional argument
// ("$1"), never interpolated into the shell program.
//
// The setup token is read from the read-only snapshot file into a shell
// variable and handed to the CLI as CLAUDE_CODE_OAUTH_TOKEN inline, exactly as
// the exec Claude runtime does (daemon/internal/exec/claude/spec.go), so the
// token never enters AgentSpec.Env, the launcher-environment digest, or any
// durable state. HOME, CLAUDE_CONFIG_DIR, and the disable/sandbox flags arrive
// through the container environment (claudeReviewProvider.containerEnv).
func claudeReviewCommand(workspaceTarget, model, reasoningEffort, prompt string) []string {
	// The findings schema is the shared provider-neutral contract, passed inline
	// via --json-schema (the CLI accepts the schema JSON as the argument value).
	command := "set -e; mkdir -p " + shellQuote(claudeReviewHome) + "; " +
		"mkdir -p " + shellQuote(claudeReviewConfigTarget) + "; " +
		"mkdir -p " + shellQuote(codexReviewOutputDir) + "; " +
		"cd " + shellQuote(workspaceTarget) + "; " +
		"set +e; token=\"$(cat " + shellQuote(claudeReviewSnapshotTokenSource) + ")\"; " +
		"CLAUDE_CODE_OAUTH_TOKEN=\"$token\" claude -p \"$1\"" +
		" --output-format json" +
		" --json-schema " + shellQuote(reviewFindingsJSONSchema) +
		" --model " + shellQuote(model) +
		" --effort " + shellQuote(reasoningEffort) +
		" --no-session-persistence" +
		" --append-system-prompt-file " + shellQuote(claudeReviewSnapshotInstrSource) +
		// --safe-mode disables every candidate-controlled customization (the
		// workspace's .claude settings, commands, hooks, and MCP config), so only
		// the admitted instruction bundle and administrator policy are
		// authoritative. Without it, --dangerously-skip-permissions would let a
		// reviewed repo's own configuration influence its shadow review and
		// suppress or fabricate findings. Matches the exec Claude launcher
		// (daemon/internal/exec/claude/spec.go) and the plan's launch contract.
		" --safe-mode" +
		" --dangerously-skip-permissions" +
		" --allowedTools Read Grep Glob" +
		" > " + shellQuote(codexReviewEventsPath) + " 2>&1; " +
		"review_status=$?; unset token; " +
		// Extract the structured findings object from the CLI's JSON envelope to
		// the shared result path. jq is present in the pinned agent image; a
		// missing structured_output leaves the result empty, which the collector
		// classifies as a contradiction.
		"jq -e .structured_output " + shellQuote(codexReviewEventsPath) +
		" > " + shellQuote(codexReviewResultPath) + " 2>/dev/null; " +
		"printf '%s\\n' \"$review_status\" > " + shellQuote(codexReviewStatusPath) +
		"; exit \"$review_status\""
	return []string{"sh", "-c", command, "freeside-claude-review", prompt}
}

// NewClaudeReviewLifecycle builds the Claude review runtime owner on the shared
// review lifecycle, injecting the Claude provider seam.
func NewClaudeReviewLifecycle(
	rt Runtime, cfg Config, authorizeRuntimeResources RuntimeResourceAuthorizer,
) (*CodexReviewLifecycle, error) {
	return newReviewLifecycle(claudeReviewProvider{}, rt, cfg, authorizeRuntimeResources)
}

// NewClaudeReviewSource builds the Claude shadow ReviewSource on the shared
// review source, injecting the Claude provider seam before the shared
// constructor validates the configuration digest against it.
func NewClaudeReviewSource(cfg CodexReviewSourceConfig) (*CodexReviewSource, error) {
	cfg.provider = claudeReviewProvider{}
	return NewCodexReviewSource(cfg)
}

// ClaudeReviewConfigurationDigest binds the Claude review trust profile to every
// deployment-owned input that can change a production review's behavior, exactly
// as CodexReviewConfigurationDigest does for Codex.
func ClaudeReviewConfigurationDigest(
	cfg CodexReviewConfig,
	workspaceSizeMB int64,
	authMode CodexAuthMode,
	authIdentityID domain.AuthIdentityID,
	costOwner string,
) (domain.Digest, error) {
	return reviewConfigurationDigest(
		claudeReviewProvider{}, cfg, workspaceSizeMB, authMode, authIdentityID, costOwner,
	)
}
