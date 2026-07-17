package verify

import (
	"path"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// DefaultVerificationControlPatterns is the §5.6 verification-control
// path class: files that steer what the recipe's commands actually
// execute or check. A candidate weakening lint config, swapping a
// dependency pin, or rewriting a build entrypoint is the
// "test: @echo passed" attack routed around the recipe, so a change
// here is mechanically identified and risk-flagged for the publication
// gate. These are a mandatory minimum, never disableable; the resolved
// recipe path itself joins the class at evaluation time. CI and
// reviewer-instruction paths are already the importer's
// publish-blocking classes and are deliberately not duplicated here.
var DefaultVerificationControlPatterns = []string{
	// Dependency pinning steers what `go test` compiles and runs.
	"**/go.mod",
	"**/go.sum",
	"**/go.work",
	"**/go.work.sum",
	// Build entrypoints commonly wrapped around verification.
	"**/Makefile",
	"**/GNUmakefile",
	"**/makefile",
	"**/*.mk",
	"**/justfile",
	"**/Taskfile.yml",
	"**/Taskfile.yaml",
	// Lint configuration is a verification-control surface by prior
	// decision (daemon bootstrap note): weakening it silently weakens
	// every later verification.
	"**/.golangci.yml",
	"**/.golangci.yaml",
	"**/.golangci.json",
	"**/.golangci.toml",
	// Attributes steer materialization (ident, text/eol, filter); the
	// verifier neutralizes them at checkout, and the flag makes the
	// attempt visible. Also the importer's git_metadata class;
	// duplicated here because it is verification-control, not only
	// checkout metadata.
	"**/.gitattributes",
}

// verificationControl returns the mandatory glob-pattern class: the
// defaults, the resolved recipe path, and any caller-supplied widening.
// The defaults ALWAYS apply: a caller can widen the class but never
// narrow or disable it (the importer's widen-only rule, held for the
// same §12 reason). The recipe's own command entrypoints are handled
// separately, by literal match (see flagControlPaths), because they
// name specific files rather than a protected-name pattern.
func verificationControl(extra []string, recipePath string) []string {
	patterns := make([]string, 0, len(DefaultVerificationControlPatterns)+1+len(extra))
	patterns = append(patterns, DefaultVerificationControlPatterns...)
	patterns = append(patterns, recipePath)
	return append(patterns, extra...)
}

// flagControlPaths raises a verification_control_path finding for every
// candidate change, deletions included, whose path is in the class:
// deleting a Makefile or the recipe steers verification exactly as
// rewriting one does. Changes are the importer's audited account; the
// verifier never re-derives the diff. A non-UTF-8 path (PathHex set)
// is flagged unconditionally: it cannot be matched honestly, and an
// unmatchable name must not be the way around a mandatory class.
//
// Two match kinds. The glob class (defaults, recipe path, widening) is
// matched against the alias-normalized path (trailing dot/space and
// NTFS ADS suffix trimmed, HFS-ignorables stripped): those are
// protected-name *patterns*, and a canonical candidate path can
// materialize as such a name on a downstream checkout. commandPaths are
// specific files the trusted recipe executes, so they are matched by
// exact case/normalization fold against the canonical change path with
// NO alias normalization and NO glob interpretation: the runner opens
// the literal name, so `scripts/check:fast.sh` or `scripts/check...sh`
// must match its own bytes, not a truncated or pattern-expanded form.
func flagControlPaths(changes []importer.Change, extra, commandPaths []string, recipePath string) []Finding {
	patterns := verificationControl(extra, recipePath)
	entrypoints := make(map[string]struct{}, len(commandPaths))
	for _, cp := range commandPaths {
		entrypoints[foldPath(cp)] = struct{}{}
	}
	var findings []Finding
	for _, c := range changes {
		if c.PathHex != "" {
			findings = append(findings, Finding{
				PathHex: c.PathHex,
				Kind:    FindingVerificationControlPath,
				Detail:  string(c.Kind) + "; path is not canonical UTF-8, so it cannot be honestly matched against the class",
			})
			continue
		}
		_, isEntrypoint := entrypoints[foldPath(c.Path)]
		if isEntrypoint || matchAny(patterns, normalizeAliases(c.Path)) {
			findings = append(findings, Finding{
				Path:   c.Path,
				Kind:   FindingVerificationControlPath,
				Detail: string(c.Kind),
			})
		}
	}
	return findings
}

// The matching machinery below is a deliberate package-local copy of
// the importer's (unexported there; shared-package edits are outside
// this unit's scope). It must stay decision-identical: a path the
// importer's classes would catch under folding must fold the same way
// here.

// matchAny reports whether p matches any of the slash-separated glob
// patterns under NFC + Unicode full case folding, where "**" spans any
// number of path segments and other segments use path.Match semantics.
func matchAny(patterns []string, p string) bool {
	p = foldPath(p)
	for _, pat := range patterns {
		if matchSegments(strings.Split(foldPath(pat), "/"), strings.Split(p, "/")) {
			return true
		}
	}
	return false
}

func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		if matchSegments(pat[1:], segs) {
			return true // ** spans zero segments
		}
		if len(segs) > 0 {
			return matchSegments(pat, segs[1:]) // ** consumes one and stays greedy
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], segs[1:])
}

// caseFold performs Unicode full case folding, the fold a
// case-insensitive filesystem uses. It is stateless and safe to reuse.
var caseFold = cases.Fold()

// foldPath folds a path the way a case- and normalization-insensitive
// filesystem does: per component, NFC-normalize then apply Unicode full
// case folding (ß→ss, the ﬁ ligature→fi), so an aliased spelling of a
// protected name still matches the class.
func foldPath(p string) string {
	comps := strings.Split(p, "/")
	for i, c := range comps {
		comps[i] = caseFold.String(norm.NFC.String(c))
	}
	return strings.Join(comps, "/")
}

// normalizeAliases folds each path component through the deterministic
// aliases a downstream NTFS/HFS checkout collapses: HFS-ignorable code
// points stripped, an NTFS alternate-data-stream suffix dropped
// (everything from the first colon), and trailing dots/spaces trimmed.
// Case folding is left to matchAny.
func normalizeAliases(p string) string {
	comps := strings.Split(p, "/")
	for i, c := range comps {
		comps[i] = normalizeComponentAliases(c)
	}
	return strings.Join(comps, "/")
}

func normalizeComponentAliases(c string) string {
	if strings.ContainsFunc(c, hfsIgnorable) {
		var b strings.Builder
		for _, r := range c {
			if !hfsIgnorable(r) {
				b.WriteRune(r)
			}
		}
		c = b.String()
	}
	if i := strings.IndexByte(c, ':'); i >= 0 {
		c = c[:i]
	}
	return strings.TrimRight(c, ". ")
}

// hfsIgnorable reports the code points HFS+ filename comparison
// ignores, matching git's own protectHFS set.
func hfsIgnorable(r rune) bool {
	switch {
	case r >= 0x200c && r <= 0x200f,
		r >= 0x202a && r <= 0x202e,
		r >= 0x206a && r <= 0x206f,
		r == 0xfeff:
		return true
	}
	return false
}
