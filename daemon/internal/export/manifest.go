package export

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ManifestVersion identifies the wire format this package emits. The
// manifest+blob layout is the gauntlet-internal contract between the export
// helper and the hostile importer; any incompatible change bumps this string.
const ManifestVersion = "freeside.export.manifest/v1"

// Digest is a sha256 content address in the repo's canonical
// "sha256:<64 lowercase hex>" form. Defined locally: the helper is a
// standalone binary and imports nothing from the shared domain package.
type Digest string

func (d Digest) valid() bool {
	s, ok := strings.CutPrefix(string(d), "sha256:")
	if !ok || len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// EntryKind classifies one workspace entry. Only regular entries carry
// content (digest, size, mode, blob); every other kind is recorded for the
// importer's publish-blocking enforcement and never blobbed.
type EntryKind string

const (
	// EntryRegular is a regular file, normalized to git semantics: the mode
	// keeps only the owner-executable bit (0644 or 0755) and the content is
	// digest-addressed.
	EntryRegular EntryKind = "regular"
	// EntrySymlink records a symbolic link and its target verbatim; the
	// target is never resolved or followed.
	EntrySymlink EntryKind = "symlink"
	// EntrySubmodule records a directory that carries its own .git entry (a
	// nested working tree); its content is not descended into. The pointer
	// commit could only come from untrusted workspace .git state, so none is
	// recorded.
	EntrySubmodule EntryKind = "submodule"
	// EntrySpecial records a non-regular, non-symlink filesystem object
	// (FIFO, socket, device, or other irregular file).
	EntrySpecial EntryKind = "special"
	// EntryUnusualMode records a regular file whose mode carries
	// setuid/setgid/sticky bits; it is recorded unnormalized and unblobbed.
	EntryUnusualMode EntryKind = "unusual_mode"
	// EntryGitDir is the workspace's own top-level .git, recorded as a
	// single entry and never walked or blobbed (its content is untrusted by
	// design, plan §5.6).
	EntryGitDir EntryKind = "git_dir"
	// EntryInvalidPath records a name whose raw bytes cannot be represented
	// as a canonical UTF-8 path; the bytes are preserved losslessly in
	// path_hex. A directory of this kind is not descended into.
	EntryInvalidPath EntryKind = "invalid_path"
)

// AllEntryKinds lists every valid EntryKind.
var AllEntryKinds = []EntryKind{
	EntryRegular,
	EntrySymlink,
	EntrySubmodule,
	EntrySpecial,
	EntryUnusualMode,
	EntryGitDir,
	EntryInvalidPath,
}

func (k EntryKind) valid() bool {
	switch k {
	case EntryRegular, EntrySymlink, EntrySubmodule, EntrySpecial,
		EntryUnusualMode, EntryGitDir, EntryInvalidPath:
		return true
	default:
		return false
	}
}

// Entry is one manifest line, binding a workspace path to what the helper
// observed there. Kind-dependent fields are pointers rendering explicit
// null, per the domain golden convention. Path and PathHex are mutually
// exclusive: PathHex carries the raw name bytes only for invalid_path
// entries.
type Entry struct {
	Path        string    `json:"path,omitempty"`
	PathHex     string    `json:"path_hex,omitempty"`
	Kind        EntryKind `json:"kind"`
	Mode        *string   `json:"mode"`
	Size        *int64    `json:"size"`
	Digest      *Digest   `json:"digest"`
	Target      *string   `json:"target"`
	BlobOmitted bool      `json:"blob_omitted,omitempty"`
}

// Manifest is the normalized description of one exported workspace: schema
// version plus entries sorted bytewise by path. It deliberately carries no
// counts, timestamps, or host details, so an identical workspace yields
// byte-identical output.
type Manifest struct {
	Version string  `json:"version"`
	Entries []Entry `json:"entries"`
}

// sortKey is the raw name bytes an entry sorts and deduplicates by: Path for
// representable entries, the decoded PathHex bytes otherwise. Validate has
// already established that exactly one of the two is set.
func (e Entry) sortKey() []byte {
	if e.Kind == EntryInvalidPath {
		raw, err := hex.DecodeString(e.PathHex)
		if err != nil {
			return []byte(e.PathHex)
		}
		return raw
	}
	return []byte(e.Path)
}

// validCanonicalPath reports whether p is the canonical relative
// slash-separated form the manifest requires: fs.ValidPath, not the root,
// valid UTF-8, and free of NUL (which fs.ValidPath and utf8.ValidString
// both accept, yet no real filesystem path can carry; this is the
// canonical-path gate for hostile manifests, so it fails here rather than
// reaching the importer looking canonical).
func validCanonicalPath(p string) bool {
	return p != "." && fs.ValidPath(p) && utf8.ValidString(p) &&
		!strings.ContainsRune(p, 0)
}

// Validate reports whether the entry is well-formed: a valid kind, a
// canonical name, and exactly the fields its kind requires.
func (e Entry) Validate() error {
	if !e.Kind.valid() {
		return fmt.Errorf("entry kind %q: %w", e.Kind, ErrInvalidEntryKind)
	}
	if e.Kind != EntryInvalidPath {
		if !validCanonicalPath(e.Path) {
			return fmt.Errorf("entry path %q: %w", e.Path, ErrInvalidPath)
		}
		if e.PathHex != "" {
			return fmt.Errorf("entry %q: path_hex on a representable path: %w", e.Path, ErrKindFieldMismatch)
		}
	}
	switch e.Kind {
	case EntryRegular:
		return e.validateRegular()
	case EntrySymlink:
		if e.Target == nil {
			return fmt.Errorf("symlink %q lacks a target: %w", e.Path, ErrKindFieldMismatch)
		}
		return e.forbidExcept(fieldTarget)
	case EntrySubmodule:
		return e.forbidExcept(fieldNone)
	case EntrySpecial:
		return e.forbidExcept(fieldNone)
	case EntryUnusualMode:
		if e.Mode == nil || !validUnusualMode(*e.Mode) {
			return fmt.Errorf("unusual_mode entry %q: %w", e.Path, ErrInvalidMode)
		}
		return e.forbidExcept(fieldMode)
	case EntryGitDir:
		if e.Path != ".git" {
			return fmt.Errorf("git_dir entry %q must be the workspace's own .git: %w", e.Path, ErrInvalidPath)
		}
		return e.forbidExcept(fieldNone)
	case EntryInvalidPath:
		return e.validateInvalidPath()
	}
	return fmt.Errorf("entry kind %q: %w", e.Kind, ErrInvalidEntryKind)
}

// The kind-dependent field a non-regular kind is allowed to carry, for
// forbidExcept.
type allowedField int

const (
	fieldNone allowedField = iota
	fieldMode
	fieldTarget
)

// forbidExcept enforces that no kind-dependent field is set beyond the one
// the entry's kind allows. Regular entries have their own validator; every
// other kind carries at most one field.
func (e Entry) forbidExcept(allowed allowedField) error {
	name := e.Path
	if name == "" {
		name = e.PathHex
	}
	if e.Mode != nil && allowed != fieldMode {
		return fmt.Errorf("entry %q: mode on a %s entry: %w", name, e.Kind, ErrKindFieldMismatch)
	}
	if e.Target != nil && allowed != fieldTarget {
		return fmt.Errorf("entry %q: target on a %s entry: %w", name, e.Kind, ErrKindFieldMismatch)
	}
	if e.Size != nil {
		return fmt.Errorf("entry %q: size on a %s entry: %w", name, e.Kind, ErrKindFieldMismatch)
	}
	if e.Digest != nil {
		return fmt.Errorf("entry %q: digest on a %s entry: %w", name, e.Kind, ErrKindFieldMismatch)
	}
	if e.BlobOmitted {
		return fmt.Errorf("entry %q: blob_omitted on a %s entry: %w", name, e.Kind, ErrKindFieldMismatch)
	}
	return nil
}

func (e Entry) validateRegular() error {
	if e.Mode == nil || (*e.Mode != "0644" && *e.Mode != "0755") {
		return fmt.Errorf("regular entry %q: mode must be 0644 or 0755: %w", e.Path, ErrInvalidMode)
	}
	if e.Size == nil {
		return fmt.Errorf("regular entry %q lacks a size: %w", e.Path, ErrKindFieldMismatch)
	}
	if *e.Size < 0 {
		return fmt.Errorf("regular entry %q: %w", e.Path, ErrNegativeSize)
	}
	if e.Digest == nil || !e.Digest.valid() {
		return fmt.Errorf("regular entry %q: %w", e.Path, ErrInvalidDigest)
	}
	if e.Target != nil {
		return fmt.Errorf("regular entry %q carries a target: %w", e.Path, ErrKindFieldMismatch)
	}
	return nil
}

func (e Entry) validateInvalidPath() error {
	if e.Path != "" {
		return fmt.Errorf("invalid_path entry carries a path %q: %w", e.Path, ErrKindFieldMismatch)
	}
	if e.PathHex == "" || len(e.PathHex)%2 != 0 || e.PathHex != strings.ToLower(e.PathHex) {
		return fmt.Errorf("invalid_path entry %q: %w", e.PathHex, ErrInvalidPathHex)
	}
	raw, err := hex.DecodeString(e.PathHex)
	if err != nil {
		return fmt.Errorf("invalid_path entry %q: %w", e.PathHex, ErrInvalidPathHex)
	}
	if validCanonicalPath(string(raw)) {
		return fmt.Errorf("invalid_path entry %q decodes to a representable path: %w", e.PathHex, ErrInvalidPathHex)
	}
	return e.forbidExcept(fieldNone)
}

// validUnusualMode reports whether s is the five-digit octal form of a mode
// carrying at least one setuid/setgid/sticky bit, e.g. "04755".
func validUnusualMode(s string) bool {
	if len(s) != 5 || s[0] != '0' {
		return false
	}
	m, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return false
	}
	return m&0o7000 != 0
}

// Validate reports whether the manifest is well-formed: the known version,
// every entry valid, and entries in canonical order (strictly ascending by
// raw name bytes, so the encoding is deterministic and names are unique).
func (m Manifest) Validate() error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("manifest version %q: %w", m.Version, ErrUnknownManifestVersion)
	}
	for i, e := range m.Entries {
		if err := e.Validate(); err != nil {
			return err
		}
		if i > 0 && bytes.Compare(m.Entries[i-1].sortKey(), e.sortKey()) >= 0 {
			return fmt.Errorf("entry %d: %w", i, ErrEntriesNotCanonical)
		}
	}
	return nil
}

// Encode returns the manifest's wire bytes: the validated value marshaled
// with two-space indentation plus a trailing newline. This is the exact
// byte form written to manifest.json and pinned by the golden tests;
// identical workspaces encode to identical bytes.
func (m Manifest) Encode() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return append(body, '\n'), nil
}
