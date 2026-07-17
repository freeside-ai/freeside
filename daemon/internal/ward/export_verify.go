package ward

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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
// and its decoded manifest, released only after checks 5 and 7 passed.
type exportOutput struct {
	// Dir holds manifest.json and blobs/sha256/<hex>, extracted from the
	// exporter's rootfs archive.
	Dir string
	// Manifest is the decoded, validated §5.6 manifest.
	Manifest export.Manifest
}

// verifyExport runs checks 5 and 7 against the exported rootfs archive at
// tarPath: it extracts only the proof file and the handoff output into
// destDir (nothing else in the rootfs is trusted enough to touch the host
// filesystem), verifies the proof (check 5), then verifies the manifest,
// every blob digest, and the §5.4 scanner hook (check 7). Any deviation
// fails closed with the failing check.
func (b *Backend) verifyExport(ctx context.Context, tarPath, destDir string) (*exportOutput, error) {
	proof, err := b.extractHandoff(tarPath, destDir)
	if err != nil {
		return nil, err
	}
	if proof == nil {
		return nil, failf(CheckInExporterVerification,
			"exported rootfs has no proof file at %s", b.cfg.ProofPath)
	}
	if err := verifyProof(proof); err != nil {
		return nil, err
	}

	manifest, err := b.verifyManifest(destDir)
	if err != nil {
		return nil, err
	}
	if err := b.cfg.Scanner.Scan(ctx, destDir); err != nil {
		// The scanner's error may quote the matched secret; ConformanceFailure
		// reasons must never carry credential material, so the detail is
		// withheld here. A scanner that needs to record specifics logs them to
		// its own audited sink.
		return nil, failf(CheckExportVerification, "output scan refused the export (details withheld)")
	}
	return &exportOutput{Dir: destDir, Manifest: manifest}, nil
}

// extractHandoff streams the archive, extracting regular files under the
// handoff directory plus the proof file, and returns the proof bytes (nil
// if absent). Everything else in the rootfs is skipped; a traversal path,
// a non-regular entry inside the handoff output, or exceeding the extraction
// cap fails the export_verification check.
func (b *Backend) extractHandoff(tarPath, destDir string) ([]byte, error) {
	f, err := os.Open(tarPath) //nolint:gosec // gate-generated temp path, never external input
	if err != nil {
		return nil, failf(CheckExportVerification, "open exported archive: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	handoffRel := strings.TrimPrefix(b.cfg.HandoffDir, "/")
	proofRel := strings.TrimPrefix(b.cfg.ProofPath, "/")

	var proof []byte
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
			return nil, failf(CheckExportVerification, "read exported archive: %v", err)
		}
		if len(hdr.Name) > maxArchivePathBytes {
			return nil, failf(CheckExportVerification, "archive entry path exceeds the length cap")
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") {
			return nil, failf(CheckExportVerification, "archive entry %s escapes the archive root", redactPath(hdr.Name))
		}

		switch {
		case name == proofRel:
			if hdr.Typeflag != tar.TypeReg {
				return nil, failf(CheckExportVerification, "proof entry is not a regular file")
			}
			if hdr.Size > maxProofBytes {
				return nil, failf(CheckExportVerification, "proof entry is %d bytes, cap %d", hdr.Size, maxProofBytes)
			}
			proof, err = io.ReadAll(io.LimitReader(tr, maxProofBytes+1))
			if err != nil {
				return nil, failf(CheckExportVerification, "read proof entry: %v", err)
			}
			if int64(len(proof)) > maxProofBytes {
				return nil, failf(CheckExportVerification, "proof entry exceeds %d-byte cap", maxProofBytes)
			}
		case name == handoffRel || strings.HasPrefix(name, handoffRel+"/"):
			rel := strings.TrimPrefix(strings.TrimPrefix(name, handoffRel), "/")
			if rel == "" {
				if hdr.Typeflag != tar.TypeDir {
					return nil, failf(CheckExportVerification, "handoff root is not a directory")
				}
				continue
			}
			parent := path.Dir(rel)
			if parent == "." {
				parent = ""
			}
			if _, ok := seenDirs[parent]; !ok {
				return nil, failf(CheckExportVerification, "handoff output parent directory was not declared")
			}
			outputEntries++
			if outputEntries > b.cfg.MaxExportEntries {
				return nil, failf(CheckExportVerification, "handoff output exceeds the entry cap")
			}
			dest := filepath.Join(destDir, filepath.FromSlash(rel))
			switch hdr.Typeflag {
			case tar.TypeDir:
				if err := os.Mkdir(dest, 0o750); err != nil {
					// The os error embeds the destination path (which carries
					// the attacker-derived rel); report only the redacted name.
					return nil, failf(CheckExportVerification, "create output dir for entry %s failed", redactPath(name))
				}
				seenDirs[rel] = struct{}{}
			case tar.TypeReg:
				n, err := extractFile(tr, dest, b.cfg.MaxExportBytes-extracted)
				extracted += n
				if err != nil {
					// extractFile returns path-free category errors, safe to
					// include; the entry name is redacted.
					return nil, failf(CheckExportVerification, "extract entry %s: %v", redactPath(name), err)
				}
			default:
				// A symlink, hardlink, or device inside the handoff output
				// can redirect later reads or writes; the §5.6 output is
				// regular files in directories, full stop.
				return nil, failf(CheckExportVerification,
					"handoff output entry %s has non-regular type %q", redactPath(name), string(hdr.Typeflag))
			}
		}
	}
	return proof, nil
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

// verifyManifest decodes and validates the manifest, then proves the blob
// tree matches it exactly: every required blob present with the manifest's
// digest and size, and nothing in the output directory beyond the manifest
// and referenced blobs.
func (b *Backend) verifyManifest(destDir string) (export.Manifest, error) {
	var manifest export.Manifest
	raw, err := os.ReadFile(filepath.Join(destDir, export.ManifestFilename)) //nolint:gosec // gate-owned extraction dir
	if err != nil {
		return manifest, failf(CheckExportVerification, "read manifest: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		// A decode error can quote an unknown field name or value from the
		// manifest, which is workspace-derived; report the failure without it.
		return manifest, failf(CheckExportVerification, "manifest is not valid %s JSON", export.ManifestVersion)
	}
	// Decode stops at the first JSON value; trailing bytes would be released
	// in the output directory unchecked, and downstream consumes the bytes,
	// not this struct. Require the manifest file to be exactly one value.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return manifest, failf(CheckExportVerification, "manifest carries trailing bytes after the first JSON value")
	}
	if err := manifest.Validate(); err != nil {
		// A validation error names the offending entry path, which is
		// workspace-derived; report the failure without it.
		return manifest, failf(CheckExportVerification, "manifest failed %s validation", export.ManifestVersion)
	}

	referenced := make(map[string]bool)
	for _, e := range manifest.Entries {
		if e.Kind != export.EntryRegular || e.BlobOmitted {
			continue
		}
		hexDigest := strings.TrimPrefix(string(*e.Digest), "sha256:")
		rel := path.Join("blobs", "sha256", hexDigest)
		referenced[rel] = true
		if err := verifyBlob(filepath.Join(destDir, filepath.FromSlash(rel)), hexDigest, *e.Size); err != nil {
			return manifest, failf(CheckExportVerification, "blob %s: %v", hexDigest, err)
		}
	}

	if err := verifyNoStrays(destDir, referenced); err != nil {
		return manifest, err
	}
	return manifest, nil
}

// verifyBlob re-hashes one blob file against its manifest digest and size.
func verifyBlob(blobPath, wantHex string, wantSize int64) error {
	f, err := os.Open(blobPath) //nolint:gosec // path built from a validated manifest digest under the gate-owned dir
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != wantSize {
		return fmt.Errorf("size %d, manifest says %d", n, wantSize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantHex {
		return fmt.Errorf("content hashes to %s, manifest says %s", got, wantHex)
	}
	return nil
}

// verifyNoStrays walks the extracted output and rejects anything that is not
// the manifest or a referenced blob: an unreferenced file in the handoff
// output has no provenance and never continues downstream.
func verifyNoStrays(destDir string, referenced map[string]bool) error {
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
		if d.IsDir() {
			switch rel {
			case ".", "blobs", "blobs/sha256":
				return nil
			default:
				return failf(CheckExportVerification, "output carries unreferenced directory %s", redactPath(rel))
			}
		}
		if rel == export.ManifestFilename || referenced[rel] {
			return nil
		}
		// rel is an attacker-derived output filename; redact it in the reason.
		return failf(CheckExportVerification, "output carries unreferenced file %s", redactPath(rel))
	})
}
