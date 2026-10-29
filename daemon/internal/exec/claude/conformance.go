package claude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
	"github.com/freeside-ai/freeside/daemon/internal/procbound"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const conformanceOutput = "conformance/output.txt"

// ConformanceTask keeps the two challenges out of the user prompt: one is
// available only through a workspace read, the other only through instructions.
// Seed is a private, disposable repository owned by the caller.
type ConformanceTask struct {
	Seed                        string
	Base                        domain.BaseRevision
	ReadNonce, InstructionNonce string
}

// NewConformanceTask authors a small local base rather than letting repository
// instructions or dependencies turn a capability test into unrelated work.
// base supplies the configured repository identity; the fixture owns its SHA.
func NewConformanceTask(ctx context.Context, dir string, base domain.BaseRevision) (ConformanceTask, error) {
	var challenge [32]byte
	if _, err := rand.Read(challenge[:]); err != nil {
		return ConformanceTask{}, err
	}
	task := ConformanceTask{
		Seed: dir, Base: base,
		ReadNonce: hex.EncodeToString(challenge[:16]), InstructionNonce: hex.EncodeToString(challenge[16:]),
	}
	if err := os.MkdirAll(filepath.Join(dir, "conformance"), 0o700); err != nil {
		return ConformanceTask{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "conformance/input.txt"), []byte(task.ReadNonce+"\n"), 0o600); err != nil {
		return ConformanceTask{}, err
	}
	git := func(args ...string) (string, error) {
		cmd := osexec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed git operations over the daemon-authored fixture
		procbound.Bind(cmd, 0)
		defer procbound.Reap(cmd)
		cmd.Dir = dir
		cmd.Env = append(gitenv.Scrubbed(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=Freeside Conformance", "GIT_AUTHOR_EMAIL=conformance@invalid",
			"GIT_COMMITTER_NAME=Freeside Conformance", "GIT_COMMITTER_EMAIL=conformance@invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("conformance git %s: %w: %s", args[0], err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"add", "conformance/input.txt"},
		{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "commit", "-m", "Seed conformance challenge"},
	} {
		if _, err := git(args...); err != nil {
			return ConformanceTask{}, err
		}
	}
	sha, err := git("rev-parse", "HEAD")
	if err != nil {
		return ConformanceTask{}, err
	}
	task.Base.BaseSHA = sha
	if err := task.Base.Validate(); err != nil {
		return ConformanceTask{}, err
	}
	if _, err := git("checkout", "--detach", sha); err != nil {
		return ConformanceTask{}, err
	}
	// Match the daemon-owned repository marks that ward requires on every
	// seed, even this disposable local base. They influence no git behavior.
	if _, err := git("config", "--local", "freeside.transport.repo", task.Base.Repo); err != nil {
		return ConformanceTask{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, ".git/freeside-repository-id"), []byte(strconv.FormatInt(task.Base.RepositoryID, 10)+"\n"), 0o600); err != nil {
		return ConformanceTask{}, err
	}
	return task, nil
}

// ConformanceInput names the same identity, image, and model selection the
// implementer launches. Volumes must resolve the generation selected for this
// invocation, independently of any legacy identity binding.
type ConformanceInput struct {
	Task                       ConformanceTask
	InvocationID               domain.InvocationID
	Image                      domain.ImageRef
	Identity                   domain.AuthIdentityID
	RouteModelID, NativeEffort string
	Volumes                    AuthStoreVolumes
}

// ConformanceHandoff uses the production provider builder, including its
// prompt protocol, outcome marker, credential mount, and clean launch state.
func ConformanceHandoff(ctx context.Context, in ConformanceInput) (ward.HandoffSpec, error) {
	if in.Volumes == nil || in.Task.ReadNonce == "" || in.Task.InstructionNonce == "" || in.InvocationID == "" {
		return ward.HandoffSpec{}, fmt.Errorf("incomplete Claude conformance input")
	}
	instructions := []byte("Conformance instruction token: " + in.Task.InstructionNonce + "\n")
	hs, err := (claudeProvider{volumes: in.Volumes}).HandoffSpec(ctx, stage.ProviderHandoffInput{
		InvocationID: in.InvocationID, RunID: RunIDFor(in.InvocationID), Seed: in.Task.Seed,
		Spec: exec.StartSpec{
			Base: in.Task.Base, ImageRef: in.Image, AuthIdentityID: in.Identity,
			Workspace: WorkspaceFor(in.InvocationID), CredentialMode: domain.CredentialSubscriptionContained,
			EgressProfile: domain.EgressProviderOnly, RouteModelID: in.RouteModelID, NativeEffort: in.NativeEffort,
		},
		PromptDelivery: launchDelivery,
		Prompt: "Perform only this small conformance task. Read conformance/input.txt. " +
			"Write its text and the conformance instruction token from your system instructions to " + conformanceOutput +
			". Also write both values to " + export.SummaryEvidencePath + ". Do not commit. Then finish.",
		Instructions: ward.VendorInstructions{
			Vendor: domain.AgentVendorClaude, Delivery: domain.VendorInstructionDeliveryAppendFile, Present: true,
			Digest: domain.Digest(contentaddr.Sum(instructions)), Body: instructions,
		},
	})
	if err != nil {
		return ward.HandoffSpec{}, err
	}
	// The shared stage driver normally supplies the launch size after the
	// provider builder. This task is a conformance launch, with ward's fixed
	// conformance budget rather than a project's build budget.
	hs.Class, hs.Size = ward.LaunchConformance, ward.DefaultLaunchSize(ward.LaunchConformance)
	return hs, nil
}

// JudgeConformance consumes only gate-verified exports and observations. The
// transcript and blobs are re-hashed at this read boundary. A completed run
// may prove a smaller set; absent or unreadable exports are failed runs.
func JudgeConformance(result *ward.HandoffResult, task ConformanceTask, id domain.InvocationID, authorities []string) (domain.LaunchCapabilitySet, error) {
	if result == nil || result.ExportDir == "" || task.ReadNonce == "" || task.InstructionNonce == "" {
		return nil, fmt.Errorf("claude conformance has no export or challenge")
	}
	var summary, transcript, output []byte
	structured := false
	for _, entry := range result.Evidence.Entries {
		if entry.Label != "agent-transcript" && entry.Label != export.SummaryEvidenceLabel {
			continue
		}
		body, err := conformanceBlob(result.ExportDir, export.EvidenceBlobsDirname, entry.Digest, entry.Size)
		if err != nil {
			return nil, err
		}
		if entry.Label == "agent-transcript" {
			transcript = body
		} else {
			summary = body
			// The export normalizes away source paths. This label is authored
			// by the production launcher only for SummaryEvidencePath.
			structured = result.EvidencePresent && entry.MediaType == "text/markdown" &&
				entry.Provenance.ProducerInvocationID == string(id)
		}
	}
	if len(transcript) == 0 {
		return nil, fmt.Errorf("claude conformance has no transcript")
	}
	for _, entry := range result.Manifest.Entries {
		if entry.Path != conformanceOutput || entry.Kind != export.EntryRegular || entry.Digest == nil || entry.Size == nil || entry.BlobOmitted {
			continue
		}
		var err error
		output, err = conformanceBlob(result.ExportDir, "blobs", *entry.Digest, *entry.Size)
		if err != nil {
			return nil, err
		}
	}
	severed, completed, err := judgeConformanceTranscript(transcript, sessionIDFor(id))
	if err != nil {
		return nil, err
	}
	observed := domain.LaunchCapabilitySet{}
	add := func(capability domain.LaunchCapability, holds bool) {
		if holds {
			observed = append(observed, capability)
		}
	}
	add(domain.LaunchCapReadTools, bytes.Contains(summary, []byte(task.ReadNonce)))
	add(domain.LaunchCapInstructionDelivery, bytes.Contains(summary, []byte(task.InstructionNonce)))
	add(domain.LaunchCapMutationTools, bytes.Contains(output, []byte(task.ReadNonce)) && bytes.Contains(output, []byte(task.InstructionNonce)))
	add(domain.LaunchCapStructuredOutput, structured)
	add(domain.LaunchCapContextSeverance, severed)
	store, egress := result.AuthStore, result.Egress
	want, got := slices.Clone(authorities), slices.Clone(egress.Allowlist)
	slices.Sort(want)
	slices.Sort(got)
	add(domain.LaunchCapRouteStoreContract, completed && store.Leased && store.Shared && store.PostAttested &&
		store.AuthIdentityID != "" && store.Holder == id && store.PreDigest != "" && store.PreDigest == store.PostDigest && !store.Mutated &&
		egress.Profile == domain.EgressProviderOnly && egress.HostOnly && egress.RegistrySetDigest == "" &&
		len(want) > 0 && slices.Equal(want, got))
	return domain.NewLaunchCapabilitySet(observed...), nil
}

func conformanceBlob(root, channel string, digest export.Digest, size int64) ([]byte, error) {
	hexDigest, ok := contentaddr.Parse(string(digest))
	const limit = 16 << 20
	if !ok || size < 0 || size > limit {
		return nil, fmt.Errorf("invalid conformance blob digest or size")
	}
	f, err := os.Open(filepath.Join(root, channel, "sha256", hexDigest)) //nolint:gosec // fixed channel and validated content address under gate-owned export
	if err != nil {
		return nil, fmt.Errorf("open conformance blob: %w", err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != size || contentaddr.Sum(body) != string(digest) {
		return nil, fmt.Errorf("conformance blob failed digest/size verification")
	}
	return body, nil
}

func judgeConformanceTranscript(body []byte, sessionID string) (severed, completed bool, err error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Type       string            `json:"type"`
			Subtype    string            `json:"subtype"`
			SessionID  string            `json:"session_id"`
			MCPServers []json.RawMessage `json:"mcp_servers"`
			Plugins    []json.RawMessage `json:"plugins"`
			IsError    *bool             `json:"is_error"`
		}
		if ward.RejectDuplicateJSONKeys(line) != nil || json.Unmarshal(line, &record) != nil {
			return false, false, fmt.Errorf("invalid Claude conformance transcript")
		}
		if record.Type == "system" && record.Subtype == "init" {
			severed = record.SessionID == sessionID && record.MCPServers != nil && len(record.MCPServers) == 0 &&
				record.Plugins != nil && len(record.Plugins) == 0
		}
		if record.Type == "result" {
			completed = record.SessionID == sessionID && record.Subtype == "success" && record.IsError != nil && !*record.IsError
		}
	}
	return severed, completed, scanner.Err()
}
