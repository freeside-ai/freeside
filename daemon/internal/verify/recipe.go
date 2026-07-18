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
	"unicode"

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
// Commands are argument vectors given verbatim on the wire
// ([["go", "test", "./..."]]): no shell, no splitting, arguments
// opaque. An argument may therefore hold a space or any metacharacter
// (`xcodebuild -destination 'generic/platform=iOS Simulator'`, a regex)
// as one element without smuggling chaining, substitution, or
// redirection past the no-shell execution path, which never sees the
// argument as anything but an opaque execve token.
type Recipe struct {
	Commands [][]string
	Capture  CaptureMode
}

// recipeWire is the recipe's JSON wire form, e.g.
// {"commands": [["go", "test", "./..."], ["go", "vet", "./..."]],
// "capture": "none"}. Each command is an explicit argument vector, not
// a shell string: an argv element carries its spaces and metacharacters
// verbatim, and the parser neither splits nor rewrites it.
// JSON rather than the plan's §5.12 YAML config syntax: the daemon
// carries no YAML dependency and the control-plane config format is not
// yet initialized; the decision note records the revisit condition.
//
// Tokens are *string, not string, so the parser can tell a JSON null
// (nil) from an intentional empty string (a non-nil pointer to ""):
// json unmarshals null into a string's zero value, which would let a
// malformed `["swift", "test", null]` masquerade as a valid empty
// argument, so a null token fails closed rather than executing.
type recipeWire struct {
	Commands [][]*string `json:"commands"`
	Capture  string      `json:"capture"`
}

// ParseRecipe parses and validates trusted recipe bytes. Unknown
// fields, trailing data, an empty command list, an empty command (no
// argv or an empty executable name), a null or NUL-bearing token, a
// ".." path segment in a token, and an invalid capture mode all fail
// closed with ErrRecipeInvalid. Command arguments are otherwise opaque:
// no shell, no whitespace splitting, no metacharacter rejection.
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
	for _, rawArgv := range w.Commands {
		argv := make([]string, 0, len(rawArgv))
		for _, tok := range rawArgv {
			if tok == nil {
				return Recipe{}, fmt.Errorf("recipe command carries a null token: %w", ErrRecipeInvalid)
			}
			argv = append(argv, *tok)
		}
		if err := validateCommand(argv); err != nil {
			return Recipe{}, err
		}
		r.Commands = append(r.Commands, argv)
	}
	if !r.Capture.valid() {
		return Recipe{}, fmt.Errorf("recipe capture mode %q: %w", w.Capture, ErrRecipeInvalid)
	}
	return r, nil
}

// validateCommand fails closed on a malformed argument vector. A recipe
// is trusted, so these guard against an authoring mistake or an
// adversarial recipe, never candidate input: an empty vector or empty
// executable name has nothing to run; a NUL byte cannot cross execve
// and would otherwise surface as an opaque runtime error; and a ".."
// path segment in any token is rejected before path.Clean can collapse
// it, because a collapsed token (`./link/../verify.sh` -> `verify.sh`)
// makes CommandPaths and the symlink-entrypoint guard record and check
// a different path than the OS resolves and executes. Arguments are
// otherwise opaque: spaces and shell metacharacters are legal, since
// the runner passes each element to execve verbatim, never to a shell.
func validateCommand(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("recipe declares an empty command: %w", ErrRecipeInvalid)
	}
	for _, tok := range argv {
		if strings.ContainsRune(tok, 0) {
			return fmt.Errorf("command token %q carries a NUL byte: %w", tok, ErrRecipeInvalid)
		}
		for _, seg := range strings.Split(tok, "/") {
			if seg == ".." {
				return fmt.Errorf("command token %q carries a %q path segment: %w", tok, "..", ErrRecipeInvalid)
			}
		}
	}
	return nil
}

// CommandPaths returns the repo-relative file paths the recipe's
// commands reference: a script entrypoint (`./scripts/verify.sh`) or a
// script argument (`bash scripts/verify.sh`). These are
// verification-control surfaces, because a candidate that rewrites one
// changes what the trusted recipe actually runs. Bare command names
// (`go`, `bash`) resolve through the room's PATH to its toolchain, not
// a candidate file, and are excluded; so are absolute paths, Go's `...`
// package patterns, and whitespace-bearing tokens (a multi-word argv
// operand such as `-destination "generic/platform=iOS Simulator"`, not
// a single filename; see repoRelPath). Paths are returned slash-clean
// with any leading `./` stripped, matching the change account's path
// space.
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
// `./internal/...`), and carries no whitespace. Whitespace is the one
// content rule: opaque argv packs a multi-word operand into a single
// token (`generic/platform=iOS Simulator`, a `sh -c` script string
// `scripts/verify.sh --fast`), so a token bearing any space, tab, or
// newline is an operand, not one filename, and treating it as a repo
// path both over-flags the operand and, when a prefix segment collides
// with a repo symlink, spuriously trips the symlink-entrypoint guard.
// A verification entrypoint path with embedded whitespace is therefore
// not supported; the recipe author names it without spaces. This leaves
// one latent residual: a `sh -c "..."` recipe hides its real entrypoint
// inside the whitespace-bearing string, so a candidate edit to that
// script goes unflagged (an under-flag). No current recipe uses a shell
// runner, so it stays latent; the decision note carries the follow-up.
// Otherwise no character in the filename disqualifies
// it: the no-shell runner executes the literal name, so a glob
// metacharacter, a colon, three embedded dots (`check...sh`), or a
// trailing dot is all part of a real file the candidate could tamper
// with; the downstream match is exact and literal, never a glob or
// alias fold. The path is path.Clean-normalized so an unclean but valid
// spelling the OS still resolves (`scripts/./verify.sh`,
// `scripts//verify.sh`) matches the canonical changed path rather than
// slipping the flag.
func repoRelPath(tok string) (string, bool) {
	if !strings.Contains(tok, "/") || strings.HasPrefix(tok, "/") ||
		strings.ContainsFunc(tok, unicode.IsSpace) {
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
