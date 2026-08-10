package pathfold_test

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
)

type operation int

const (
	foldComponent operation = iota
	foldPath
	foldEqual
	normalizeAliases
	matchAny
	validGlob
	validSHA1Hex
)

// TestPrimitives pins every alias and validation axis shared by the importer
// and verifier in one table so a new rule has one decision surface and one
// regression corpus.
func TestPrimitives(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name     string
		op       operation
		input    string
		other    string
		patterns []string
		fold     bool
		want     string
		wantBool bool
		wantErr  bool
	}{
		// Case and Unicode normalization.
		{name: "simple case", op: foldComponent, input: "ReadMe", want: "readme"},
		{name: "sharp s full fold", op: foldComponent, input: "Straße", want: "strasse"},
		{name: "ligature full fold", op: foldComponent, input: "ﬁle", want: "file"},
		{name: "dotted capital i stays distinct", op: foldEqual, input: "İ", other: "i", wantBool: false},
		{name: "composed and decomposed equal", op: foldEqual, input: "é", other: "e\u0301", wantBool: true},
		{name: "multi segment fold", op: foldPath, input: "Dir/Straße/ﬁle", want: "dir/strasse/file"},

		// HFS+ ignorables: every range boundary plus adjacent retained code points.
		{name: "hfs first range lower", op: normalizeAliases, input: "a\u200cb", want: "ab"},
		{name: "hfs first range upper", op: normalizeAliases, input: "a\u200fb", want: "ab"},
		{name: "hfs second range lower", op: normalizeAliases, input: "a\u202ab", want: "ab"},
		{name: "hfs second range upper", op: normalizeAliases, input: "a\u202eb", want: "ab"},
		{name: "hfs third range lower", op: normalizeAliases, input: "a\u206ab", want: "ab"},
		{name: "hfs third range upper", op: normalizeAliases, input: "a\u206fb", want: "ab"},
		{name: "hfs byte order mark", op: normalizeAliases, input: "a\ufeffb", want: "ab"},
		{name: "hfs adjacent before retained", op: normalizeAliases, input: "a\u200bb", want: "a\u200bb"},
		{name: "hfs adjacent gap retained", op: normalizeAliases, input: "a\u2029b", want: "a\u2029b"},

		// NTFS alternate streams and trailing aliases.
		{name: "ads stream", op: normalizeAliases, input: "name:stream", want: "name"},
		{name: "ads unnamed data", op: normalizeAliases, input: "name::$DATA", want: "name"},
		{name: "ads bare colon", op: normalizeAliases, input: "name:", want: "name"},
		{name: "ads non final component", op: normalizeAliases, input: "dir:stream/file", want: "dir/file"},
		{name: "trailing dot", op: normalizeAliases, input: "name.", want: "name"},
		{name: "trailing space", op: normalizeAliases, input: "name ", want: "name"},
		{name: "trailing mixed dots spaces", op: normalizeAliases, input: "name. . ", want: "name"},
		{name: "interior dots spaces survive", op: normalizeAliases, input: "na. me", want: "na. me"},

		// Separators, segment matching, and exact-vs-folded policy.
		{name: "empty slash component survives", op: foldPath, input: "Dir//File", want: "dir//file"},
		{name: "backslash is not separator", op: matchAny, input: `Dir\File`, patterns: []string{"dir/file"}, fold: true, wantBool: false},
		{name: "double star zero segments", op: matchAny, input: "a/b", patterns: []string{"a/**/b"}, wantBool: true},
		{name: "double star one segment", op: matchAny, input: "a/x/b", patterns: []string{"a/**/b"}, wantBool: true},
		{name: "double star many segments", op: matchAny, input: "a/x/y/b", patterns: []string{"a/**/b"}, wantBool: true},
		{name: "double star greedy continuation", op: matchAny, input: "a/x/b/y/b", patterns: []string{"a/**/b"}, wantBool: true},
		{name: "folded match", op: matchAny, input: "Straße/FILE", patterns: []string{"strasse/file"}, fold: true, wantBool: true},
		{name: "exact match rejects case alias", op: matchAny, input: "Straße/FILE", patterns: []string{"strasse/file"}, fold: false, wantBool: false},

		// Fail-closed glob and SHA-1 validation.
		{name: "double star glob valid", op: validGlob, input: "a/**/b"},
		{name: "unclosed class invalid", op: validGlob, input: "a/[/b", wantErr: true},
		{name: "sha 39 characters", op: validSHA1Hex, input: sha[:39], wantBool: false},
		{name: "sha 40 lowercase hex", op: validSHA1Hex, input: sha, wantBool: true},
		{name: "sha 41 characters", op: validSHA1Hex, input: sha + "0", wantBool: false},
		{name: "sha uppercase hex", op: validSHA1Hex, input: strings.ToUpper(sha), wantBool: false},
		{name: "sha non hex", op: validSHA1Hex, input: sha[:39] + "g", wantBool: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			switch tt.op {
			case foldComponent:
				if got := pathfold.FoldComponent(tt.input); got != tt.want {
					t.Fatalf("FoldComponent(%q) = %q, want %q", tt.input, got, tt.want)
				}
			case foldPath:
				if got := pathfold.FoldPath(tt.input); got != tt.want {
					t.Fatalf("FoldPath(%q) = %q, want %q", tt.input, got, tt.want)
				}
			case foldEqual:
				got := pathfold.FoldComponent(tt.input) == pathfold.FoldComponent(tt.other)
				if got != tt.wantBool {
					t.Fatalf("fold equality for %q and %q = %v, want %v", tt.input, tt.other, got, tt.wantBool)
				}
			case normalizeAliases:
				if got := pathfold.NormalizeAliases(tt.input); got != tt.want {
					t.Fatalf("NormalizeAliases(%q) = %q, want %q", tt.input, got, tt.want)
				}
			case matchAny:
				if got := pathfold.MatchAny(tt.patterns, tt.input, tt.fold); got != tt.wantBool {
					t.Fatalf("MatchAny(%q, %q, %v) = %v, want %v", tt.patterns, tt.input, tt.fold, got, tt.wantBool)
				}
			case validGlob:
				if gotErr := pathfold.ValidGlob(tt.input) != nil; gotErr != tt.wantErr {
					t.Fatalf("ValidGlob(%q) error = %v, want error %v", tt.input, gotErr, tt.wantErr)
				}
			case validSHA1Hex:
				if got := pathfold.ValidSHA1Hex(tt.input); got != tt.wantBool {
					t.Fatalf("ValidSHA1Hex(%q) = %v, want %v", tt.input, got, tt.wantBool)
				}
			default:
				t.Fatalf("unknown operation %d", tt.op)
			}
		})
	}
}
