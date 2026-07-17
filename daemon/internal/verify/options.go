package verify

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// Defaults. As at the importer's hostile boundary, a zero cap selects
// the default and a negative value is invalid: an accidentally
// unbounded verification must fail the wrong way loudly.
const (
	// DefaultRecipePath is where the trusted recipe lives in a repository
	// that carries one in-tree (the base-commit source); onboarding
	// adopts this path.
	DefaultRecipePath = ".freeside/verify.json"
	// DefaultMaxRecipeBytes caps the recipe blob read; a recipe is a
	// small command list, so a megabyte is already generous.
	DefaultMaxRecipeBytes = 1 << 20
	// DefaultMaxTranscriptBytes caps the assembled command transcript.
	DefaultMaxTranscriptBytes = 1 << 20
	// DefaultCommandTimeout bounds one recipe command, aligned with the
	// §5.12 stage-active-time budget example.
	DefaultCommandTimeout = 45 * time.Minute
)

// Options configures one verification. The zero value of every field
// except HeadSHA, BaseSHA, InvocationID, RecipeSource, and Room selects
// a documented default.
type Options struct {
	// HeadSHA is the candidate commit to verify: the importer's
	// Result.CommitSHA, supplied from the daemon's own records.
	// Required, 40 lowercase hex.
	HeadSHA string
	// BaseSHA is the enforced base the candidate was imported onto; the
	// base-commit recipe source reads the recipe at exactly this commit.
	// Required, 40 lowercase hex.
	BaseSHA string
	// InvocationID is the verifier invocation recorded in every evidence
	// artifact's provenance (§5.15). Required.
	InvocationID domain.InvocationID
	// RecipeSource declares where the trusted recipe loads from (§5.8).
	// Required: there is no default source and never a candidate one.
	RecipeSource RecipeSource
	// RecipePath is the recipe's in-tree path, used by the base-commit
	// source and by divergence detection. Empty means DefaultRecipePath.
	RecipePath string
	// Room executes the recipe's commands in the materialized workspace.
	// Required.
	Room Room
	// ApprovedRecipes is the trusted policy's approved-recipe set;
	// publish eligibility of the emitted evidence derives from it. Nil
	// approves nothing, so evidence is emitted publish-ineligible: the
	// fail-closed direction.
	ApprovedRecipes map[domain.Digest]bool
	// Changes is the importer's audited account of the candidate change
	// set; verification-control flagging consumes it. The verifier never
	// re-derives the diff.
	Changes []importer.Change
	// GitPath is the git binary to run; empty means "git" from PATH.
	GitPath string
	// Policy is the verification's policy surface.
	Policy Policy
}

// Policy is the verification's policy surface: the widen-only
// verification-control pattern extension and the execution caps.
type Policy struct {
	// ExtraVerificationControlPatterns is ADDED to the mandatory §5.6
	// verification-control class; it can widen the class but never
	// narrows or disables it (the defaults always apply).
	ExtraVerificationControlPatterns []string
	// MaxRecipeBytes caps the recipe blob reads (trusted base and
	// candidate head copies alike).
	MaxRecipeBytes int64
	// MaxTranscriptBytes caps the assembled command transcript.
	MaxTranscriptBytes int64
	// CommandTimeout bounds each recipe command; a command that
	// overruns is killed and recorded as a failed step.
	CommandTimeout time.Duration
}

// withDefaults returns a copy with every zero field set to its default.
func (o Options) withDefaults() Options {
	if o.RecipePath == "" {
		o.RecipePath = DefaultRecipePath
	}
	if o.GitPath == "" {
		o.GitPath = "git"
	}
	o.Policy = o.Policy.withDefaults()
	return o
}

func (p Policy) withDefaults() Policy {
	if p.MaxRecipeBytes == 0 {
		p.MaxRecipeBytes = DefaultMaxRecipeBytes
	}
	if p.MaxTranscriptBytes == 0 {
		p.MaxTranscriptBytes = DefaultMaxTranscriptBytes
	}
	if p.CommandTimeout == 0 {
		p.CommandTimeout = DefaultCommandTimeout
	}
	return p
}

// validate rejects an invocation the verification must not even start:
// options are daemon-supplied, so a violation is a caller bug, not
// hostile input, and it fails loud.
func (o Options) validate() error {
	if !validSHA1Hex(o.HeadSHA) {
		return fmt.Errorf("head SHA %q is not 40 lowercase hex: %w", o.HeadSHA, ErrInvalidOptions)
	}
	if !validSHA1Hex(o.BaseSHA) {
		return fmt.Errorf("base SHA %q is not 40 lowercase hex: %w", o.BaseSHA, ErrInvalidOptions)
	}
	if o.InvocationID == "" {
		return fmt.Errorf("invocation id is empty: %w", ErrInvalidOptions)
	}
	if !o.RecipeSource.valid() {
		return fmt.Errorf("recipe source is unset: %w", ErrInvalidOptions)
	}
	if o.Room == nil {
		return fmt.Errorf("room is unset: %w", ErrInvalidOptions)
	}
	if err := validRecipePath(o.RecipePath); err != nil {
		return fmt.Errorf("recipe path %q: %w: %w", o.RecipePath, err, ErrInvalidOptions)
	}
	if o.Policy.MaxRecipeBytes < 0 || o.Policy.MaxTranscriptBytes < 0 || o.Policy.CommandTimeout < 0 {
		return fmt.Errorf("negative policy cap: %w", ErrInvalidOptions)
	}
	// A widening glob that does not compile would silently match nothing
	// (fail open), so a safety widening meant to add coverage would add
	// none. Reject at the boundary instead.
	for _, pat := range o.Policy.ExtraVerificationControlPatterns {
		if err := validGlob(pat); err != nil {
			return fmt.Errorf("policy pattern %q: %w: %w", pat, err, ErrInvalidOptions)
		}
	}
	return nil
}

// validRecipePath keeps the daemon-supplied recipe path inside the
// tree and representable in both the <commit>:<path> spec and the
// alias-normalized pattern match: relative, slash-separated, no empty
// or dot components, and no colon (the spec separator) or backslash.
func validRecipePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, ":\\") {
		return fmt.Errorf("must be a relative slash path without colon or backslash")
	}
	for _, comp := range strings.Split(p, "/") {
		if comp == "" || comp == "." || comp == ".." {
			return fmt.Errorf("component %q is not allowed", comp)
		}
	}
	return nil
}

// validGlob reports whether every segment of a slash-separated pattern
// compiles under path.Match ("**" is handled specially by matchSegments
// and always valid), so an unparseable widening pattern fails closed at
// this boundary rather than silently matching nothing.
func validGlob(pattern string) error {
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return err
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
