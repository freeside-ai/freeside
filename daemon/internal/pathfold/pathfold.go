// Package pathfold centralizes the daemon's case, normalization, and
// filesystem-alias decisions for repository paths. It is a neutral leaf so
// import and verification trust boundaries cannot drift apart.
package pathfold

import (
	"path"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// caseFold performs Unicode full case folding, the fold a case-insensitive
// filesystem uses. It is stateless and safe to reuse.
var caseFold = cases.Fold()

// FoldComponent folds one repository path component using the case and
// Unicode normalization posture of the reference checkout filesystem. Full
// folding, not simple lowercasing, matches APFS: it folds ß→ss and the ﬁ
// ligature→fi while keeping İ (U+0130) apart from i.
func FoldComponent(component string) string {
	return caseFold.String(norm.NFC.String(component))
}

// FoldPath folds a whole path for use as a case- and
// normalization-insensitive collision or matching key.
func FoldPath(p string) string {
	components := strings.Split(p, "/")
	for i, component := range components {
		components[i] = FoldComponent(component)
	}
	return strings.Join(components, "/")
}

// NormalizeAliases folds each path component through the deterministic
// aliases a downstream NTFS/HFS checkout collapses: HFS-ignorable code points
// stripped, an NTFS alternate-data-stream suffix dropped at the first colon,
// and trailing dots/spaces trimmed. Case folding is left to MatchAny.
func NormalizeAliases(p string) string {
	components := strings.Split(p, "/")
	for i, component := range components {
		components[i] = normalizeComponentAliases(component)
	}
	return strings.Join(components, "/")
}

func normalizeComponentAliases(component string) string {
	if strings.ContainsFunc(component, hfsIgnorable) {
		var b strings.Builder
		for _, r := range component {
			if !hfsIgnorable(r) {
				b.WriteRune(r)
			}
		}
		component = b.String()
	}
	if i := strings.IndexByte(component, ':'); i >= 0 {
		component = component[:i]
	}
	return strings.TrimRight(component, ". ")
}

// hfsIgnorable reports the code points HFS+ filename comparison ignores,
// matching git's own protectHFS set.
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

// MatchAny reports whether p matches any slash-separated glob pattern. "**"
// spans any number of path segments; other segments use path.Match semantics.
// When fold is true, patterns and p use the same NFC plus Unicode full case
// fold as the APFS collision model. When false, case and normalization remain
// exact while the glob syntax still applies.
func MatchAny(patterns []string, p string, fold bool) bool {
	if fold {
		p = FoldPath(p)
	}
	for _, pattern := range patterns {
		if fold {
			pattern = FoldPath(pattern)
		}
		if matchSegments(strings.Split(pattern, "/"), strings.Split(p, "/")) {
			return true
		}
	}
	return false
}

func matchSegments(pattern, segments []string) bool {
	if len(pattern) == 0 {
		return len(segments) == 0
	}
	if pattern[0] == "**" {
		if matchSegments(pattern[1:], segments) {
			return true
		}
		if len(segments) > 0 {
			return matchSegments(pattern, segments[1:])
		}
		return false
	}
	if len(segments) == 0 {
		return false
	}
	matched, err := path.Match(pattern[0], segments[0])
	if err != nil || !matched {
		return false
	}
	return matchSegments(pattern[1:], segments[1:])
}

// ValidGlob reports whether every segment of a slash-separated pattern
// compiles under path.Match. "**" is handled specially by matchSegments and
// is always valid, so callers can reject an invalid policy before it silently
// matches nothing and fails open.
func ValidGlob(pattern string) error {
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "**" {
			continue
		}
		if _, err := path.Match(segment, ""); err != nil {
			return err
		}
	}
	return nil
}

// ValidSHA1Hex reports whether s is a full 40-character lowercase hex object
// name.
func ValidSHA1Hex(s string) bool {
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
