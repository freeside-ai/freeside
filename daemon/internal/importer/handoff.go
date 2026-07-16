package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// loadManifest reads and re-validates the handoff manifest. The read is
// capped before any byte is parsed; the decode is strict (an unknown
// field or trailing content is hostile until a coordinated v1 widening
// lands in-tree, since the importer is the format's only consumer); and
// the export package's own Validate re-runs at this boundary, so
// nothing downstream ever sees a manifest the producer's rules would
// reject (the trust-boundary re-gate convention).
func loadManifest(handoffDir string, pol Policy) (export.Manifest, error) {
	name := filepath.Join(handoffDir, export.ManifestFilename)
	// The handoff directory is daemon-supplied, but its contents are the
	// untrusted boundary: a hostile handoff can plant a FIFO, device, or
	// symlink at manifest.json, so this open is hardened (no symlink
	// follow, no blocking, regular-file only) rather than a bare
	// os.Open, which would block on a FIFO or read through a symlink
	// before any validation runs.
	f, err := openRegular(name, ErrManifestUnreadable)
	if err != nil {
		return export.Manifest{}, fmt.Errorf("open manifest: %w: %w", ErrManifestUnreadable, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, pol.MaxManifestBytes+1))
	if err != nil {
		return export.Manifest{}, fmt.Errorf("read manifest: %w: %w", ErrManifestUnreadable, err)
	}
	if int64(len(data)) > pol.MaxManifestBytes {
		return export.Manifest{}, fmt.Errorf("manifest exceeds %d bytes: %w", pol.MaxManifestBytes, ErrManifestTooLarge)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m export.Manifest
	if err := dec.Decode(&m); err != nil {
		return export.Manifest{}, fmt.Errorf("decode manifest: %w: %w", ErrManifestInvalid, err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return export.Manifest{}, fmt.Errorf("manifest carries trailing content: %w", ErrManifestInvalid)
	}
	if len(m.Entries) > pol.MaxEntries {
		return export.Manifest{}, fmt.Errorf("%d entries exceed the cap of %d: %w", len(m.Entries), pol.MaxEntries, ErrManifestTooLarge)
	}
	if err := m.Validate(); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: %w", ErrManifestInvalid, err)
	}
	return m, nil
}
