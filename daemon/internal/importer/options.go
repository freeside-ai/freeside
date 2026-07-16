package importer

import (
	"fmt"
	"time"
)

// Default caps. The blob caps mirror the export helper's defaults so an
// unconfigured importer accepts exactly what an unconfigured exporter
// emits; the manifest byte cap bounds the intake read before any byte
// is parsed. Unlike the exporter (whose zero disables a cap), a zero
// here selects the default and a negative value is invalid: this is the
// hostile boundary, and an accidentally uncapped import fails the wrong
// way.
const (
	DefaultMaxManifestBytes = 256 << 20
	DefaultMaxEntries       = 1_000_000
	DefaultMaxBlobBytes     = 100 << 20
	DefaultMaxTotalBytes    = 1 << 30
)

// Default daemon authorship for the clean commit. §5.6: the daemon
// authors its own commit; no agent-supplied identity ever appears. The
// email's reserved .invalid TLD says honestly that it is not a mailbox.
const (
	DefaultAuthorName    = "freeside-daemon"
	DefaultAuthorEmail   = "daemon@freeside.invalid"
	DefaultCommitMessage = "freeside: gauntlet import"
)

// Options configures one import. The zero value of every field except
// BaseSHA selects a documented default.
type Options struct {
	// BaseSHA is the enforced base: the exact commit the agent workspace
	// was spawned from, supplied from the daemon's own records. The
	// manifest deliberately carries no base field (workspace parentage
	// is untrusted), and the checkout's HEAD must resolve to exactly
	// this commit. Required, 40 lowercase hex.
	BaseSHA string
	// ImportRef, when set, is a fully qualified ref (refs/...) updated
	// to point at the produced commit, anchoring it against gc.
	ImportRef string
	// CommitMessage is the daemon-authored commit message.
	CommitMessage string
	// AuthorName and AuthorEmail are the daemon identity recorded as
	// both author and committer.
	AuthorName  string
	AuthorEmail string
	// CommitDate pins the author and committer time (rendered UTC); the
	// zero value means the current time. Pinning it makes the produced
	// commit SHA deterministic for a given base and change set.
	CommitDate time.Time
	// GitPath is the git binary to run; empty means "git" from PATH.
	GitPath string
	// Policy is the import's policy surface.
	Policy Policy
}

// Policy is the import's policy surface: the path-class patterns, the
// declared-scope allowlist, and the caps enforced at intake and over
// the change set.
type Policy struct {
	// Allowlist, when non-nil, is the work unit's declared path scope as
	// glob patterns ("**" spans path segments): every derived change,
	// deletions included, must match one, and a change outside it is an
	// allowlist_violation finding. nil means unrestricted; an empty
	// non-nil list flags every change.
	Allowlist []string
	// ExtraAutomationControlPatterns is ADDED to the mandatory §5.5
	// automation-control class; it can widen the gate but never narrows
	// or disables it (the defaults always apply).
	ExtraAutomationControlPatterns []string
	// ExtraReviewerInstructionPatterns is ADDED to the mandatory §5.8
	// reviewer-instruction class, with the same widen-only semantics.
	ExtraReviewerInstructionPatterns []string
	// ExtraGitMetadataPatterns is ADDED to the mandatory git-metadata
	// class, with the same widen-only semantics.
	ExtraGitMetadataPatterns []string
	// MaxManifestBytes caps the manifest.json read.
	MaxManifestBytes int64
	// MaxEntries caps the manifest entry count.
	MaxEntries int
	// MaxBlobBytes is the largest changed file the size policy accepts
	// without a size_violation finding.
	MaxBlobBytes int64
	// MaxTotalBytes bounds the summed size of added and modified
	// content before the change set as a whole is a size_violation.
	MaxTotalBytes int64
}

// withDefaults returns a copy with every zero field set to its default.
func (o Options) withDefaults() Options {
	if o.CommitMessage == "" {
		o.CommitMessage = DefaultCommitMessage
	}
	if o.AuthorName == "" {
		o.AuthorName = DefaultAuthorName
	}
	if o.AuthorEmail == "" {
		o.AuthorEmail = DefaultAuthorEmail
	}
	if o.GitPath == "" {
		o.GitPath = "git"
	}
	o.Policy = o.Policy.withDefaults()
	return o
}

func (p Policy) withDefaults() Policy {
	if p.MaxManifestBytes == 0 {
		p.MaxManifestBytes = DefaultMaxManifestBytes
	}
	if p.MaxEntries == 0 {
		p.MaxEntries = DefaultMaxEntries
	}
	if p.MaxBlobBytes == 0 {
		p.MaxBlobBytes = DefaultMaxBlobBytes
	}
	if p.MaxTotalBytes == 0 {
		p.MaxTotalBytes = DefaultMaxTotalBytes
	}
	return p
}

// validate rejects an invocation the import must not even start:
// options are daemon-supplied, so a violation is a caller bug, not
// hostile input, and it fails loud.
func (o Options) validate() error {
	if !validSHA1Hex(o.BaseSHA) {
		return fmt.Errorf("base SHA %q is not 40 lowercase hex: %w", o.BaseSHA, ErrInvalidOptions)
	}
	if o.ImportRef != "" && !importRefValid(o.ImportRef) {
		return fmt.Errorf("import ref %q is not a fully qualified safe ref: %w", o.ImportRef, ErrInvalidOptions)
	}
	if o.Policy.MaxManifestBytes < 0 || o.Policy.MaxEntries < 0 ||
		o.Policy.MaxBlobBytes < 0 || o.Policy.MaxTotalBytes < 0 {
		return fmt.Errorf("negative policy cap: %w", ErrInvalidOptions)
	}
	// A caller-supplied glob that does not compile would otherwise
	// silently match nothing (fail open), so a safety-gate widening
	// meant to add coverage would add none. Reject at the boundary
	// instead: these patterns are daemon-supplied, so a bad one is a
	// caller bug that fails loud.
	for _, group := range [][]string{
		o.Policy.Allowlist,
		o.Policy.ExtraAutomationControlPatterns,
		o.Policy.ExtraReviewerInstructionPatterns,
		o.Policy.ExtraGitMetadataPatterns,
	} {
		for _, pat := range group {
			if err := validGlob(pat); err != nil {
				return fmt.Errorf("policy pattern %q: %w: %w", pat, err, ErrInvalidOptions)
			}
		}
	}
	return nil
}

// validSHA1Hex reports whether s is a full 40-character lowercase hex
// object name (the sha1 object format this package requires).
func validSHA1Hex(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
