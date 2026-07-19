package export

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxEvidenceDescriptorBytes bounds the descriptor read: it is a small list of
// source declarations, so a larger file is hostile and must not be slurped into
// memory before the parser can reject it.
const maxEvidenceDescriptorBytes = 1 << 20

// errReservedAbsent signals that the reserved evidence subtree, or a component
// of a path under it, is absent (fs.ErrNotExist at some level). It distinguishes
// "the workspace declared no evidence" (a missing descriptor, benign) from a
// hostile or broken declaration.
var errReservedAbsent = errors.New("reserved evidence path is absent")

// emitEvidence writes the evidence channel (evidence.json plus its evidence/
// blobs) from the agent-declared descriptor under EvidenceWorkspaceDir. A
// missing descriptor means the workspace declared no evidence, and nothing is
// written (the importer treats an absent evidence.json as the pre-evidence
// shape). Any present-but-malformed declaration, or any source that is missing,
// non-regular, reached through a symlink, or over-cap, fails the whole export
// closed: the evidence schema has no way to omit a blob, so a partial or lying
// evidence channel is never emitted.
func emitEvidence(fsys fs.FS, outDir string, opts Options) error {
	info, err := lstatUnderReserved(fsys, EvidenceDescriptorPath)
	if err != nil {
		if errors.Is(err, errReservedAbsent) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("evidence descriptor %q: %w", EvidenceDescriptorPath, ErrEvidenceSourceNotRegular)
	}

	raw, err := readReservedFile(fsys, EvidenceDescriptorPath, maxEvidenceDescriptorBytes)
	if err != nil {
		return err
	}
	desc, err := DecodeEvidenceSourceManifest(raw)
	if err != nil {
		return err
	}

	bw, err := newBlobWriter(outDir, EvidenceBlobsDirname)
	if err != nil {
		return err
	}
	var written int64
	entries := make([]EvidenceEntry, 0, len(desc.Sources))
	for _, s := range desc.Sources {
		finfo, err := lstatUnderReserved(fsys, s.Path)
		if err != nil {
			if errors.Is(err, errReservedAbsent) {
				return fmt.Errorf("evidence source %q path %q: %w", s.Label, s.Path, ErrEvidenceSourceMissing)
			}
			return err
		}
		if !finfo.Mode().IsRegular() {
			return fmt.Errorf("evidence source %q path %q: %w", s.Label, s.Path, ErrEvidenceSourceNotRegular)
		}
		size := finfo.Size()
		if opts.MaxEvidenceBlobBytes > 0 && size > opts.MaxEvidenceBlobBytes {
			return fmt.Errorf("evidence source %q: %d bytes: %w", s.Label, size, ErrEvidenceBlobTooLarge)
		}
		// Compare against the remaining headroom, never written+size, so a
		// hostile size near MaxInt64 cannot wrap past the cap (written never
		// exceeds the budget, so the subtraction cannot overflow).
		if opts.MaxEvidenceTotalBytes > 0 && size > opts.MaxEvidenceTotalBytes-written {
			return fmt.Errorf("evidence source %q: %w", s.Label, ErrEvidenceBudgetExhausted)
		}
		res, err := bw.digestAndStore(fsys, s.Path, true)
		if err != nil {
			return err
		}
		if res.size != size {
			return fmt.Errorf("evidence source %q: size %d became %d: %w", s.Label, size, res.size, ErrWorkspaceChanged)
		}
		written += res.bytesWritten
		entries = append(entries, EvidenceEntry{
			Label:      s.Label,
			MediaType:  s.MediaType,
			Size:       res.size,
			Digest:     res.digest,
			Provenance: s.provenance(),
		})
	}

	// The descriptor need not be sorted; the wire manifest must be (Encode's
	// Validate requires strictly ascending unique labels, already guaranteed
	// unique by the descriptor's own validation).
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare([]byte(entries[i].Label), []byte(entries[j].Label)) < 0
	})
	m := EvidenceManifest{Version: EvidenceManifestVersion, Entries: entries}
	body, err := m.Encode()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, EvidenceFilename), body, 0o600); err != nil {
		return fmt.Errorf("write evidence manifest: %w", err)
	}
	return nil
}

// lstatUnderReserved resolves p (already validated canonical and under
// EvidenceWorkspaceDir) one component at a time, requiring every intermediate
// component to be a real directory, never a symlink, so an intermediate symlink
// cannot redirect the resolution outside the workspace. It returns the final
// component's FileInfo without following it if it is itself a symlink. A missing
// component returns errReservedAbsent. This is the symlink-safe counterpart to
// the walk, which likewise never follows a link (walk.go).
func lstatUnderReserved(fsys fs.FS, p string) (fs.FileInfo, error) {
	parts := strings.Split(p, "/")
	prefix := ""
	for i, part := range parts {
		if prefix == "" {
			prefix = part
		} else {
			prefix += "/" + part
		}
		info, err := fs.Lstat(fsys, prefix)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, errReservedAbsent
			}
			return nil, fmt.Errorf("lstat %q: %w", prefix, err)
		}
		if i == len(parts)-1 {
			return info, nil
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsDir() {
			// An intermediate component that is a symlink or non-directory can
			// only redirect the resolution; treat the whole path as
			// non-regular rather than following it.
			return nil, fmt.Errorf("evidence path %q: component %q is not a directory: %w", p, prefix, ErrEvidenceSourceNotRegular)
		}
	}
	return nil, fmt.Errorf("evidence path %q: %w", p, ErrEvidenceSourceMissing)
}

// readReservedFile reads a regular file already resolved under the reserved
// subtree, bounded by limit so a hostile file cannot exhaust memory. The +1
// distinguishes an at-cap file from an over-cap one.
func readReservedFile(fsys fs.FS, p string, limit int64) ([]byte, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", p, err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", p, err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("evidence descriptor %q: %w", p, ErrEvidenceDescriptorTooLarge)
	}
	return raw, nil
}
