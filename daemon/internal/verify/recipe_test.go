package verify

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// trustedRecipeBytes is the §5.12 first-repository recipe in the
// package's JSON wire form (explicit argv arrays); fixtures across the
// suite reuse it so digests stay stable.
const trustedRecipeBytes = `{"commands": [["go", "test", "./..."], ["go", "vet", "./..."]], "capture": "none"}`

func TestParseRecipeGolden(t *testing.T) {
	r, err := ParseRecipe([]byte(trustedRecipeBytes))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	doc := struct {
		Commands [][]string  `json:"commands"`
		Capture  CaptureMode `json:"capture"`
		Digest   string      `json:"digest"`
	}{r.Commands, r.Capture, string(RecipeDigest([]byte(trustedRecipeBytes)))}
	got, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden.Assert(t, "recipe_parsed", append(got, '\n'))
}

func TestParseRecipeRejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty bytes", ``},
		{"null", `null`},
		{"unknown field", `{"commands": [["true"]], "capture": "none", "extra": 1}`},
		// Trailing-byte enumeration: a value followed by any stray
		// bytes, including a bare closing delimiter that json.Decoder's
		// More() does not see at top level.
		{"trailing object", `{"commands": [["true"]], "capture": "none"} {}`},
		{"trailing close bracket", `{"commands": [["true"]], "capture": "none"}]`},
		{"trailing close brace", `{"commands": [["true"]], "capture": "none"}}`},
		{"trailing comma", `{"commands": [["true"]], "capture": "none"},`},
		{"trailing word", `{"commands": [["true"]], "capture": "none"} trailing`},
		{"trailing array", `{"commands": [["true"]], "capture": "none"}[1]`},
		{"leading array wrap", `[{"commands": [["true"]], "capture": "none"}]`},
		{"commands missing", `{"capture": "none"}`},
		{"commands empty", `{"commands": [], "capture": "none"}`},
		{"commands wrong type", `{"commands": "true", "capture": "none"}`},
		// A command must be an argv array, never a bare string: the
		// whitespace-split wire form is gone.
		{"command not an array", `{"commands": ["go test"], "capture": "none"}`},
		{"command empty argv", `{"commands": [[]], "capture": "none"}`},
		{"command empty executable", `{"commands": [[""]], "capture": "none"}`},
		{"capture missing", `{"commands": [["true"]]}`},
		{"capture invalid", `{"commands": [["true"]], "capture": "screenshots"}`},
		{"capture case variant", `{"commands": [["true"]], "capture": "None"}`},
		// A NUL byte cannot cross execve; rejected at parse rather than
		// surfacing as an opaque runtime error.
		{"nul byte in token", "{\"commands\": [[\"go\\u0000test\"]], \"capture\": \"none\"}"},
		// A JSON null token unmarshals into the zero string; rejected so
		// malformed recipe JSON cannot masquerade as an empty argument.
		{"null argument token", `{"commands": [["swift", "test", null]], "capture": "none"}`},
		{"null executable token", `{"commands": [[null]], "capture": "none"}`},
		{"null command", `{"commands": [null], "capture": "none"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRecipe([]byte(tc.raw)); !errors.Is(err, ErrRecipeInvalid) {
				t.Fatalf("ParseRecipe(%q) = %v, want ErrRecipeInvalid", tc.raw, err)
			}
		})
	}
}

// TestParseRecipeArgvOpaque pins acceptance item 1: an argv element with
// a space (an xcodebuild destination) or a shell metacharacter survives
// parse verbatim as one element. Nothing is split, folded, or rewritten,
// so the destination reaches the room as a single argument.
func TestParseRecipeArgvOpaque(t *testing.T) {
	raw := `{"commands": [` +
		`["xcodebuild", "-destination", "generic/platform=iOS Simulator"], ` +
		`["grep", "-E", "a|b > c", "."]` +
		`], "capture": "none"}`
	r, err := ParseRecipe([]byte(raw))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	want := [][]string{
		{"xcodebuild", "-destination", "generic/platform=iOS Simulator"},
		{"grep", "-E", "a|b > c", "."},
	}
	if !reflect.DeepEqual(r.Commands, want) {
		t.Fatalf("Commands = %#v, want %#v", r.Commands, want)
	}
}

// TestParseRecipeRejectsShellCommandString pins #154: a shell -c token
// introduces a second parser whose command string can hide the real
// repository entrypoint from CommandPaths. Direct shell-script argv
// without a shell command-string option remains valid, as do opaque
// multi-word operands for non-shell tools.
func TestParseRecipeRejectsShellCommandString(t *testing.T) {
	reject := [][]string{
		{"sh", "-c", "./scripts/verify.sh --fast"},
		{"/bin/sh", "-ec", "./scripts/verify.sh --fast"},
		{"bash", "-xc", "./scripts/verify.sh --fast"},
		{"dash", "-c", "./scripts/verify.sh --fast"},
		{"zsh", "-c", "./scripts/verify.sh --fast"},
		{"fish", "-c", "./scripts/verify.fish --fast"},
		{"fish", "-C", "./scripts/verify.fish --fast"},
		{"fish", "--command=./scripts/verify.fish --fast"},
		{"fish", "--init-command", "./scripts/verify.fish --fast"},
		// Deliberate fail-closed over-rejection: do not parse shell
		// option ordering to decide whether the script receives -c.
		{"bash", "scripts/verify.sh", "-c", "fast"},
	}
	for _, argv := range reject {
		t.Run("reject/"+argv[0]+"/"+argv[1], func(t *testing.T) {
			raw := marshalRecipe(t, [][]string{argv})
			if _, err := ParseRecipe(raw); !errors.Is(err, ErrRecipeInvalid) {
				t.Fatalf("ParseRecipe(%s) = %v, want ErrRecipeInvalid", raw, err)
			}
		})
	}

	keep := []struct {
		name string
		argv []string
	}{
		{"direct shell script", []string{"bash", "scripts/verify.sh", "--fast"}},
		{"direct executable with c flag", []string{"./scripts/verify.sh", "-c", "fast"}},
		{"non-shell c option", []string{"python3", "-c", "print('a/b c')"}},
		{"multi-word operand", []string{"xcodebuild", "-destination", "generic/platform=iOS Simulator"}},
	}
	for _, tc := range keep {
		t.Run("keep/"+tc.name, func(t *testing.T) {
			raw := marshalRecipe(t, [][]string{tc.argv})
			if _, err := ParseRecipe(raw); err != nil {
				t.Fatalf("ParseRecipe(%s) = %v, want nil", raw, err)
			}
		})
	}
}

// TestParseRecipeRejectsDotDotToken is the adversarial enumeration of the
// ".." path-segment input space in command tokens (#140 hardening): a
// ".." segment that path.Clean would collapse desyncs CommandPaths and
// the symlink-entrypoint guard from the file the OS resolves and
// executes, so any ".." *segment* fails closed at parse regardless of
// position, depth, or surrounding tokens, while a ".." that is only part
// of a real filename must survive as an opaque argument.
func TestParseRecipeRejectsDotDotToken(t *testing.T) {
	reject := []string{
		"..",
		"../x",
		"a/..",
		"a/../b",
		"./link/../verify.sh",
		"../../etc/passwd",
		"scripts/../../x.sh",
	}
	for _, tok := range reject {
		t.Run("reject/"+tok, func(t *testing.T) {
			raw := marshalRecipe(t, [][]string{{"bash", tok}})
			if _, err := ParseRecipe(raw); !errors.Is(err, ErrRecipeInvalid) {
				t.Fatalf("ParseRecipe(%s) = %v, want ErrRecipeInvalid", raw, err)
			}
		})
	}
	// A ".." that is not a whole path segment is a real filename (or an
	// opaque argument) and must not be swept up by the segment check.
	keep := []string{"a..b", "..bar", "foo..", "scripts/ok..sh"}
	for _, tok := range keep {
		t.Run("keep/"+tok, func(t *testing.T) {
			raw := marshalRecipe(t, [][]string{{"bash", tok}})
			if _, err := ParseRecipe(raw); err != nil {
				t.Fatalf("ParseRecipe(%s) = %v, want nil (real filename)", raw, err)
			}
		})
	}
}

// marshalRecipe builds recipe wire bytes for the given commands with a
// valid capture mode.
func marshalRecipe(t *testing.T, commands [][]string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"commands": commands, "capture": "none"})
	if err != nil {
		t.Fatalf("marshal recipe: %v", err)
	}
	return raw
}

// TestRecipeDigestBindsExactBytes pins that the digest is over the raw
// bytes as loaded: a semantically identical recipe with different
// whitespace is a different digest, so approvals can never alias.
func TestRecipeDigestBindsExactBytes(t *testing.T) {
	a := RecipeDigest([]byte(`{"commands":[["true"]],"capture":"none"}`))
	b := RecipeDigest([]byte(`{"commands": [["true"]], "capture": "none"}`))
	if a == b {
		t.Fatal("distinct byte forms produced the same recipe digest")
	}
}

func TestCaptureModeValidity(t *testing.T) {
	for _, m := range AllCaptureModes {
		if !m.valid() {
			t.Errorf("registered capture mode %q reports invalid", m)
		}
	}
	if CaptureMode("").valid() {
		t.Error("zero capture mode reports valid")
	}
}

func TestRecipeSourceValidity(t *testing.T) {
	if (RecipeSource{}).valid() {
		t.Error("zero recipe source reports valid")
	}
	if ConfigRecipe(nil).valid() {
		t.Error("config source with nil bytes reports valid")
	}
	if !ConfigRecipe([]byte(trustedRecipeBytes)).valid() {
		t.Error("config source with bytes reports invalid")
	}
	if !BaseCommitRecipe().valid() {
		t.Error("base-commit source reports invalid")
	}
}

// TestRecipeCommandPaths pins which command tokens count as
// verification-control surfaces: repo-relative script entrypoints and
// script arguments, but not PATH toolchains, package patterns, flags,
// or absolute paths.
func TestRecipeCommandPaths(t *testing.T) {
	cases := []struct {
		name string
		cmds [][]string
		want []string
	}{
		{"go toolchain", [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}}, nil},
		{"local entrypoint", [][]string{{"./scripts/verify.sh"}}, []string{"scripts/verify.sh"}},
		{"bare-relative entrypoint", [][]string{{"scripts/verify.sh"}}, []string{"scripts/verify.sh"}},
		{"interpreter plus script", [][]string{{"bash", "tools/check.sh", "--fast"}}, []string{"tools/check.sh"}},
		{"unclean dot path", [][]string{{"bash", "scripts/./verify.sh"}}, []string{"scripts/verify.sh"}},
		{"unclean double slash", [][]string{{"bash", "scripts//verify.sh"}}, []string{"scripts/verify.sh"}},
		{"dot-prefixed", [][]string{{"./scripts/verify.sh"}}, []string{"scripts/verify.sh"}},
		{"absolute path excluded", [][]string{{"/usr/bin/make", "check"}}, nil},
		{"package pattern excluded", [][]string{{"go", "build", "./internal/..."}}, nil},
		{"dedup", [][]string{{"bash", "scripts/a.sh"}, {"bash", "scripts/a.sh"}}, []string{"scripts/a.sh"}},
		{"glob-metachar filename kept", [][]string{{"./scripts/check[fast].sh"}}, []string{"scripts/check[fast].sh"}},
		{"embedded-ellipsis filename kept", [][]string{{"./scripts/check...sh"}}, []string{"scripts/check...sh"}},
		{"colon filename kept", [][]string{{"./scripts/check:fast.sh"}}, []string{"scripts/check:fast.sh"}},
		{"package pattern segment excluded", [][]string{{"go", "test", "./..."}}, nil},
		{"nested package pattern excluded", [][]string{{"go", "build", "./internal/..."}}, nil},
		// Whitespace-bearing operands are not single filenames (#149): a
		// multi-word argv operand or shell-shaped command string carries a
		// space and must not be read as a repo path, so it neither
		// over-flags nor trips the symlink-entrypoint guard. Parsed recipes
		// cannot contain the latter (#154); this direct-value case keeps
		// CommandPaths' lexical contract pinned independently.
		{"destination operand excluded", [][]string{{"xcodebuild", "-destination", "generic/platform=iOS Simulator"}}, nil},
		{"shell-shaped string excluded", [][]string{{"sh", "-c", "./scripts/verify.sh --fast"}}, nil},
		{"tab-bearing operand excluded", [][]string{{"grep", "-rn", "a/b\tc"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Recipe{Commands: tc.cmds}.CommandPaths()
			if len(got) != len(tc.want) {
				t.Fatalf("CommandPaths() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("CommandPaths() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
