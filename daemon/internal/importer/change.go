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
// deletions.
type Change struct {
	Path   string        `json:"path"`
	Kind   ChangeKind    `json:"kind"`
	Mode   string        `json:"mode,omitempty"`
	Digest export.Digest `json:"digest,omitempty"`
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
}

// public renders the construction-facing form as the API's Change.
func (c plannedChange) public() Change {
	return Change{Path: c.path, Kind: c.kind, Mode: c.mode, Digest: c.digest}
}
