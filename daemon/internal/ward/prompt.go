package ward

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	// MaxPromptFileBytes bounds the new transport independently of shell argv.
	MaxPromptFileBytes = 1 << 20
	PromptFileTarget   = "/var/lib/freeside/prompt"
	PromptFilePath     = PromptFileTarget + "/prompt.txt"
	promptVolumeTarget = "/prompt-volume"
	promptStageDir     = "/prompt-input"
	promptReadyDir     = "/prompt-ready"
)

// PromptFile is immutable user input, never a vendor instruction bundle.
// Ward observes these exact bytes before the writer can consume them on stdin.
type PromptFile struct {
	Digest domain.Digest
	Body   []byte
}

func NewPromptFile(body []byte) *PromptFile {
	sum := sha256.Sum256(body)
	return &PromptFile{Digest: domain.Digest(contentaddr.Format(sum[:])), Body: bytes.Clone(body)}
}

func (p *PromptFile) Clone() *PromptFile {
	if p == nil {
		return nil
	}
	return &PromptFile{Digest: p.Digest, Body: bytes.Clone(p.Body)}
}

func (p *PromptFile) Validate() error {
	if p == nil || len(p.Body) == 0 || len(p.Body) > MaxPromptFileBytes || !utf8.Valid(p.Body) {
		return fmt.Errorf("%w: prompt file must contain bounded UTF-8 input", ErrInvalidHandoffSpec)
	}
	sum := sha256.Sum256(p.Body)
	if string(p.Digest) != contentaddr.Format(sum[:]) {
		return fmt.Errorf("%w: prompt file digest differs from its bytes", ErrInvalidHandoffSpec)
	}
	return nil
}

// preparePrompt binds the prompt bytes to an owned read-only volume through
// the staged-volume handshake before the writer can consume them on stdin.
func (b *Backend) preparePrompt(ctx context.Context, hs HandoffSpec, names handoffNames, st *runState) error {
	if hs.Agent.PromptFile == nil {
		return nil
	}
	return b.prepareStagedVolume(ctx, hs, stagedVolume{
		label:  "prompt",
		volume: names.Prompt, volumeTarget: promptVolumeTarget, sizeMB: 2, volumeClaim: &st.prompt,
		seeder:        buildPromptSeederSpec(b.cfg, hs, names, st.ownershipLabel),
		seederClaim:   &st.promptSeeder,
		observer:      buildPromptObserverSpec(b.cfg, hs, names, st.ownershipLabel),
		observerClaim: &st.promptObserver,
		stageDir:      promptStageDir, readyDir: promptReadyDir,
		stage: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "prompt.txt"), hs.Agent.PromptFile.Body, 0o600)
		},
		verify: func(proof []byte) error {
			return verifyPromptProof(proof, st.ownershipLabel.Value, hs.Agent.PromptFile.Digest)
		},
	}, st)
}

func buildPromptSeederSpec(cfg Config, hs HandoffSpec, names handoffNames, ownership Label) ContainerSpec {
	root := shellQuote(promptVolumeTarget)
	script := stateSeederScript(promptVolumeTarget, stateManifestEmpty) + "; n=0; while [ ! -f " + shellQuote(promptReadyDir+"/"+seedReadyFile) +
		" ]; do n=$((n+1)); [ \"$n\" -le " + strconv.Itoa(seederScriptTicks(cfg)) + " ] || exit 1; sleep 1; done; " +
		"cp " + shellQuote(promptStageDir+"/prompt.txt") + " " + root + "/prompt.txt; " +
		"chown 0:0 " + root + " " + root + "/prompt.txt; chmod 0755 " + root + "; chmod 0400 " + root + "/prompt.txt; sync"
	return ContainerSpec{
		Size: hs.Size,
		Name: names.PromptSeeder, Image: cfg.ExporterImage, Command: []string{"sh", "-c", script},
		NetworkDisabled: true,
		Mounts:          []Mount{{Type: MountVolume, Source: names.Prompt, Target: promptVolumeTarget}},
		Labels:          append(runLabels(hs.RunID), ownership),
	}
}

func buildPromptObserverSpec(cfg Config, hs HandoffSpec, names handoffNames, ownership Label) ContainerSpec {
	root, file := shellQuote(promptVolumeTarget), shellQuote(promptVolumeTarget+"/prompt.txt")
	// Prove a single regular file, its root-owned modes and actual byte hash.
	// A failed check emits no accepted proof, including failed sha256sum.
	script := "set -eu; LC_ALL=C; export LC_ALL; [ -f " + file + " ] && [ ! -L " + file + " ]; " +
		"[ \"$(find " + root + " -mindepth 1 -maxdepth 1 -exec sh -c 'printf x' \\;)\" = x ]; " +
		"[ \"$(stat -c '%a:%u:%g' " + root + ")\" = '755:0:0' ]; " +
		"[ \"$(stat -c '%a:%u:%g' " + file + ")\" = '400:0:0' ]; " +
		"digest=$(sha256sum " + file + "); digest=${digest%% *}; " +
		"printf '%s %s\\n' " + shellQuote(ownership.Value) + " \"$digest\" > " + shellQuote(stateProofPath) + "; sync"
	return ContainerSpec{
		Size: hs.Size,
		Name: names.PromptObserver, Image: cfg.ExporterImage, Command: []string{"sh", "-c", script},
		NetworkDisabled: true,
		Mounts:          []Mount{{Type: MountVolume, Source: names.Prompt, Target: promptVolumeTarget, ReadOnly: true}},
		Labels:          append(runLabels(hs.RunID), ownership),
	}
}

func verifyPromptProof(proof []byte, nonce string, digest domain.Digest) error {
	want := nonce + " " + contentaddr.Hex(string(digest)) + "\n"
	if !bytes.Equal(proof, []byte(want)) {
		return failf(CheckControlPlaneIsolation, "observed prompt does not match its immutable input")
	}
	return nil
}

func promptMounts(names handoffNames, present bool) []Mount {
	if !present {
		return nil
	}
	return []Mount{{Type: MountVolume, Source: names.Prompt, Target: PromptFileTarget, ReadOnly: true}}
}

func clonePromptSpec(hs HandoffSpec) HandoffSpec {
	hs.Agent.PromptFile = hs.Agent.PromptFile.Clone()
	if hs.Agent.FailureTranscript != nil {
		copy := *hs.Agent.FailureTranscript
		hs.Agent.FailureTranscript = &copy
	}
	return hs
}

// promptResourceNames is included only where the current invocation may use
// file delivery; historical conformance fixtures retain their old namespace.
func promptResourceNames(names handoffNames, resources RuntimeResourceNames) RuntimeResourceNames {
	resources.Containers = append(slices.Clone(resources.Containers), names.PromptSeeder, names.PromptObserver)
	resources.Volumes = append(slices.Clone(resources.Volumes), names.Prompt)
	return resources
}
