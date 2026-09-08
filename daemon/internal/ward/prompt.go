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

// preparePrompt uses the existing controlled rootfs-copy handshake. Runtime
// copy directly into a mounted volume does not reliably populate that volume.
func (b *Backend) preparePrompt(ctx context.Context, hs HandoffSpec, names handoffNames, st *runState) error {
	if hs.Agent.PromptFile == nil {
		return nil
	}
	if err := b.createStateVolume(ctx, hs.RunID, names.Prompt, 2, &st.prompt, st); err != nil {
		return err
	}
	snapshot, err := os.MkdirTemp("", "freeside-prompt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(snapshot) //nolint:errcheck // gate-owned transient input
	input, ready := filepath.Join(snapshot, "input"), filepath.Join(snapshot, "ready")
	for _, dir := range []string{input, ready} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(input, "prompt.txt"), hs.Agent.PromptFile.Body, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ready, seedReadyFile), []byte("ready\n"), 0o600); err != nil {
		return err
	}
	seeder := buildPromptSeederSpec(b.cfg, hs, names, st.ownershipLabel)
	if err := b.startPromptRole(ctx, seeder, names.Prompt, &st.promptSeeder, st); err != nil {
		return err
	}
	for _, copy := range []struct{ source, target string }{{input, promptStageDir}, {ready, promptReadyDir}} {
		copyCtx, cancel := context.WithTimeout(ctx, b.cfg.SeedTimeout)
		err := b.rt.CopyIntoContainer(copyCtx, seeder.Name, copy.source, copy.target)
		cancel()
		if err != nil {
			return failf(CheckControlPlaneIsolation, "copy prompt input: %v", err)
		}
	}
	if err := b.waitStopped(ctx, seeder.Name, st.promptSeeder, st.ownershipLabel, b.cfg.SeedTimeout); err != nil {
		return err
	}
	if err := b.removePromptRole(ctx, seeder.Name, &st.promptSeeder, st); err != nil {
		return err
	}
	observer := buildPromptObserverSpec(b.cfg, hs, names, st.ownershipLabel)
	if err := b.startPromptRole(ctx, observer, names.Prompt, &st.promptObserver, st); err != nil {
		return err
	}
	if err := b.waitStopped(ctx, observer.Name, st.promptObserver, st.ownershipLabel, b.cfg.SeedTimeout); err != nil {
		return err
	}
	// Reuse the bounded rootfs proof reader. The observer exports only its
	// nonce/digest proof; the prompt itself lives on the separate volume.
	proof, err := b.readStateProof(ctx, hs.RunID, observer.Name, st)
	if err != nil {
		return err
	}
	if err := verifyPromptProof(proof, st.ownershipLabel.Value, hs.Agent.PromptFile.Digest); err != nil {
		return err
	}
	return b.removePromptRole(ctx, observer.Name, &st.promptObserver, st)
}

func (b *Backend) startPromptRole(ctx context.Context, spec ContainerSpec, volume string, claim *objectClaim, st *runState) error {
	claim.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return failf(CheckControlPlaneIsolation, "create prompt role: %v", err)
	}
	claim.owned = true
	rep, err := b.rt.Inspect(ctx, spec.Name)
	if err != nil {
		return err
	}
	if err := verifySeedRoleAllowlist(rep, spec, volume, promptVolumeTarget, CheckControlPlaneIsolation); err != nil {
		return err
	}
	claim.fingerprint, err = ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, st.ownershipLabel)
	if err != nil {
		return err
	}
	return b.rt.StartContainer(ctx, spec.Name)
}

func (b *Backend) removePromptRole(ctx context.Context, name string, claim *objectClaim, st *runState) error {
	if err := b.rt.DeleteContainer(ctx, name); err != nil {
		return err
	}
	if err := b.verifyContainerAbsent(ctx, name, *claim, st.ownershipLabel, CheckControlPlaneIsolation); err != nil {
		return err
	}
	*claim = objectClaim{}
	return nil
}

func buildPromptSeederSpec(cfg Config, hs HandoffSpec, names handoffNames, ownership Label) ContainerSpec {
	root := shellQuote(promptVolumeTarget)
	script := stateSeederScript(promptVolumeTarget, stateManifestEmpty) + "; n=0; while [ ! -f " + shellQuote(promptReadyDir+"/"+seedReadyFile) +
		" ]; do n=$((n+1)); [ \"$n\" -le " + strconv.Itoa(seederScriptTicks(cfg)) + " ] || exit 1; sleep 1; done; " +
		"cp " + shellQuote(promptStageDir+"/prompt.txt") + " " + root + "/prompt.txt; " +
		"chown 0:0 " + root + " " + root + "/prompt.txt; chmod 0755 " + root + "; chmod 0400 " + root + "/prompt.txt; sync"
	return ContainerSpec{
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
		"[ \"$(find " + root + " -mindepth 1 -maxdepth 1 -printf x)\" = x ]; " +
		"[ \"$(stat -c '%a:%u:%g' " + root + ")\" = '755:0:0' ]; " +
		"[ \"$(stat -c '%a:%u:%g' " + file + ")\" = '400:0:0' ]; " +
		"digest=$(sha256sum " + file + "); digest=${digest%% *}; " +
		"printf '%s %s\\n' " + shellQuote(ownership.Value) + " \"$digest\" > " + shellQuote(stateProofPath) + "; sync"
	return ContainerSpec{
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
	return hs
}

// promptResourceNames is included only where the current invocation may use
// file delivery; historical conformance fixtures retain their old namespace.
func promptResourceNames(names handoffNames, resources RuntimeResourceNames) RuntimeResourceNames {
	resources.Containers = append(slices.Clone(resources.Containers), names.PromptSeeder, names.PromptObserver)
	resources.Volumes = append(slices.Clone(resources.Volumes), names.Prompt)
	return resources
}
