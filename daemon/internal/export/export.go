package export

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Options configures one export. Both caps are pinned by the exporter
// image invocation, so they never vary between runs of the same workspace,
// and entries are processed in manifest (sorted) order, so which blobs a
// budget omits is deterministic. An omitted-but-needed blob is
// publish-blocking at the importer, the contained failure direction for a
// workspace built to exceed the caps. Zero or negative disables either cap.
//
// The blob caps bound bytes written to the exporter root filesystem, not
// bytes read: an over-cap file is still streamed once through the hash to
// record its digest (blobless regular entries still bind a digest), so
// total bytes read equals the workspace size. That is bounded, and with no
// network there is no amplification, but it is the workspace volume that
// bounds it, not these caps.
type Options struct {
	// MaxBlobBytes is the largest regular file that still gets a content
	// blob. Larger files keep their digest and size in the manifest but
	// set blob_omitted.
	MaxBlobBytes int64
	// MaxTotalBlobBytes bounds the bytes written under blobs/ across the
	// whole export, so many under-cap files cannot exhaust the exporter
	// root filesystem either. Charged by bytes actually written: a dedup
	// hit costs nothing and never sets blob_omitted.
	MaxTotalBlobBytes int64
	// MaxEntries fails the export closed (ErrTooManyEntries) when the walk
	// touches more names (files and directories alike) than this, a count
	// guard on the manifest and the in-memory entry slice: blobless
	// entries (empty files, empty directories, symlinks, invalid names)
	// evade both blob caps yet still consume an Entry each. Enforced
	// against batched directory reads, so one huge directory cannot
	// balloon memory before the cap fires. It bounds the entry count, not
	// bytes: peak memory also scales with total path-name length (and
	// doubles while Encode marshals), so the effective ceiling is
	// MaxEntries times the workspace's longest paths. That is finite but
	// not tight; the tight bound is the ward's bounded workspace volume
	// (§5.7), since every recorded name occupies a real on-disk dirent.
	MaxEntries int
	// MaxEvidenceBlobBytes caps one evidence-channel blob and
	// MaxEvidenceTotalBytes the aggregate evidence bytes. Unlike the repo caps
	// above, an over-cap evidence source fails the export closed rather than
	// omitting its blob: the evidence schema requires a blob for every entry
	// (evidence.go), so an omitted evidence blob is not representable. Zero or
	// negative disables either cap.
	MaxEvidenceBlobBytes  int64
	MaxEvidenceTotalBytes int64
}

// ManifestFilename is the manifest's fixed name under the output
// directory; blobs live beside it under blobs/sha256/.
const ManifestFilename = "manifest.json"

// Export walks the read-only workspace, writes digest-addressed blobs and
// the normalized manifest under outDir, and returns the manifest. It reads
// workspace content exactly once per regular file, never executes or
// follows anything, and fails loud if a file's observed bytes disagree
// with its recorded size (the workspace is quiescent by contract: the
// agent VM is gone before the exporter runs).
func Export(fsys fs.FS, outDir string, opts Options) (Manifest, error) {
	if err := ensureEmptyOutput(outDir); err != nil {
		return Manifest{}, err
	}
	entries, err := walkWorkspace(fsys, opts.MaxEntries)
	if err != nil {
		return Manifest{}, err
	}
	// Resolve and validate the agent-declared evidence channel BEFORE writing
	// any output, so a malformed declaration fails the whole export closed (no
	// manifest is written, so the ward gate, which verifies output not exit
	// status, fails the handoff) rather than degrading to a repo-only export.
	// The reserved subtree was skipped by the walk above.
	evidence, err := resolveEvidence(fsys, opts)
	if err != nil {
		return Manifest{}, err
	}
	bw, err := newBlobWriter(outDir, "blobs")
	if err != nil {
		return Manifest{}, err
	}
	var written int64
	for i, e := range entries {
		if e.Kind != EntryRegular {
			continue
		}
		store := blobAllowed(*e.Size, written, opts)
		res, err := bw.digestAndStore(fsys, e.Path, store)
		if err != nil {
			return Manifest{}, err
		}
		if res.size != *e.Size {
			return Manifest{}, fmt.Errorf("file %q: size %d became %d: %w", e.Path, *e.Size, res.size, ErrWorkspaceChanged)
		}
		written += res.bytesWritten
		entries[i].Digest = &res.digest
		entries[i].BlobOmitted = !res.stored
	}
	if entries == nil {
		entries = []Entry{}
	}
	m := Manifest{Version: ManifestVersion, Entries: entries}
	body, err := m.Encode()
	if err != nil {
		return Manifest{}, err
	}
	// Emit the pre-resolved evidence channel (evidence.json plus its evidence/
	// blobs) BEFORE writing manifest.json. manifest.json is the atomic commit
	// marker: the ward gate keys success on it, so writing it last means any
	// failure emitting the declared evidence — a malformed descriptor (already
	// caught by resolveEvidence above), or an infrastructure failure like ENOSPC
	// during the blob copy here — leaves no manifest, and the whole handoff fails
	// closed rather than silently degrading to a repo-only export that dropped
	// the agent's declared evidence.
	if err := writeEvidence(fsys, outDir, evidence); err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(outDir, ManifestFilename), body, 0o600); err != nil {
		return Manifest{}, fmt.Errorf("write manifest: %w", err)
	}
	return m, nil
}

// blobAllowed reports whether a regular file of the given size still gets
// a content blob, with written bytes already spent from the aggregate
// budget. The budget check compares against the remaining headroom rather
// than summing, because written+size can wrap negative for a hostile size
// near MaxInt64 and slip past the cap; written never exceeds the budget,
// so the subtraction cannot overflow.
func blobAllowed(size, written int64, opts Options) bool {
	if opts.MaxBlobBytes > 0 && size > opts.MaxBlobBytes {
		return false
	}
	if opts.MaxTotalBlobBytes > 0 && size > opts.MaxTotalBlobBytes-written {
		return false
	}
	return true
}

// ensureEmptyOutput requires outDir to be absent (it is created) or an
// empty directory. A dirty output (a retried export's leftovers, a stale
// path baked into the exporter rootfs) could otherwise sit beside this
// export's blobs and masquerade as its output, so the export fails closed
// instead; together with the blob writer trusting only what it wrote, the
// collected output holds exactly what this helper emitted: the repo channel
// (manifest.json plus its blobs) and, when the workspace declares one, the
// evidence channel (evidence.json plus its evidence/ blobs, emitted by
// emitEvidence from the reserved .freeside-evidence/ descriptor).
func ensureEmptyOutput(outDir string) error {
	existing, err := os.ReadDir(outDir)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(outDir, 0o750); err != nil {
				return fmt.Errorf("create output directory: %w", err)
			}
			return nil
		}
		return fmt.Errorf("inspect output directory: %w", err)
	}
	if len(existing) > 0 {
		return fmt.Errorf("output directory %q holds %d entries: %w", outDir, len(existing), ErrOutputNotEmpty)
	}
	return nil
}
