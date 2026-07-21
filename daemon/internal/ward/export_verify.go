package ward

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// maxProofBytes bounds the proof file; the real one is four short lines.
const maxProofBytes = 64 << 10

// maxArchivePathBytes rejects pathological tar names before path cleaning or
// host filesystem calls. It matches the common PATH_MAX ceiling while the
// configurable entry budget separately bounds created output objects.
const maxArchivePathBytes = 4 << 10

// redactPath returns a stable, non-reversible token for an archive-derived
// path. Exported paths are workspace filenames (attacker-influenced) that
// could themselves embed a credential, and ConformanceFailure reasons must
// never carry credential material; the token distinguishes failures and lets
// an operator confirm a suspected name by recomputing it, without the reason
// revealing the name.
func redactPath(p string) string {
	sum := sha256.Sum256([]byte(p))
	return "path:" + hex.EncodeToString(sum[:])[:16]
}

// exportOutput is a verified handoff export: the extracted output directory
// and its decoded manifests, released only after check 7 passed.
type exportOutput struct {
	// Dir holds manifest.json and blobs/sha256/<hex>, plus evidence.json and
	// evidence/sha256/<hex> when the workspace declared an evidence channel,
	// extracted from the exporter's rootfs archive.
	Dir string
	// Manifest is the decoded, digest-verified §5.6 repo-change manifest.
	Manifest export.Manifest
	// Evidence is the decoded, digest-verified evidence manifest; valid only
	// when EvidencePresent is true (an absent evidence channel is the
	// pre-evidence shape, not an error).
	Evidence          export.EvidenceManifest
	EvidencePresent   bool
	CommitPlanPresent bool
}

// verifyExport runs check 7 against the exported rootfs archive at tarPath: it
// extracts the handoff output into destDir (nothing else in the rootfs is
// trusted enough to touch the host filesystem), verifies both §5.6 channels
// (the repo manifest and blobs, and the evidence manifest and blobs when
// present), proves no unreferenced strays, and runs the §5.4 scanner hook over
// the whole output. Any deviation fails closed with the failing check. Check 5
// (the in-exporter proof) is no longer part of the per-handoff path: the
// exporter now runs only the trusted helper, which emits the channels but not
// the environment proof; check 4's inspect-before-execute covers the mount
// topology on every handoff, and check 5 is attested at conformance time by a
// dedicated probe (Suite.Full), symmetric with the network-free proof.
func (b *Backend) verifyExport(ctx context.Context, tarPath, destDir string) (*exportOutput, error) {
	if err := b.extractHandoff(tarPath, destDir); err != nil {
		return nil, err
	}
	manifest, repoRef, err := b.verifyManifest(destDir)
	if err != nil {
		return nil, err
	}
	evidence, evidencePresent, evidenceRef, err := b.verifyEvidence(destDir)
	if err != nil {
		return nil, err
	}
	planPresent, err := b.verifyCommitPlan(destDir)
	if err != nil {
		return nil, err
	}
	if err := verifyNoStrays(destDir, repoRef, evidenceRef, evidencePresent, planPresent); err != nil {
		return nil, err
	}
	if err := b.cfg.Scanner.Scan(ctx, destDir); err != nil {
		// The scanner's error may quote the matched secret; ConformanceFailure
		// reasons must never carry credential material, so the detail is
		// withheld here. A scanner that needs to record specifics logs them to
		// its own audited sink.
		return nil, failf(CheckExportVerification, "output scan refused the export (details withheld)")
	}
	return &exportOutput{Dir: destDir, Manifest: manifest, Evidence: evidence, EvidencePresent: evidencePresent, CommitPlanPresent: planPresent}, nil
}

func (b *Backend) verifyCommitPlan(destDir string) (bool, error) {
	f, err := os.Open(filepath.Join(destDir, export.CommitPlanFilename)) //nolint:gosec // gate-owned extraction dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, failf(CheckExportVerification, "read commit plan")
	}
	defer f.Close() //nolint:errcheck // read-only handle
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, failf(CheckExportVerification, "commit plan is not a regular file")
	}
	if info.Size() > b.cfg.MaxManifestBytes {
		return false, failf(CheckExportVerification, "commit plan exceeds the byte cap")
	}
	return true, nil
}

// extractHandoff streams the archive, extracting regular files under the
// handoff directory into destDir. Everything else in the rootfs (including a
// stray /handoff-proof.txt a prior payload might have written, now outside the
// per-handoff path) is skipped; a traversal path, a non-regular entry inside
// the handoff output, or exceeding the extraction cap fails the
// export_verification check.
func (b *Backend) extractHandoff(tarPath, destDir string) error {
	f, err := os.Open(tarPath) //nolint:gosec // gate-generated temp path, never external input
	if err != nil {
		return failf(CheckExportVerification, "open exported archive: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	handoffRel := strings.TrimPrefix(b.cfg.HandoffDir, "/")

	var extracted int64
	var outputEntries int
	// destDir itself is the already-created handoff root. Requiring every
	// nested parent to appear first as a directory header makes one accepted
	// tar header create at most one host filesystem object, so the entry cap
	// is an inode cap too rather than something MkdirAll can bypass.
	seenDirs := map[string]struct{}{"": {}}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return failf(CheckExportVerification, "read exported archive: %v", err)
		}
		if len(hdr.Name) > maxArchivePathBytes {
			return failf(CheckExportVerification, "archive entry path exceeds the length cap")
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") {
			return failf(CheckExportVerification, "archive entry %s escapes the archive root", redactPath(hdr.Name))
		}
		// Only the handoff output crosses to the host filesystem; every other
		// rootfs entry (OS files, a stray /handoff-proof.txt) is skipped.
		if name != handoffRel && !strings.HasPrefix(name, handoffRel+"/") {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(name, handoffRel), "/")
		if rel == "" {
			if hdr.Typeflag != tar.TypeDir {
				return failf(CheckExportVerification, "handoff root is not a directory")
			}
			continue
		}
		parent := path.Dir(rel)
		if parent == "." {
			parent = ""
		}
		if _, ok := seenDirs[parent]; !ok {
			return failf(CheckExportVerification, "handoff output parent directory was not declared")
		}
		outputEntries++
		if outputEntries > b.cfg.MaxExportEntries {
			return failf(CheckExportVerification, "handoff output exceeds the entry cap")
		}
		dest := filepath.Join(destDir, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(dest, 0o750); err != nil {
				// The os error embeds the destination path (which carries
				// the attacker-derived rel); report only the redacted name.
				return failf(CheckExportVerification, "create output dir for entry %s failed", redactPath(name))
			}
			seenDirs[rel] = struct{}{}
		case tar.TypeReg:
			n, err := extractFile(tr, dest, b.cfg.MaxExportBytes-extracted)
			extracted += n
			if err != nil {
				// extractFile returns path-free category errors, safe to
				// include; the entry name is redacted.
				return failf(CheckExportVerification, "extract entry %s: %v", redactPath(name), err)
			}
		default:
			// A symlink, hardlink, or device inside the handoff output
			// can redirect later reads or writes; the §5.6 output is
			// regular files in directories, full stop.
			return failf(CheckExportVerification,
				"handoff output entry %s has non-regular type %q", redactPath(name), string(hdr.Typeflag))
		}
	}
	return nil
}

// extractFile writes one archive entry to dest, returning the bytes written
// and failing once the remaining extraction budget is exhausted. Its errors
// are path-free categories: the os errors it would otherwise wrap embed dest
// (which carries the attacker-derived entry name), so the caller redacts the
// name and this reports only what went wrong, never where.
func extractFile(r io.Reader, dest string, budget int64) (int64, error) {
	if budget < 0 {
		return 0, errors.New("extraction cap exhausted")
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dest is joined under the gate-owned destDir
	if err != nil {
		return 0, errors.New("create destination file failed")
	}
	n, err := io.Copy(f, io.LimitReader(r, budget))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, errors.New("write failed")
	}
	if n == budget {
		// LimitReader reports equality for both an exact fit and truncation.
		// Probe one byte from the entry without writing it: EOF proves the file
		// exactly filled the remaining budget; a byte proves overflow.
		var probe [1]byte
		if m, perr := io.ReadFull(r, probe[:]); m > 0 {
			return n, errors.New("extraction cap exhausted")
		} else if perr != nil && !errors.Is(perr, io.EOF) {
			return n, errors.New("read failed")
		}
	}
	return n, nil
}

// verifyManifest decodes and validates the repo-change manifest, then proves
// every referenced blob is present with the manifest's digest and size,
// returning the set of referenced blob paths (blobs/sha256/<hex>) for the
// caller's combined stray check.
func (b *Backend) verifyManifest(destDir string) (export.Manifest, map[string]bool, error) {
	var manifest export.Manifest
	mf, err := os.Open(filepath.Join(destDir, export.ManifestFilename)) //nolint:gosec // gate-owned extraction dir
	if err != nil {
		return manifest, nil, failf(CheckExportVerification, "read manifest: %v", err)
	}
	defer mf.Close() //nolint:errcheck // read-only handle
	// A hostile manifest can fill the whole per-file extraction budget, so
	// bound the heap read here rather than pulling the entire file in before
	// validation can reject it. The +1 distinguishes an at-cap file from an
	// over-cap one.
	raw, err := io.ReadAll(io.LimitReader(mf, b.cfg.MaxManifestBytes+1))
	if err != nil {
		return manifest, nil, failf(CheckExportVerification, "read manifest: %v", err)
	}
	if int64(len(raw)) > b.cfg.MaxManifestBytes {
		return manifest, nil, failf(CheckExportVerification, "manifest exceeds the %d-byte cap", b.cfg.MaxManifestBytes)
	}
	// The raw manifest bytes are the artifact released to the gauntlet, but
	// encoding/json is last-value-wins on duplicate members, so a hostile
	// manifest with a duplicate key (two `path` or `digest` entries) would
	// validate as its collapsed struct while the released bytes stay
	// contradictory. Reject non-canonical duplicate keys before decoding, the
	// same structural gate the runtime decoders apply, so the validated view
	// and the released bytes cannot disagree.
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return manifest, nil, failf(CheckExportVerification, "manifest is not canonical %s JSON", export.ManifestVersion)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		// A decode error can quote an unknown field name or value from the
		// manifest, which is workspace-derived; report the failure without it.
		return manifest, nil, failf(CheckExportVerification, "manifest is not valid %s JSON", export.ManifestVersion)
	}
	// Decode stops at the first JSON value; trailing bytes would be released
	// in the output directory unchecked, and downstream consumes the bytes,
	// not this struct. Require the manifest file to be exactly one value.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return manifest, nil, failf(CheckExportVerification, "manifest carries trailing bytes after the first JSON value")
	}
	if err := manifest.Validate(); err != nil {
		// A validation error names the offending entry path, which is
		// workspace-derived; report the failure without it.
		return manifest, nil, failf(CheckExportVerification, "manifest failed %s validation", export.ManifestVersion)
	}

	referenced := make(map[string]bool)
	// Distinct paths may share a digest (identical files), and each blob is
	// re-hashed here; without dedup a small manifest of many entries pointing
	// at one large blob forces a full-file read per entry. Key the dedup on
	// (digest, size), not digest alone: a hostile manifest can claim two sizes
	// for one digest, and a per-digest skip would leave the second, lying size
	// unverified. A wrong size fails verifyBlob immediately, so only entries
	// that all agree on (digest, size) collapse to a single read.
	verified := make(map[string]bool)
	for _, e := range manifest.Entries {
		if e.Kind != export.EntryRegular || e.BlobOmitted {
			continue
		}
		hexDigest := strings.TrimPrefix(string(*e.Digest), "sha256:")
		rel := path.Join("blobs", "sha256", hexDigest)
		referenced[rel] = true
		key := hexDigest + ":" + strconv.FormatInt(*e.Size, 10)
		if verified[key] {
			continue
		}
		verified[key] = true
		if err := verifyBlob(filepath.Join(destDir, filepath.FromSlash(rel)), hexDigest, *e.Size); err != nil {
			// hexDigest is manifest-derived (attacker-influenced) and a regular
			// entry's digest field could encode a credential as 64 hex chars;
			// redact it to a stable token and rely on verifyBlob's value-free
			// category error, so a refused export cannot echo the raw digest.
			return manifest, nil, failf(CheckExportVerification, "blob %s failed verification: %v", redactPath(hexDigest), err)
		}
	}
	return manifest, referenced, nil
}

// verifyEvidence decodes and digest-verifies the evidence channel when the
// exporter emitted one. An absent evidence.json is the pre-evidence shape (the
// workspace declared no evidence), returning present=false with no error; the
// caller's stray check then forbids any evidence.json or evidence/ leftover.
// When present, the manifest is decoded through the canonical-or-reject boundary
// (which subsumes the repo channel's duplicate-key gate) and every entry's blob
// under evidence/sha256/ is re-hashed. It returns the referenced evidence blob
// paths for the combined stray check.
func (b *Backend) verifyEvidence(destDir string) (export.EvidenceManifest, bool, map[string]bool, error) {
	var em export.EvidenceManifest
	ef, err := os.Open(filepath.Join(destDir, export.EvidenceFilename)) //nolint:gosec // gate-owned extraction dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return em, false, nil, nil
		}
		return em, false, nil, failf(CheckExportVerification, "read evidence manifest: %v", err)
	}
	defer ef.Close() //nolint:errcheck // read-only handle
	raw, err := io.ReadAll(io.LimitReader(ef, b.cfg.MaxManifestBytes+1))
	if err != nil {
		return em, false, nil, failf(CheckExportVerification, "read evidence manifest: %v", err)
	}
	if int64(len(raw)) > b.cfg.MaxManifestBytes {
		return em, false, nil, failf(CheckExportVerification, "evidence manifest exceeds the %d-byte cap", b.cfg.MaxManifestBytes)
	}
	em, err = export.DecodeEvidenceManifest(raw)
	if err != nil {
		// A decode error can quote workspace-derived label/field bytes; report
		// the failure without them.
		return export.EvidenceManifest{}, false, nil, failf(CheckExportVerification, "evidence manifest is not valid %s", export.EvidenceManifestVersion)
	}

	referenced := make(map[string]bool)
	verified := make(map[string]bool)
	for _, e := range em.Entries {
		// Every evidence entry has a mandatory blob (the schema cannot omit one).
		hexDigest := strings.TrimPrefix(string(e.Digest), "sha256:")
		rel := path.Join(export.EvidenceBlobsDirname, "sha256", hexDigest)
		referenced[rel] = true
		key := hexDigest + ":" + strconv.FormatInt(e.Size, 10)
		if verified[key] {
			continue
		}
		verified[key] = true
		if err := verifyBlob(filepath.Join(destDir, filepath.FromSlash(rel)), hexDigest, e.Size); err != nil {
			return export.EvidenceManifest{}, false, nil, failf(CheckExportVerification, "evidence blob %s failed verification: %v", redactPath(hexDigest), err)
		}
	}
	return em, true, referenced, nil
}

// verifyBlob re-hashes one blob file against its manifest digest and size. Its
// errors are value-free categories: the blob path embeds the manifest digest
// and both the wanted digest and size are manifest-derived
// (attacker-influenced), so none is formatted into a reason the caller reports;
// the caller redacts the blob identifier.
func verifyBlob(blobPath, wantHex string, wantSize int64) error {
	f, err := os.Open(blobPath) //nolint:gosec // path built from a validated manifest digest under the gate-owned dir
	if err != nil {
		// The os error embeds blobPath, which carries the manifest digest.
		return errors.New("missing or unreadable")
	}
	defer f.Close() //nolint:errcheck // read-only handle
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return errors.New("read failed")
	}
	if n != wantSize {
		return errors.New("size does not match the manifest")
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantHex {
		return errors.New("content does not match the manifest digest")
	}
	return nil
}

// verifyNoStrays walks the extracted output and rejects anything that is not
// the manifest or a referenced blob: an unreferenced file in the handoff
// output has no provenance and never continues downstream. It covers both §5.6
// channels: the repo channel (manifest.json, blobs/) always, and the evidence
// channel (evidence.json, evidence/) only when the exporter emitted one, so a
// stale or planted evidence.json or evidence/ blob with no evidence channel is
// an orphan and fails closed (matching the importer's own layout audit).
func verifyNoStrays(destDir string, repoRef, evidenceRef map[string]bool, evidencePresent, planPresent bool) error {
	allowedDirs := map[string]bool{"": true, "blobs": true, "blobs/sha256": true}
	if evidencePresent {
		allowedDirs[export.EvidenceBlobsDirname] = true
		allowedDirs[export.EvidenceBlobsDirname+"/sha256"] = true
	}
	return filepath.WalkDir(destDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A walk error embeds the path (attacker-derived); report generically.
			return failf(CheckExportVerification, "walk output failed")
		}
		rel, err := filepath.Rel(destDir, p)
		if err != nil {
			return failf(CheckExportVerification, "walk output failed")
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if allowedDirs[rel] {
				return nil
			}
			return failf(CheckExportVerification, "output carries unreferenced directory %s", redactPath(rel))
		}
		if rel == export.ManifestFilename || repoRef[rel] {
			return nil
		}
		if evidencePresent && (rel == export.EvidenceFilename || evidenceRef[rel]) {
			return nil
		}
		if planPresent && rel == export.CommitPlanFilename {
			return nil
		}
		// rel is an attacker-derived output filename; redact it in the reason.
		return failf(CheckExportVerification, "output carries unreferenced file %s", redactPath(rel))
	})
}
