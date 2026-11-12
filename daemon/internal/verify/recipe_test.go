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
	t.Parallel()
	r, err := ParseRecipe([]byte(trustedRecipeBytes))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	type command struct {
		Argv    []string       `json:"argv"`
		Room    ExecutionClass `json:"room"`
		History bool           `json:"history"`
	}
	commands := make([]command, 0, len(r.Commands))
	for _, c := range r.Commands {
		commands = append(commands, command{c.Argv, c.Class, c.History})
	}
	doc := struct {
		Commands []command   `json:"commands"`
		Capture  CaptureMode `json:"capture"`
		Digest   string      `json:"digest"`
	}{commands, r.Capture, string(RecipeDigest([]byte(trustedRecipeBytes)))}
	got, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden.Assert(t, "recipe_parsed", append(got, '\n'))
}

func TestParseRecipeRejects(t *testing.T) {
	t.Parallel()
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
		{"number command", `{"commands": [7], "capture": "none"}`},
		// The object form names its argv and room explicitly; nothing
		// about it is defaulted.
		{"object unknown room", `{"commands": [{"argv": ["true"], "room": "linux_vm"}], "capture": "none"}`},
		{"object room case variant", `{"commands": [{"argv": ["true"], "room": "MACOS_HOST"}], "capture": "none"}`},
		{"object empty room", `{"commands": [{"argv": ["true"], "room": ""}], "capture": "none"}`},
		{"object missing room", `{"commands": [{"argv": ["true"]}], "capture": "none"}`},
		{"object null room", `{"commands": [{"argv": ["true"], "room": null}], "capture": "none"}`},
		{"object room not a string", `{"commands": [{"argv": ["true"], "room": 1}], "capture": "none"}`},
		{"object missing argv", `{"commands": [{"room": "macos_host"}], "capture": "none"}`},
		{"object null argv", `{"commands": [{"argv": null, "room": "macos_host"}], "capture": "none"}`},
		{"object empty argv", `{"commands": [{"argv": [], "room": "macos_host"}], "capture": "none"}`},
		{"object argv a string", `{"commands": [{"argv": "true", "room": "macos_host"}], "capture": "none"}`},
		{"object null token", `{"commands": [{"argv": ["swift", null], "room": "macos_host"}], "capture": "none"}`},
		{"object unknown field", `{"commands": [{"argv": ["true"], "room": "macos_host", "env": {}}], "capture": "none"}`},
		{"object case-folded duplicate room", `{"commands": [{"argv": ["true"], "room": "project_image", "Room": "macos_host"}], "capture": "none"}`},
		{"object duplicate room", `{"commands": [{"argv": ["true"], "room": "project_image", "room": "macos_host"}], "capture": "none"}`},
		{"object shell command string", `{"commands": [{"argv": ["sh", "-c", "true"], "room": "macos_host"}], "capture": "none"}`},
		{"object dotdot token", `{"commands": [{"argv": ["../escape.sh"], "room": "macos_host"}], "capture": "none"}`},
		// History is a boolean asked for on a project_image command only:
		// the macOS slot helper can't pass the range's SHAs.
		{"history on macos_host", `{"commands": [{"argv": ["true"], "room": "macos_host", "history": true}], "capture": "none"}`},
		{"history null", `{"commands": [{"argv": ["true"], "room": "project_image", "history": null}], "capture": "none"}`},
		{"history string", `{"commands": [{"argv": ["true"], "room": "project_image", "history": "true"}], "capture": "none"}`},
		{"history number", `{"commands": [{"argv": ["true"], "room": "project_image", "history": 1}], "capture": "none"}`},
		{"history object", `{"commands": [{"argv": ["true"], "room": "project_image", "history": {}}], "capture": "none"}`},
		{"history duplicate", `{"commands": [{"argv": ["true"], "room": "project_image", "history": false, "history": true}], "capture": "none"}`},
		{"history case-folded duplicate", `{"commands": [{"argv": ["true"], "room": "project_image", "history": false, "History": true}], "capture": "none"}`},
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
	t.Parallel()
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
	if !reflect.DeepEqual(r.CommandsIn(ClassProjectImage), want) {
		t.Fatalf("Commands = %#v, want %#v", r.Commands, want)
	}
}

// TestParseRecipeExecutionClasses pins the two command forms: a bare
// argv array runs in the project image, and an object names its room.
// Whitespace before a command does not change how its form is read.
func TestParseRecipeExecutionClasses(t *testing.T) {
	t.Parallel()
	raw := `{"commands": [` +
		`["bash", "scripts/check.sh", "app", "linux"], ` +
		`{"argv": ["bash", "scripts/check.sh", "app", "test"], "room": "macos_host"},` +
		"\n\t" + `{"room": "project_image", "argv": ["go", "vet", "./..."]}` +
		`], "capture": "none"}`
	r, err := ParseRecipe([]byte(raw))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	want := []Command{
		{Argv: []string{"bash", "scripts/check.sh", "app", "linux"}, Class: ClassProjectImage},
		{Argv: []string{"bash", "scripts/check.sh", "app", "test"}, Class: ClassMacOSHost},
		{Argv: []string{"go", "vet", "./..."}, Class: ClassProjectImage},
	}
	if !reflect.DeepEqual(r.Commands, want) {
		t.Fatalf("Commands = %#v, want %#v", r.Commands, want)
	}
	if got := r.CommandsIn(ClassMacOSHost); !reflect.DeepEqual(got, [][]string{want[1].Argv}) {
		t.Errorf("CommandsIn(macos_host) = %#v", got)
	}
	if got := r.CommandsIn(ClassProjectImage); !reflect.DeepEqual(got, [][]string{want[0].Argv, want[2].Argv}) {
		t.Errorf("CommandsIn(project_image) = %#v", got)
	}
	// A macos_host command's script is a verification-control path like
	// any other: CommandPaths covers every class.
	if got := r.CommandPaths(); !reflect.DeepEqual(got, []string{"scripts/check.sh"}) {
		t.Errorf("CommandPaths = %#v", got)
	}
}

// TestParseRecipeHistory pins the history field: only the object form can
// ask, a false or absent value asks for nothing, macos_host may say false,
// and the image proof's command list leaves a history command out while
// CommandsIn keeps it.
func TestParseRecipeHistory(t *testing.T) {
	t.Parallel()
	raw := `{"commands": [` +
		`["go", "test", "./..."], ` +
		`{"argv": ["bash", "scripts/check-commit-messages.sh"], "room": "project_image", "history": true}, ` +
		`{"argv": ["go", "vet", "./..."], "room": "project_image", "history": false}, ` +
		`{"argv": ["bash", "scripts/check.sh", "app", "test"], "room": "macos_host", "history": false}` +
		`], "capture": "none"}`
	r, err := ParseRecipe([]byte(raw))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	want := []Command{
		{Argv: []string{"go", "test", "./..."}, Class: ClassProjectImage},
		{Argv: []string{"bash", "scripts/check-commit-messages.sh"}, Class: ClassProjectImage, History: true},
		{Argv: []string{"go", "vet", "./..."}, Class: ClassProjectImage},
		{Argv: []string{"bash", "scripts/check.sh", "app", "test"}, Class: ClassMacOSHost},
	}
	if !reflect.DeepEqual(r.Commands, want) {
		t.Fatalf("Commands = %#v, want %#v", r.Commands, want)
	}
	if got := r.ProofCommands(); !reflect.DeepEqual(got, [][]string{want[0].Argv, want[2].Argv}) {
		t.Errorf("ProofCommands = %#v, want the project_image commands without history", got)
	}
	if got := r.CommandsIn(ClassProjectImage); !reflect.DeepEqual(got, [][]string{want[0].Argv, want[1].Argv, want[2].Argv}) {
		t.Errorf("CommandsIn(project_image) = %#v, want every project_image command", got)
	}
}

// TestParseRecipeRejectsShellCommandString pins #154: a shell -c token
// introduces a second parser whose command string can hide the real
// repository entrypoint from CommandPaths. Direct shell-script argv
// without a shell command-string option remains valid, as do opaque
// multi-word operands for non-shell tools.
func TestParseRecipeRejectsShellCommandString(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	a := RecipeDigest([]byte(`{"commands":[["true"]],"capture":"none"}`))
	b := RecipeDigest([]byte(`{"commands": [["true"]], "capture": "none"}`))
	if a == b {
		t.Fatal("distinct byte forms produced the same recipe digest")
	}
}

func TestCaptureModeValidity(t *testing.T) {
	t.Parallel()
	for _, m := range AllCaptureModes {
		if !m.valid() {
			t.Errorf("registered capture mode %q reports invalid", m)
		}
	}
	if CaptureMode("").valid() {
		t.Error("zero capture mode reports valid")
	}
}

func TestExecutionClassValidity(t *testing.T) {
	t.Parallel()
	for _, c := range AllExecutionClasses {
		if !c.valid() {
			t.Errorf("registered execution class %q reports invalid", c)
		}
	}
	if ExecutionClass("").valid() {
		t.Error("zero execution class reports valid")
	}
}

// imageRecipe builds a recipe whose commands all run in the project
// image, the shape every recipe had before execution classes.
func imageRecipe(argvs [][]string) Recipe {
	r := Recipe{Capture: CaptureNone}
	for _, argv := range argvs {
		r.Commands = append(r.Commands, Command{Argv: argv, Class: ClassProjectImage})
	}
	return r
}

func TestRecipeSourceValidity(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
			got := imageRecipe(tc.cmds).CommandPaths()
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
