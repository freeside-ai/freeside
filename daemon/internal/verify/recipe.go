package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
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

// ExecutionClass names the room a recipe command runs in (§5.7): the
// Linux project image or the credential-free macOS verification
// executor. The recipe, not the caller, chooses each command's class,
// and the class is part of the bytes the recipe digest covers, so
// moving a command to another room is a recipe change that needs
// reapproval. The zero value "" is invalid by design.
type ExecutionClass string

const (
	// ClassProjectImage runs a command in the repository's project
	// image: the room every recipe ran in before classes existed, and
	// the class of a command given in the bare argv form.
	ClassProjectImage ExecutionClass = "project_image"
	// ClassMacOSHost runs a command on the macOS host's credential-free
	// verification executor.
	ClassMacOSHost ExecutionClass = "macos_host"
)

// AllExecutionClasses lists every valid ExecutionClass: the single
// place a new class is registered, driving the table-driven tests.
var AllExecutionClasses = []ExecutionClass{ClassProjectImage, ClassMacOSHost}

// valid is the validity predicate; as a predicate it uses default.
func (c ExecutionClass) valid() bool {
	switch c {
	case ClassProjectImage, ClassMacOSHost:
		return true
	default:
		return false
	}
}

// Command is one recipe command: its argument vector, the class of room
// that runs it, and whether it asks for the candidate's git history.
type Command struct {
	Argv  []string
	Class ExecutionClass
	// History asks the verifier to add a verifier-written .git holding
	// the candidate's commits to this command's workspace, and to export
	// the range's base and head SHAs (history.go). Only a project_image
	// command may ask; a command that doesn't keeps the exact-tree
	// workspace.
	History bool
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
	Commands []Command
	Capture  CaptureMode
}

// ProofCommands returns the argument vectors of the project_image
// commands the project image's offline proof can run, in recipe order:
// every project_image command that doesn't ask for history. The proof
// runs each command in a copy of one commit with no candidate, so it has
// no base and head to give a history command, which would fail there
// for want of them. Verification is where a history command first runs.
func (r Recipe) ProofCommands() [][]string {
	var out [][]string
	for _, cmd := range r.Commands {
		if cmd.Class == ClassProjectImage && !cmd.History {
			out = append(out, cmd.Argv)
		}
	}
	return out
}

// CommandsIn returns the argument vectors of the recipe's commands of
// one class, in recipe order.
func (r Recipe) CommandsIn(class ExecutionClass) [][]string {
	var out [][]string
	for _, cmd := range r.Commands {
		if cmd.Class == class {
			out = append(out, cmd.Argv)
		}
	}
	return out
}

// recipeWire is the recipe's JSON wire form, e.g.
// {"commands": [["go", "test", "./..."], {"argv": ["bash",
// "scripts/check.sh", "app", "test"], "room": "macos_host"}],
// "capture": "none"}. Each command is an explicit argument vector, not
// a shell string: an argv element carries its spaces and metacharacters
// verbatim, and the parser neither splits nor rewrites it. A command is
// either a bare argv array, which runs in the project image, or an
// object naming its argv and its room (commandObjectWire). The bare form
// is the only form recipes had before execution classes, so every
// earlier recipe keeps its bytes and therefore its digest and approval.
// JSON rather than the plan's §5.12 YAML config syntax: the daemon
// carries no YAML dependency and the control-plane config format is not
// yet initialized; the decision note records the revisit condition.
//
// Each command decodes in a second strictjson pass over its raw bytes
// (parseCommand), so both forms keep the outer decode's refusal of
// unknown fields and duplicate names.
type recipeWire struct {
	Commands []json.RawMessage `json:"commands"`
	Capture  string            `json:"capture"`
}

// commandObjectWire is a command's object form,
// {"argv": [...], "room": "macos_host"}. Both fields are required:
// a missing room is refused rather than defaulted, so the object form
// always says where its command runs. The optional "history" boolean
// asks for the candidate's git history; it is raw so a null is refused
// rather than read as absent.
//
// Tokens are *string, not string, so the parser can tell a JSON null
// (nil) from an intentional empty string (a non-nil pointer to ""):
// json unmarshals null into a string's zero value, which would let a
// malformed `["swift", "test", null]` masquerade as a valid empty
// argument, so a null token fails closed rather than executing. The
// same holds for the bare array form.
type commandObjectWire struct {
	Argv    []*string       `json:"argv"`
	Room    *string         `json:"room"`
	History json.RawMessage `json:"history"`
}

// ParseRecipe parses and validates trusted recipe bytes. Unknown
// fields, trailing data, an empty command list, a command that is
// neither an argv array nor a command object, a command object with no
// argv or no room or an unknown room, an empty command (no argv or an
// empty executable name), a null or NUL-bearing token, a ".." path
// segment in a token, a recognized direct shell runner carrying a
// command-string option, a "history" that isn't a boolean or is true on
// a macos_host command, and an invalid capture mode all fail closed with
// ErrRecipeInvalid. Command arguments are otherwise opaque: no
// whitespace splitting or metacharacter rejection.
func ParseRecipe(raw []byte) (Recipe, error) {
	var w recipeWire
	if err := strictjson.Decode(raw, &w, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return Recipe{}, fmt.Errorf("recipe carries trailing data: %w", ErrRecipeInvalid)
		}
		return Recipe{}, fmt.Errorf("recipe does not parse: %w: %w", err, ErrRecipeInvalid)
	}
	if len(w.Commands) == 0 {
		return Recipe{}, fmt.Errorf("recipe declares no commands: %w", ErrRecipeInvalid)
	}
	r := Recipe{
		Commands: make([]Command, 0, len(w.Commands)),
		Capture:  CaptureMode(w.Capture),
	}
	for _, rawCmd := range w.Commands {
		cmd, err := parseCommand(rawCmd)
		if err != nil {
			return Recipe{}, err
		}
		r.Commands = append(r.Commands, cmd)
	}
	if !r.Capture.valid() {
		return Recipe{}, fmt.Errorf("recipe capture mode %q: %w", w.Capture, ErrRecipeInvalid)
	}
	return r, nil
}

// parseCommand decodes one command by its JSON kind: an array is the
// bare argv form and runs in the project image; an object is the
// explicit form and must name both its argv and a valid room. Anything
// else (a string, a number, null) is refused.
func parseCommand(raw json.RawMessage) (Command, error) {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 {
		return Command{}, fmt.Errorf("recipe command is empty: %w", ErrRecipeInvalid)
	}
	var rawArgv []*string
	class := ClassProjectImage
	history := false
	switch trimmed[0] {
	case '[':
		if err := strictjson.Decode(raw, &rawArgv, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
			return Command{}, fmt.Errorf("recipe command does not parse: %w: %w", err, ErrRecipeInvalid)
		}
	case '{':
		var obj commandObjectWire
		if err := strictjson.Decode(raw, &obj, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
			return Command{}, fmt.Errorf("recipe command object does not parse: %w: %w", err, ErrRecipeInvalid)
		}
		if obj.Argv == nil {
			return Command{}, fmt.Errorf("recipe command object carries no argv: %w", ErrRecipeInvalid)
		}
		if obj.Room == nil {
			return Command{}, fmt.Errorf("recipe command object names no room: %w", ErrRecipeInvalid)
		}
		class = ExecutionClass(*obj.Room)
		if !class.valid() {
			return Command{}, fmt.Errorf("recipe command room %q is not an execution class: %w", *obj.Room, ErrRecipeInvalid)
		}
		rawArgv = obj.Argv
		if obj.History != nil {
			var err error
			if history, err = parseHistory(obj.History, class); err != nil {
				return Command{}, err
			}
		}
	default:
		return Command{}, fmt.Errorf("recipe command is neither an argv array nor a command object: %w", ErrRecipeInvalid)
	}
	argv := make([]string, 0, len(rawArgv))
	for _, tok := range rawArgv {
		if tok == nil {
			return Command{}, fmt.Errorf("recipe command carries a null token: %w", ErrRecipeInvalid)
		}
		argv = append(argv, *tok)
	}
	if err := validateCommand(argv); err != nil {
		return Command{}, err
	}
	return Command{Argv: argv, Class: class, History: history}, nil
}

// parseHistory reads a command object's "history" value: a JSON boolean,
// never null or another type. Only a project_image command may ask for
// history. The macOS slot helper passes a command exactly HOME, TMPDIR,
// PATH, and LC_ALL (macverify's slot-run.sh), so it can't carry the
// range's SHAs without changing the root-owned helper on every executor.
func parseHistory(raw json.RawMessage, class ExecutionClass) (bool, error) {
	var history *bool
	if err := strictjson.Decode(raw, &history, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		return false, fmt.Errorf("recipe command history is not a boolean: %w: %w", err, ErrRecipeInvalid)
	}
	if history == nil {
		return false, fmt.Errorf("recipe command history is null: %w", ErrRecipeInvalid)
	}
	if *history && class != ClassProjectImage {
		return false, fmt.Errorf("recipe command room %q cannot give a command history: %w", class, ErrRecipeInvalid)
	}
	return *history, nil
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
// the runner passes each element to execve verbatim. Shell-runner -c
// forms are the exception: the command string would make the shell do
// a second parse and hide repo entrypoints from CommandPaths, so the
// recipe must express the command as explicit argv instead.
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
	if shellCommandString(argv) {
		return fmt.Errorf("shell runner %q carries a command-string option; use explicit argv: %w", argv[0], ErrRecipeInvalid)
	}
	return nil
}

// shellCommandString reports recognized direct shell forms that hand an
// opaque token to a second command parser. A -c may be its own token or
// part of a short-option cluster (for example, -ec). Fish's equivalent
// -C/--command/--init-command forms are included. We deliberately reject
// these options anywhere in a direct shell-runner argv instead of
// interpreting shell-specific option ordering: trusted verification
// recipes can run a repository script directly when that script itself
// needs one of these flags.
func shellCommandString(argv []string) bool {
	shell := path.Base(argv[0])
	switch shell {
	case "sh", "ash", "bash", "csh", "dash", "fish", "ksh", "mksh", "rbash", "tcsh", "yash", "zsh":
	default:
		return false
	}
	for _, tok := range argv[1:] {
		if len(tok) > 1 && tok[0] == '-' && tok[1] != '-' && strings.ContainsRune(tok[1:], 'c') {
			return true
		}
		if shell == "fish" {
			if len(tok) > 1 && tok[0] == '-' && tok[1] != '-' && strings.ContainsRune(tok[1:], 'C') {
				return true
			}
			if tok == "--command" || strings.HasPrefix(tok, "--command=") ||
				tok == "--init-command" || strings.HasPrefix(tok, "--init-command=") {
				return true
			}
		}
	}
	return false
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
// space. Every command counts whatever its class: a candidate that
// rewrites a script the macOS executor runs changes the recipe's effect
// as much as one the project image runs.
func (r Recipe) CommandPaths() []string {
	seen := map[string]bool{}
	var paths []string
	for _, cmd := range r.Commands {
		for _, tok := range cmd.Argv {
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
// ParseRecipe rejects recognized direct shell command-string forms
// before this classification, so the whitespace rule cannot hide an
// entrypoint behind such a command string. A verification entrypoint
// path with embedded whitespace is not supported; the recipe author
// names it without spaces. Otherwise no character in the filename
// disqualifies it: the no-shell runner executes the literal name, so a
// glob metacharacter, a colon, three embedded dots (`check...sh`), or a
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
	return domain.Digest(contentaddr.Format(sum[:]))
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
