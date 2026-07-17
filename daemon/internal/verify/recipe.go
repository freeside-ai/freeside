package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// CaptureMode is a recipe's evidence-capture declaration (§5.15: evidence
// is captured per the recipe's capture block). The zero value "" is
// invalid by design: a trusted recipe declares its capture mode
// explicitly, absence is never read as "none".
type CaptureMode string

// CaptureNone declares a recipe that captures no additional evidence
// files. The verifier's own account (report and transcript) is still
// produced; the first repository's recipes use this mode (§11).
const CaptureNone CaptureMode = "none"

// AllCaptureModes lists every valid CaptureMode: the single place a new
// mode is registered, driving the table-driven tests.
var AllCaptureModes = []CaptureMode{CaptureNone}

// valid is the validity predicate; as a predicate it uses default.
func (m CaptureMode) valid() bool {
	switch m {
	case CaptureNone:
		return true
	default:
		return false
	}
}

// Recipe is a parsed trusted verification recipe: the commands the
// clean workspace runs and the capture block governing evidence.
// Commands are argument vectors, never shell text: the wire form's
// command strings are whitespace-split and any shell metacharacter is
// rejected at parse, so no recipe can smuggle chaining, substitution,
// or redirection past the no-shell execution path.
type Recipe struct {
	Commands [][]string
	Capture  CaptureMode
}

// recipeWire is the recipe's JSON wire form, e.g.
// {"commands": ["go test ./...", "go vet ./..."], "capture": "none"}.
// JSON rather than the plan's §5.12 YAML config syntax: the daemon
// carries no YAML dependency and the control-plane config format is not
// yet initialized; the decision note records the revisit condition.
type recipeWire struct {
	Commands []string `json:"commands"`
	Capture  string   `json:"capture"`
}

// shellMeta are the rejected shell metacharacters. The verifier never
// invokes a shell, so these have no function in an honest recipe; their
// presence means the recipe was written against shell semantics the
// execution path does not provide, and it fails closed at parse.
const shellMeta = "|&;<>()$`\\\"'"

// ParseRecipe parses and validates trusted recipe bytes. Unknown
// fields, trailing data, an empty command list, an empty command, a
// shell metacharacter, a control character, and an invalid capture mode
// all fail closed with ErrRecipeInvalid.
func ParseRecipe(raw []byte) (Recipe, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var w recipeWire
	if err := dec.Decode(&w); err != nil {
		return Recipe{}, fmt.Errorf("recipe does not parse: %w: %w", err, ErrRecipeInvalid)
	}
	// Require EOF after the one value. dec.More() reports another
	// element only in the current array/object context, so at top level
	// it misses a stray trailing token (`{...}]`, `{...}}`); a second
	// decode that must return io.EOF rejects every trailing byte, which
	// matters because this parser is the fail-closed boundary for
	// trusted config and base-commit recipe bytes.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return Recipe{}, fmt.Errorf("recipe carries trailing data: %w", ErrRecipeInvalid)
	}
	if len(w.Commands) == 0 {
		return Recipe{}, fmt.Errorf("recipe declares no commands: %w", ErrRecipeInvalid)
	}
	r := Recipe{
		Commands: make([][]string, 0, len(w.Commands)),
		Capture:  CaptureMode(w.Capture),
	}
	for _, c := range w.Commands {
		argv, err := splitCommand(c)
		if err != nil {
			return Recipe{}, err
		}
		r.Commands = append(r.Commands, argv)
	}
	if !r.Capture.valid() {
		return Recipe{}, fmt.Errorf("recipe capture mode %q: %w", w.Capture, ErrRecipeInvalid)
	}
	return r, nil
}

// splitCommand turns one wire command string into an argument vector.
func splitCommand(c string) ([]string, error) {
	for _, r := range c {
		if strings.ContainsRune(shellMeta, r) {
			return nil, fmt.Errorf("command %q carries shell metacharacter %q: %w", c, r, ErrRecipeInvalid)
		}
		if r < 0x20 || r == 0x7f {
			return nil, fmt.Errorf("command %q carries a control character: %w", c, ErrRecipeInvalid)
		}
	}
	argv := strings.Fields(c)
	if len(argv) == 0 {
		return nil, fmt.Errorf("recipe declares an empty command: %w", ErrRecipeInvalid)
	}
	return argv, nil
}

// CommandPaths returns the repo-relative file paths the recipe's
// commands reference: a script entrypoint (`./scripts/verify.sh`) or a
// script argument (`bash scripts/verify.sh`). These are
// verification-control surfaces, because a candidate that rewrites one
// changes what the trusted recipe actually runs. Bare command names
// (`go`, `bash`) resolve through the room's PATH to its toolchain, not
// a candidate file, and are excluded; so are absolute paths, Go's `...`
// package patterns, and glob-bearing tokens. Paths are returned
// slash-clean with any leading `./` stripped, matching the change
// account's path space.
func (r Recipe) CommandPaths() []string {
	seen := map[string]bool{}
	var paths []string
	for _, argv := range r.Commands {
		for _, tok := range argv {
			p, ok := repoRelPath(tok)
			if ok && !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// repoRelPath reports whether a command token names a repo-relative
// file and returns its cleaned path, matching the change account's path
// space. A token qualifies only if it carries a path separator (a bare
// name is a PATH lookup), is not absolute, and is not the Go recursive
// package pattern (a path segment equal to `...`, as in `./...` or
// `./internal/...`). No character in the filename disqualifies it: the
// no-shell runner executes the literal name, so a glob metacharacter, a
// colon, three embedded dots (`check...sh`), or a trailing dot is all
// part of a real file the candidate could tamper with; the downstream
// match is exact and literal, never a glob or alias fold. The path is
// path.Clean-normalized so an unclean but valid spelling the OS still
// resolves (`scripts/./verify.sh`, `scripts//verify.sh`) matches the
// canonical changed path rather than slipping the flag.
func repoRelPath(tok string) (string, bool) {
	if !strings.Contains(tok, "/") || strings.HasPrefix(tok, "/") {
		return "", false
	}
	p := path.Clean(tok)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	// Exclude only the Go recursive package pattern: a `...` path
	// *segment*, not a filename that merely contains three dots.
	for _, seg := range strings.Split(p, "/") {
		if seg == "..." {
			return "", false
		}
	}
	return p, true
}

// RecipeDigest is the content address approvals bind to (§5.12): sha256
// over the exact trusted bytes as loaded, never a canonical
// re-encoding, so two byte forms can never alias one approved digest.
func RecipeDigest(raw []byte) domain.Digest {
	sum := sha256.Sum256(raw)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// RecipeSource names where the trusted recipe bytes load from (§5.8):
// approved control-plane config bytes the caller snapshotted, or the
// trusted base commit in the daemon-owned checkout. Candidate-head
// content is never a source; a workspace copy of the recipe is data.
// The zero value is invalid by design: a caller must choose a source
// explicitly.
type RecipeSource struct {
	raw      []byte
	fromBase bool
}

// ConfigRecipe is the approved control-plane config source: raw is the
// recipe's exact approved bytes.
func ConfigRecipe(raw []byte) RecipeSource { return RecipeSource{raw: raw} }

// BaseCommitRecipe is the trusted base commit source: the verifier
// loads the recipe blob at the enforced base SHA.
func BaseCommitRecipe() RecipeSource { return RecipeSource{fromBase: true} }

// valid is the validity predicate: exactly one source is set.
func (s RecipeSource) valid() bool { return s.fromBase != (s.raw != nil) }
