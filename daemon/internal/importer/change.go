package importer

import "github.com/freeside-ai/freeside/daemon/internal/export"

// ChangeKind classifies one derived change against the enforced base.
// The zero value "" is invalid by design.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
)

// AllChangeKinds lists every valid ChangeKind.
var AllChangeKinds = []ChangeKind{ChangeAdded, ChangeModified, ChangeDeleted}

// valid is the validity predicate; as a predicate it uses default.
func (k ChangeKind) valid() bool {
	switch k {
	case ChangeAdded, ChangeModified, ChangeDeleted:
		return true
	default:
		return false
	}
}

// Change is one derived change, the importer's public account of what
// the candidate did relative to the enforced base. Mode and Digest
// describe the new content for adds and modifies and are empty for
// deletions. Path and PathHex are mutually exclusive, as in the
// manifest: a base path that is not representable as canonical UTF-8
// (some repositories legitimately track such names) is reported as raw
// bytes in PathHex so the JSON account is never lossy.
type Change struct {
	Path    string        `json:"path,omitempty"`
	PathHex string        `json:"path_hex,omitempty"`
	Kind    ChangeKind    `json:"kind"`
	Mode    string        `json:"mode,omitempty"`
	Digest  export.Digest `json:"digest,omitempty"`
}

// plannedChange is a derived change plus the construction-facing
// identity the public Change omits: the git index mode and the blob
// object name content verification derived, which construction
// cross-checks against what git actually ingested.
type plannedChange struct {
	path   string
	kind   ChangeKind
	mode   string        // git index mode ("100644"/"100755"); empty for deletions
	oid    string        // expected git blob object name; empty for deletions
	digest export.Digest // sha256; empty for deletions
	size   int64
	// verifiedPath names the daemon-private snapshot created by content
	// verification. Construction consumes this path, never the handoff.
	verifiedPath string
	// pathHex, when set, is the hex of the raw path bytes for a base
	// path that is not representable as canonical UTF-8. path still
	// holds the raw bytes for git's NUL-safe channels; pathHex is what
	// the lossless public Change reports.
	pathHex string
	// fromBase marks a change whose content object is already in the
	// checkout's object database (a mode-only change on a file whose
	// blob the export caps omitted, where base holds the identical
	// bytes). Its oid comes from the trusted base tree, so construction
	// neither ingests a handoff blob for it nor cross-checks ingestion.
	fromBase bool
}

// public renders the construction-facing form as the API's Change,
// reporting a non-representable path losslessly via PathHex.
func (c plannedChange) public() Change {
	if c.pathHex != "" {
		return Change{PathHex: c.pathHex, Kind: c.kind, Mode: c.mode, Digest: c.digest}
	}
	return Change{Path: c.path, Kind: c.kind, Mode: c.mode, Digest: c.digest}
}

// finding builds a Finding about this change, reporting a
// non-representable path losslessly via PathHex (matching public), so a
// policy classification of a non-UTF-8 path is never rendered lossy.
func (c plannedChange) finding(kind FindingKind, detail string) Finding {
	if c.pathHex != "" {
		return Finding{PathHex: c.pathHex, Kind: kind, Detail: detail}
	}
	return Finding{Path: c.path, Kind: kind, Detail: detail}
}
