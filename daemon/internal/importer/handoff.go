package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// loadManifest reads and re-validates the handoff manifest. The read is
// capped before any byte is parsed; the raw bytes must be valid UTF-8
// (encoding/json otherwise launders invalid bytes to U+FFFD, slipping a
// hostile path past validCanonicalPath); the decode is strict (an unknown
// field or trailing content is hostile until a coordinated v1 widening
// lands in-tree, since the importer is the format's only consumer); and
// the export package's own Validate re-runs at this boundary, so nothing
// downstream ever sees a manifest the producer's rules would reject (the
// trust-boundary re-gate convention).
//
// Unlike export.DecodeEvidenceManifest, this boundary is deliberately not
// canonical-or-reject (no re-encode byte-equality gate): the repo manifest
// records symlink targets verbatim (walk.go), and Encode is non-idempotent
// on invalid UTF-8. json.Marshal escapes a raw invalid byte as a six-byte
// ASCII backslash-u escape of the replacement rune, but re-emits an
// already-decoded U+FFFD rune as its raw three bytes, so a legitimate
// lossy target fails a byte-equality check against Encode's own earlier
// output. Such a gate would reject honest
// exporter output (see TestImportLossySymlinkTargetNeverElides). The
// evidence manifest has no verbatim field, so it can afford the stricter
// gate.
//
// The one leniency that gate would close and that matters here is
// duplicate-key last-wins: a hostile manifest can hide an over-cap
// "entries" array behind a second "entries" key so json discards it after
// building it, forcing a byte-cap-sized allocation that then validates as
// small. The streaming entry-count gate below handles that directly by
// summing every "entries" array before the typed decode. The remaining
// leniencies (whitespace, key order) smuggle nothing: the repo manifest
// carries no trust bit, and Validate re-gates whatever value the decoder
// resolves.
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
	// Checked before decode because encoding/json replaces invalid UTF-8
	// with U+FFFD instead of failing, laundering hostile bytes into a
	// valid-looking path that then passes validCanonicalPath. Scanning the
	// raw bytes is the only place this is catchable; every check after the
	// decode sees the already-laundered string.
	if !utf8.Valid(data) {
		return export.Manifest{}, fmt.Errorf("manifest bytes: %w: %w", ErrManifestInvalid, export.ErrInvalidUTF8)
	}
	// Bound the entry count BEFORE the typed decode (the shared helper the
	// evidence channel uses), so a hostile manifest cannot force a
	// byte-cap-sized []Entry allocation and then validate as small. The
	// streaming count sums every "entries" array's elements, matched
	// case-insensitively like the decoder and including duplicate keys, so an
	// over-cap array hidden under an alternate spelling ("Entries") or behind
	// a second "entries" key that json's last-wins would discard is rejected
	// before it is allocated. The post-decode len check below stays the
	// authoritative cap: this gate is the allocation bound, not the source of
	// truth, so a future divergence from the decoder's key matching cannot
	// silently raise the effective cap.
	if manifestEntryCountExceeds(data, pol.MaxEntries) {
		return export.Manifest{}, fmt.Errorf("entries exceed the cap of %d: %w", pol.MaxEntries, ErrManifestTooLarge)
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
	if err := capPaths(m, pol); err != nil {
		return export.Manifest{}, err
	}
	return m, nil
}

// capPaths bounds each entry's path length and component depth. Later
// stages (the structural gate's ancestor walk, the collision check's
// ancestor lookups) do work superlinear in a single path, so one deeply
// nested path well under the total manifest cap would otherwise force
// quadratic time and memory. A real repository entry never approaches
// these ceilings (a path past PATH_MAX cannot be checked out), so the
// caps only reject forged manifests.
func capPaths(m export.Manifest, pol Policy) error {
	for _, e := range m.Entries {
		name, depth := e.Path, 0
		if e.Kind == export.EntryInvalidPath {
			name = e.PathHex // hex of the raw bytes: twice the byte length, still bounded
		} else {
			depth = strings.Count(e.Path, "/") + 1
		}
		if int64(len(name)) > pol.MaxPathBytes {
			return fmt.Errorf("entry path length %d exceeds the cap of %d: %w", len(name), pol.MaxPathBytes, ErrManifestTooLarge)
		}
		if depth > pol.MaxPathDepth {
			return fmt.Errorf("entry path depth %d exceeds the cap of %d: %w", depth, pol.MaxPathDepth, ErrManifestTooLarge)
		}
	}
	return nil
}
