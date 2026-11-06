package ward

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Operator input files reach the writer read-only, outside the workspace, at
// InputFilesTarget/<name>. They are operator input, never evidence (plan
// §5.15): nothing here reads them back or exports them.
const (
	InputFilesTarget  = "/var/lib/freeside/inputs"
	inputVolumeTarget = "/inputs-volume"
	inputStageDir     = "/inputs-input"
	inputReadyDir     = "/inputs-ready"
	inputArchiveName  = "inputs.tar"
	// inputEntryOverheadBytes covers one file's block rounding and its share
	// of directory and inode space on the input volume.
	inputEntryOverheadBytes int64 = 8 << 10
)

// ErrInputBlobMismatch reports stored input bytes that differ from the
// admitted digest or size, so staging stops before any seeder runs.
var ErrInputBlobMismatch = errors.New("ward input bytes do not match the admitted manifest")

// InputFile is one operator file the writer reads at
// InputFilesTarget/<Name>: its admitted relative name, content address, and
// byte size. The media type stays with the stage: ward copies bytes and never
// interprets them.
type InputFile struct {
	Name      domain.TaskAttachmentName
	Digest    domain.Digest
	SizeBytes int64
}

// InputBlobSource opens an input's stored bytes by digest (production: the
// daemon's attachment blob store). Ward verifies what it reads against the
// manifest, so the source is trusted for availability, not for content.
type InputBlobSource interface {
	OpenContext(ctx context.Context, digest domain.Digest) (io.ReadCloser, error)
}

// validateInputFiles holds a manifest to the task-attachment list rules: each
// name valid, at most domain.MaxTaskAttachments files and
// domain.MaxTaskAttachmentsTotalBytes in total, and names that coexist on a
// case-sensitive filesystem. Ward has no media type, so every entry is checked
// against the larger per-file limit; the stage already held images to theirs.
func validateInputFiles(files []InputFile) error {
	list := make([]domain.TaskAttachment, len(files))
	for i, f := range files {
		list[i] = domain.TaskAttachment{
			Digest: f.Digest, Name: f.Name, SizeBytes: f.SizeBytes,
			MediaType:        domain.TaskAttachmentMediaOctetStream,
			ClaimedMediaType: string(domain.TaskAttachmentMediaOctetStream),
		}
	}
	if err := domain.ValidateTaskAttachments(list); err != nil {
		return fmt.Errorf("%w: input files: %w", ErrInvalidHandoffSpec, err)
	}
	return nil
}

// inputDirectories returns every directory the names imply, each once, in
// first-use order.
func inputDirectories(files []InputFile) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		name := string(f.Name)
		for end := range len(name) {
			if name[end] == '/' && !seen[name[:end]] {
				seen[name[:end]] = true
				dirs = append(dirs, name[:end])
			}
		}
	}
	return dirs
}

// inputListingDigest is the digest the input observer must report: SHA-256
// over the byte-sorted lines it prints, newline-terminated. A directory is
// "d <path>" and a file "f <sha256 hex> <path>", each path as find prints it
// from the volume root ("." or "./<name>"). Names hold no control character,
// so a line cannot be forged by another name.
func inputListingDigest(files []InputFile) string {
	lines := []string{"d ."}
	for _, dir := range inputDirectories(files) {
		lines = append(lines, "d ./"+dir)
	}
	for _, f := range files {
		lines = append(lines, "f "+contentaddr.Hex(string(f.Digest))+" ./"+string(f.Name))
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

// inputVolumeSizeMB leaves room for the bytes, per-file overhead, and the
// filesystem's own metadata and journal.
func inputVolumeSizeMB(files []InputFile) int64 {
	var total int64
	for _, f := range files {
		total += f.SizeBytes + inputEntryOverheadBytes
	}
	return 16 + (total*5/4+(1<<20)-1)>>20
}

// prepareInputs copies the manifest's files onto an owned volume and proves
// every file's bytes and path, and every entry's type, owner, and mode, before
// the writer can exist.
func (b *Backend) prepareInputs(ctx context.Context, hs HandoffSpec, names handoffNames, st *runState) error {
	files := hs.Agent.Inputs
	if len(files) == 0 {
		return nil
	}
	return b.prepareStagedVolume(ctx, hs, stagedVolume{
		label:  "inputs",
		volume: names.Inputs, volumeTarget: inputVolumeTarget, sizeMB: inputVolumeSizeMB(files),
		volumeClaim:   &st.inputs,
		seeder:        buildInputSeederSpec(b.cfg, handoffInputRoles(b.cfg, hs, names), st.ownershipLabel),
		seederClaim:   &st.inputsSeeder,
		observer:      buildInputObserverSpec(handoffInputRoles(b.cfg, hs, names), st.ownershipLabel),
		observerClaim: &st.inputsObserver,
		stageDir:      inputStageDir, readyDir: inputReadyDir,
		stage: func(dir string) error {
			return writeInputArchive(ctx, filepath.Join(dir, inputArchiveName), files, b.cfg.InputBlobs)
		},
		verify: func(proof []byte) error {
			return verifyInputProof(proof, st.ownershipLabel.Value, files)
		},
	}, st)
}

// writeInputArchive streams each blob into one tar archive, hashing as it
// goes. Names live only inside the archive, never as host paths, so the
// host's case-insensitive filesystem cannot merge "A.png" and "a.png". The
// GNU format carries long and non-ASCII names byte for byte.
func writeInputArchive(ctx context.Context, path string, files []InputFile, blobs InputBlobSource) (err error) {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // gate-owned temp path
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	tw := tar.NewWriter(out)
	for _, dir := range inputDirectories(files) {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755, Format: tar.FormatGNU,
		}); err != nil {
			return fmt.Errorf("archive input directory: %w", err)
		}
	}
	for i, f := range files {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: string(f.Name), Size: f.SizeBytes, Mode: 0o444, Format: tar.FormatGNU,
		}); err != nil {
			return fmt.Errorf("archive input %d: %w", i, err)
		}
		if err := copyInputBlob(ctx, tw, f, blobs); err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
	}
	return tw.Close()
}

// copyInputBlob writes exactly f.SizeBytes bytes of f's blob and fails closed
// when the blob is shorter, longer, or hashes to another digest.
func copyInputBlob(ctx context.Context, w io.Writer, f InputFile, blobs InputBlobSource) (err error) {
	body, err := blobs.OpenContext(ctx, f.Digest)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, body.Close()) }()
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(w, hash), body, f.SizeBytes); errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: blob is shorter than its admitted size", ErrInputBlobMismatch)
	} else if err != nil {
		return err
	}
	var extra [1]byte
	if n, err := body.Read(extra[:]); n != 0 {
		return fmt.Errorf("%w: blob is longer than its admitted size", ErrInputBlobMismatch)
	} else if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if contentaddr.Format(hash.Sum(nil)) != string(f.Digest) {
		return fmt.Errorf("%w: blob does not hash to its digest", ErrInputBlobMismatch)
	}
	return nil
}

// inputVolumeRoles names one input volume and the seeder and observer that
// fill and prove it, so the writer handoff and the review lifecycle run the
// same scripts and check the same proof.
type inputVolumeRoles struct {
	runID, volume, seeder, observer, image string
	size                                   ContainerSize
}

func handoffInputRoles(cfg Config, hs HandoffSpec, names handoffNames) inputVolumeRoles {
	return inputVolumeRoles{
		runID: hs.RunID, volume: names.Inputs, seeder: names.InputsSeeder, observer: names.InputsObserver,
		image: cfg.ExporterImage, size: hs.Size,
	}
}

// buildInputSeederSpec extracts the staged archive onto the fresh volume and
// leaves everything root-owned: directories 0755 and files 0444, readable but
// not writable by the agent's user. The observer, not this script's exit
// status, decides whether the result is accepted.
func buildInputSeederSpec(cfg Config, roles inputVolumeRoles, ownership Label) ContainerSpec {
	root := shellQuote(inputVolumeTarget)
	script := stateSeederScript(inputVolumeTarget, stateManifestEmpty) + "; n=0; while [ ! -f " + shellQuote(inputReadyDir+"/"+seedReadyFile) +
		" ]; do n=$((n+1)); [ \"$n\" -le " + strconv.Itoa(seederScriptTicks(cfg)) + " ] || exit 1; sleep 1; done; " +
		"tar -x -f " + shellQuote(inputStageDir+"/"+inputArchiveName) + " -C " + root + "; " +
		"chown -R 0:0 " + root + "; find " + root + " -type d -exec chmod 0755 {} +; " +
		"find " + root + " -type f -exec chmod 0444 {} +; sync"
	return ContainerSpec{
		Size: roles.size,
		Name: roles.seeder, Image: roles.image, Command: []string{"sh", "-c", script},
		NetworkDisabled: true,
		Mounts:          []Mount{{Type: MountVolume, Source: roles.volume, Target: inputVolumeTarget}},
		Labels:          append(runLabels(roles.runID), ownership),
	}
}

// inputObserverEntryScript prints one listing line per path argument: a
// root-owned 0755 directory, a root-owned 0444 regular file with its hash, or
// an "x" line no manifest produces for anything else (a link, a special
// file, another owner or mode). A failed hash prints nothing for its file.
const inputObserverEntryScript = `for p do case "$(stat -c "%F:%a:%u:%g" "$p")" in ` +
	`"directory:755:0:0") printf "d %s\n" "$p";; ` +
	`"regular file:444:0:0"|"regular empty file:444:0:0") h=$(sha256sum < "$p") && printf "f %s %s\n" "${h%% *}" "$p";; ` +
	`*) printf "x %s\n" "$p";; esac; done`

func inputObserverScript(nonce string) string {
	return "set -eu; LC_ALL=C; export LC_ALL; cd " + shellQuote(inputVolumeTarget) + "; " +
		"listing=$(find . -exec sh -c " + shellQuote(inputObserverEntryScript) + " sh {} + | sort | sha256sum); " +
		"printf '%s %s\\n' " + shellQuote(nonce) + " \"${listing%% *}\" > " + shellQuote(stateProofPath) + "; sync"
}

func buildInputObserverSpec(roles inputVolumeRoles, ownership Label) ContainerSpec {
	return ContainerSpec{
		Size: roles.size,
		Name: roles.observer, Image: roles.image, Command: []string{"sh", "-c", inputObserverScript(ownership.Value)},
		NetworkDisabled: true,
		Mounts:          []Mount{{Type: MountVolume, Source: roles.volume, Target: inputVolumeTarget, ReadOnly: true}},
		Labels:          append(runLabels(roles.runID), ownership),
	}
}

func verifyInputProof(proof []byte, nonce string, files []InputFile) error {
	if string(proof) != nonce+" "+inputListingDigest(files)+"\n" {
		return failf(CheckControlPlaneIsolation, "observed inputs do not match their admitted manifest")
	}
	return nil
}

func inputMounts(names handoffNames, present bool) []Mount {
	if !present {
		return nil
	}
	return []Mount{{Type: MountVolume, Source: names.Inputs, Target: InputFilesTarget, ReadOnly: true}}
}
