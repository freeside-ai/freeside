package verify

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// trustedRecipeBytes is the §5.12 first-repository recipe in the
// package's JSON wire form; fixtures across the suite reuse it so
// digests stay stable.
const trustedRecipeBytes = `{"commands": ["go test ./...", "go vet ./..."], "capture": "none"}`

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
		{"unknown field", `{"commands": ["true"], "capture": "none", "extra": 1}`},
		// Trailing-byte enumeration: a value followed by any stray
		// bytes, including a bare closing delimiter that json.Decoder's
		// More() does not see at top level.
		{"trailing object", `{"commands": ["true"], "capture": "none"} {}`},
		{"trailing close bracket", `{"commands": ["true"], "capture": "none"}]`},
		{"trailing close brace", `{"commands": ["true"], "capture": "none"}}`},
		{"trailing comma", `{"commands": ["true"], "capture": "none"},`},
		{"trailing word", `{"commands": ["true"], "capture": "none"} trailing`},
		{"trailing array", `{"commands": ["true"], "capture": "none"}[1]`},
		{"leading array wrap", `[{"commands": ["true"], "capture": "none"}]`},
		{"commands missing", `{"capture": "none"}`},
		{"commands empty", `{"commands": [], "capture": "none"}`},
		{"commands wrong type", `{"commands": "true", "capture": "none"}`},
		{"command empty", `{"commands": [""], "capture": "none"}`},
		{"command whitespace only", `{"commands": [" \t "], "capture": "none"}`},
		{"capture missing", `{"commands": ["true"]}`},
		{"capture invalid", `{"commands": ["true"], "capture": "screenshots"}`},
		{"capture case variant", `{"commands": ["true"], "capture": "None"}`},
		{"control character", "{\"commands\": [\"go\\u0007test\"], \"capture\": \"none\"}"},
		{"delete character", "{\"commands\": [\"go\\u007ftest\"], \"capture\": \"none\"}"},
	}
	// Every rejected shell metacharacter, enumerated so a future edit to
	// the set cannot silently drop one.
	for _, m := range shellMeta {
		raw, err := json.Marshal(map[string]any{
			"commands": []string{"go test " + string(m) + " x"},
			"capture":  "none",
		})
		if err != nil {
			t.Fatalf("build metacharacter case: %v", err)
		}
		cases = append(cases, struct {
			name string
			raw  string
		}{fmt.Sprintf("metacharacter %q", m), string(raw)})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRecipe([]byte(tc.raw)); !errors.Is(err, ErrRecipeInvalid) {
				t.Fatalf("ParseRecipe(%q) = %v, want ErrRecipeInvalid", tc.raw, err)
			}
		})
	}
}

// TestRecipeDigestBindsExactBytes pins that the digest is over the raw
// bytes as loaded: a semantically identical recipe with different
// whitespace is a different digest, so approvals can never alias.
func TestRecipeDigestBindsExactBytes(t *testing.T) {
	a := RecipeDigest([]byte(`{"commands":["true"],"capture":"none"}`))
	b := RecipeDigest([]byte(`{"commands": ["true"], "capture": "none"}`))
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
