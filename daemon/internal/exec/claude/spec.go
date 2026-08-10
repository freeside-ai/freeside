package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// Ward-facing constants for the pinned Claude CLI in subscription_contained
// mode. #383 fixed the setup-token topology and proved these paths under a
// root writer. #378 drops the writer to an unprivileged UID, so the pinned
// image was re-probed for exactly what that changes: setpriv with an empty
// bounding set launches, the CLI itself runs as UID 1001, and the bundle at
// instructionBundlePath is readable there only once the seeder relaxes its
// mode (ward's instructionSeederCommand). A complete credentialed run under
// the drop is the operator's scripts/run-real-work.sh, not a unit test. The
// setup token is read from the identity-bound read-only volume only inside
// the launcher and never enters AgentSpec.Env.
const (
	credentialMountTarget = "/var/lib/freeside/claude-token" //nolint:gosec // G101: mount path, not credential bytes
	credentialTokenPath   = credentialMountTarget + "/token" //nolint:gosec // G101: fixed path, not credential bytes
	instructionBundlePath = "/root/.claude/CLAUDE.md"
	// workspaceDir is where the gate mounts the workspace volume; the agent
	// runs with it as the working directory.
	workspaceDir = "/workspace"
	// workspaceSizeMB bounds one run's workspace volume.
	workspaceSizeMB int64 = 4096
	agentUID              = "1001"
	agentGID              = "1001"
	// writerOutcomePrepareFailed is the pre-agent sentinel the root wrapper
	// writes when the project image's preparation command (implementer
	// workspace hydration) exits nonzero. It is distinct from the
	// token-missing 86 so the failure reads as an environment failure rather
	// than an agent failure, and the agent never launches.
	writerOutcomePrepareFailed = 87
	// prepareHome mirrors the verification room's HOME for the preparation
	// command (ward's ProjectImageRoom runs it with HOME=/tmp/freeside-home and
	// LC_ALL=C over the workspace). The implementer hydrates with the same
	// invocation, so it resolves the exact lockfile-pinned toolchain the
	// verifier will. Keep it in step with verification_room.go.
	prepareHome = "/tmp/freeside-home"
)

// ProviderEndpoints is the provider_only egress allowlist for the Claude
// CLI: the inference API alone. It is configuration, not contract, so a
// composition may widen it; the ward proves whatever list it is given is
// what the agent could actually reach.
func ProviderEndpoints() []string { return []string{"api.anthropic.com:443"} }

// agentEnv carries the fixed command-scope Git protected configuration the
// unprivileged writer needs for ward's deliberately root-owned workspace
// mount. A repository-local safe.directory cannot authorize itself: Git
// refuses the ownership mismatch before it trusts local configuration. The
// exact path preserves that check for every other repository the writer can
// reach, and ward appends only its authenticated proxy variables.
func agentEnv() []string {
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=" + workspaceDir,
	}
}

// RunIDFor is the deterministic ward run identity for one invocation:
// content-derived, so it satisfies ward's runIDPattern for any invocation id
// and a replayed start names the same runtime objects. Exported because the
// daemon composition derives the same run's workspace reference for
// admission (engine.WithAdmissionDerivation) and must agree with the driver.
func RunIDFor(id domain.InvocationID) string {
	sum := sha256.Sum256([]byte(id))
	return "c" + hex.EncodeToString(sum[:])[:31]
}

// WorkspaceFor is the workspace reference the admission records and the gate
// creates for one invocation.
func WorkspaceFor(id domain.InvocationID) string { return ward.WorkspaceRef(RunIDFor(id)) }

type claudeProvider struct {
	volumes AuthStoreVolumes
}

func (claudeProvider) RunID(id domain.InvocationID) string { return RunIDFor(id) }

func (claudeProvider) Workspace(id domain.InvocationID) string { return WorkspaceFor(id) }

func (claudeProvider) PrepareFailedStatus() int { return writerOutcomePrepareFailed }

func (claudeProvider) RenderPrompt(inputs stage.ProviderPromptInputs) (string, error) {
	return renderPromptParts(inputs)
}

// maxPromptBytes bounds the rendered prompt below Linux's 128-KiB
// MAX_ARG_STRLEN. It travels inside one sh -c argument because the writer gets
// no stdin and ward's mount vocabulary is volume-only. Shell quoting expands
// every apostrophe fourfold, so 31 KiB plus the fixed command remains below
// the kernel limit even for the worst input. A larger prompt needs a ward
// prompt-mount vocabulary, which is a shared contract change.
const (
	linuxMaxArgumentBytes = 128 << 10
	maxPromptBytes        = 31 << 10
)

// agentCommand is the pinned CLI's unattended argv. The workspace is the
// working directory, and the transcript goes to the §5.6 evidence subtree,
// which the repo walk skips. The root launcher declares that transcript as
// sensitive evidence before dropping privilege, so it crosses only through
// the evidence channel after the writer is gone.
//
// When prepare is non-empty the root phase hydrates the workspace with the
// project image's preparation command before ownership is handed to the agent,
// so the implementer can run the admitted verification recipe over the same
// dependencies the verifier will (#522). Hydration runs as root before the
// chown sweep, so the hydrated node_modules is dropped to the agent user with
// the rest of the tree; a nonzero prepare exit writes the pre-agent sentinel
// and never launches the agent. The tree is removed again in the epilogue
// before the outcome marker, so the git-blind export walk never sees it. An
// empty prepare keeps the attended launch command byte-identical.
func agentCommand(prompt, sessionID string, invocationID domain.InvocationID, prepare []string) []string {
	descriptor, err := json.Marshal(export.EvidenceSourceManifest{
		Version: export.EvidenceSourceVersion,
		Sources: []export.EvidenceSource{{
			Label: "agent-transcript", MediaType: "application/jsonl",
			Path: transcriptEvidencePath, HeadBinding: export.EvidenceHeadIndependent,
			SensitivityClass:     export.EvidenceSensitivitySensitive,
			ProducerInvocationID: string(invocationID),
		}},
	})
	if err != nil {
		panic("marshal fixed Claude transcript descriptor: " + err.Error())
	}
	// hydrate runs before the chown sweep; guardPrefix turns the token check
	// into a two-branch guard that reports the prepare failure and skips the
	// agent. Both are empty/"if " with no preparation, so the command stays
	// byte-identical for the attended path.
	hydrate, guardPrefix := "", "if "
	if len(prepare) > 0 {
		hydrate = "prepare_status=0; set +e; ( cd " + shellQuote(workspaceDir) +
			" && HOME=" + shellQuote(prepareHome) + " LC_ALL=C " + shellJoin(prepare) +
			" ); prepare_status=$?; set -e; "
		guardPrefix = "if [ \"$prepare_status\" -ne 0 ]; then status=" +
			strconv.Itoa(writerOutcomePrepareFailed) + "; elif "
	}
	return []string{"sh", "-c", fmt.Sprintf(
		"set -eu; "+
			"chmod 0711 /root; "+
			"rm -rf %s; %s"+
			"find %s -mindepth 1 -maxdepth 1 -exec chown -hR %s:%s {} +; "+
			"chown 0:0 %s; chmod 1777 %s; "+
			"mkdir -p %s; chown 0:0 %s; chmod 1777 %s; "+
			"mkdir -p %s; chown 0:0 %s; chmod 0700 %s; "+
			"printf '%%s\\n' %s > %s; chown 0:0 %s; chmod 0644 %s; "+
			"chown %s:%s %s %s; chmod 0700 %s %s; "+
			"cd %s; status=86; "+
			"%s[ -s %s ] && token=$(cat %s); then "+
			"set +e; HOME=/root CLAUDE_CONFIG_DIR=%s DISABLE_AUTOUPDATER=1 "+
			"DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 "+
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 IS_SANDBOX=1 "+
			"CLAUDE_CODE_OAUTH_TOKEN=\"$token\" "+
			"setpriv --reuid=%s --regid=%s --clear-groups "+
			"--inh-caps=-all --ambient-caps=-all --bounding-set=-all "+
			"--no-new-privs claude -p %s "+
			"--output-format stream-json --verbose --dangerously-skip-permissions "+
			"--safe-mode --session-id %s --append-system-prompt-file %s "+
			"> %s 2>&1; status=$?; set -e; unset token; fi; "+
			"rm -rf -- %s; "+
			"printf '%%s %%s\\n' %s \"$status\" > %s; sync; exit \"$status\"",
		shellQuote(path.Dir(transcriptPath)), hydrate,
		shellQuote(workspaceDir),
		agentUID, agentGID,
		shellQuote(workspaceDir), shellQuote(workspaceDir),
		shellQuote(path.Dir(transcriptPath)), shellQuote(path.Dir(transcriptPath)),
		shellQuote(path.Dir(transcriptPath)),
		shellQuote(path.Dir(writerOutcomePath)), shellQuote(path.Dir(writerOutcomePath)),
		shellQuote(path.Dir(writerOutcomePath)),
		shellQuote(string(descriptor)), shellQuote(transcriptDescriptorPath),
		shellQuote(transcriptDescriptorPath), shellQuote(transcriptDescriptorPath),
		agentUID, agentGID,
		shellQuote(ward.ClaudeContinuityTarget), shellQuote(ward.ClaudeSessionScratchTarget),
		shellQuote(ward.ClaudeContinuityTarget), shellQuote(ward.ClaudeSessionScratchTarget),
		shellQuote(workspaceDir),
		guardPrefix,
		shellQuote(credentialTokenPath), shellQuote(credentialTokenPath),
		shellQuote(ward.ClaudeConfigRootTarget), agentUID, agentGID, shellQuote(prompt),
		shellQuote(sessionID), shellQuote(instructionBundlePath),
		shellQuote(transcriptPath), shellQuote(workspaceDir+"/node_modules"),
		shellQuote(ward.WriterNoncePlaceholder),
		shellQuote(writerOutcomePath),
	)}
}

// shellJoin renders an argv as space-separated single-quoted shell words.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func sessionIDFor(id domain.InvocationID) string {
	sum := sha256.Sum256([]byte("claude-session:" + string(id)))
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(sum[:16])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" +
		encoded[16:20] + "-" + encoded[20:]
}

func renderPromptParts(inputs stage.ProviderPromptInputs) (string, error) {
	for _, part := range []struct {
		name string
		body []byte
	}{
		{"prompt package", inputs.PromptPackage},
		{"specification", inputs.Specification},
		{"policy", inputs.Policy},
	} {
		if !utf8.Valid(part.body) {
			return "", fmt.Errorf("%w: %s is not valid UTF-8",
				ErrUnsupportedStart, part.name)
		}
	}
	prompt := string(inputs.PromptPackage) +
		"\n\n--- Approved work item specification ---\n\n" +
		string(inputs.Specification) +
		"\n\n--- Resolved per-run policy ---\n\n" +
		string(inputs.Policy) + "\n"
	if len(prompt) > maxPromptBytes {
		return "", fmt.Errorf("%w: rendered prompt is %d bytes, limit %d",
			ErrUnsupportedStart, len(prompt), maxPromptBytes)
	}
	return prompt, nil
}

// shellQuote renders one argument as a single-quoted shell word.
func shellQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := range len(s) {
		if s[i] == '\'' {
			out = append(out, `'\''`...)
			continue
		}
		out = append(out, s[i])
	}
	out = append(out, '\'')
	return string(out)
}

// handoffSpec renders one admitted start as a ward handoff request.
//
// Every containment fact is asserted here and re-verified by the gate: the
// credential mode and egress profile must be exactly what Phase 1A.2
// admits, the leased auth-store volume comes from the trusted identity
// binding rather than a driver-side name, and the vendor instructions are
// the materialized bytes the admission froze.
func (p claudeProvider) HandoffSpec(
	ctx context.Context, in stage.ProviderHandoffInput,
) (ward.HandoffSpec, error) {
	id, spec := in.InvocationID, in.Spec
	if spec.CredentialMode != domain.CredentialSubscriptionContained {
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: credential mode %q is not subscription_contained",
			ErrUnsupportedStart, spec.CredentialMode)
	}
	if spec.EgressProfile != domain.EgressProviderOnly {
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: egress profile %q is not provider_only", ErrUnsupportedStart, spec.EgressProfile)
	}
	if spec.AuthIdentityID == "" {
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: an agent stage requires an auth identity", ErrUnsupportedStart)
	}
	if want := WorkspaceFor(id); spec.Workspace != want {
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: admitted workspace %q, driver derives %q",
			ErrUnsupportedStart, spec.Workspace, want)
	}
	volume, err := p.volumes.AuthStoreVolume(ctx, spec.AuthIdentityID)
	if err != nil {
		return ward.HandoffSpec{}, fmt.Errorf("resolve auth store volume: %w", err)
	}

	hs := ward.HandoffSpec{
		RunID:           in.RunID,
		WorkspaceSizeMB: workspaceSizeMB,
		Seed: ward.WorkspaceSeed{
			Mode: ward.SeedBaseCheckout, SourceDir: in.Seed, Base: spec.Base,
		},
		Agent: ward.AgentSpec{
			Image:             string(spec.ImageRef),
			Command:           agentCommand(in.Prompt, sessionIDFor(id), id, in.Preparation),
			Env:               agentEnv(),
			EgressProfile:     spec.EgressProfile,
			OutcomeMarkerPath: writerOutcomePath,
			LaunchState:       ward.LaunchStateClaudeClean,
			CredentialMounts: []ward.CredentialMount{{
				Volume: volume, Target: credentialMountTarget,
				Manifest: ward.CredentialManifestSetupToken,
			}},
			VendorInstructions: in.Instructions,
			InstructionPolicy:  ward.ClaudeInvocationInstructionPolicy(),
		},
		AuthStoreLease: &ward.AuthStoreLeaseClaim{
			AuthIdentityID: spec.AuthIdentityID, Holder: id,
		},
	}
	// The repository-instruction contract must resolve at every process-entry
	// shape before anything runs: a boundary that cannot name its trusted base
	// is a stage that would run some entries without repository authority.
	for _, boundary := range ward.AllInvocationBoundaries {
		if _, err := hs.RepositoryInstructionBase(boundary); err != nil {
			return ward.HandoffSpec{}, fmt.Errorf("instruction base for %s: %w", boundary, err)
		}
	}
	return hs, nil
}

// transcriptPath is where the CLI's stream-json transcript lands: inside the
// reserved evidence subtree, which the repo-change walk skips entirely, so
// the transcript can only leave through the root launcher's declared evidence
// descriptor and never pollutes the candidate commit.
const (
	transcriptEvidencePath   = export.EvidenceWorkspaceDir + "/agent-transcript.jsonl"
	transcriptPath           = workspaceDir + "/" + transcriptEvidencePath
	transcriptDescriptorPath = workspaceDir + "/" + export.EvidenceDescriptorPath
)

const writerOutcomePath = workspaceDir + "/" + export.EvidenceWorkspaceDir + "/.control/writer-outcome"
