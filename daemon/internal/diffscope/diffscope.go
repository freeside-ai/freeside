// Package diffscope is the deterministic diff-overlap check behind the review
// finding contract (plan §5.13): it parses a unified diff into per-path
// new-side changed line ranges and reports whether a finding's structured
// location resolves into the reviewed diff. It executes no git and holds no
// I/O; the engine feeds it the already-produced diff text (that wiring is
// #840's). A finding whose location does not overlap the reviewed diff is a
// contradiction the caller rejects, so the check fails closed on anything it
// cannot resolve: a nil location, a path absent from the diff, or a line range
// that touches no changed new-side line.
package diffscope

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// lineRange is an inclusive, 1-based span of new-side lines a hunk added.
type lineRange struct {
	start, end int
}

// fileChange is the new-side account of one file in a diff: the path, whether
// the new side is deleted (/dev/null), and the changed new-side line ranges. A
// pure deletion carries no ranges (its new side has no lines).
type fileChange struct {
	deleted bool
	ranges  []lineRange
}

// Diff is a parsed unified diff indexed by new-side path. The zero value is an
// empty diff that overlaps nothing; construct one with Parse.
type Diff struct {
	files map[string]fileChange
}

// hunkHeader matches a unified-diff hunk header `@@ -old[,n] +new[,n] @@`,
// capturing the old start/count (groups 1,2) and new start/count (groups 3,4).
var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// Parse reads unified-diff text (the `git diff -U0` form, default a/ and b/
// prefixes) into a Diff, recording each file's new-side changed line ranges. A
// recognizable but malformed hunk header, a hunk with no preceding file header,
// or a body line the declared hunk length overran is a parse error rather than
// a silently dropped range: a validator that under-reads the diff would wave
// findings through. Non-diff lines (index, mode, similarity) are ignored.
//
// The parser is stateful about hunk bodies: after a header it consumes exactly
// the declared number of body lines, classifying each by its leading diff
// marker (`+`/`-`, and the uncounted `\ No newline` marker). A body line is
// therefore never mistaken for a file header, so a removed `-- x` line (which
// renders as `--- x`) or an added `++ x` line (`+++ x`) is consumed as content.
// A space-prefixed context line has no place in a -U0 diff and is rejected: it
// would otherwise leave an unchanged line inside a recorded new-side range. A
// pure rename with no content change emits no hunk and records no range, so a
// finding on it does not overlap (there is no reviewed change).
func Parse(diff string) (Diff, error) {
	files := map[string]fileChange{}
	var (
		curPath        string
		haveFile       bool
		oldPath        string
		oldRem, newRem int // body lines still to consume in the current hunk
	)
	for _, line := range strings.Split(diff, "\n") {
		if oldRem > 0 || newRem > 0 {
			// A body marker is valid only while its side has lines left: an added
			// line needs new-side room, a removed line old-side room. A context
			// (space-prefixed) line is rejected outright: the contracted input is
			// `git diff -U0`, which emits zero context, so a context line means
			// the input is not the contracted form. Recording it would also break
			// the range model below, which treats the whole new-side span as
			// added; an unchanged context line inside that span would then satisfy
			// Overlaps. Fail closed rather than approve a finding on a line the
			// diff never changed.
			switch {
			case strings.HasPrefix(line, "\\"): // "\ No newline at end of file": uncounted
			case strings.HasPrefix(line, "+"):
				if newRem == 0 {
					return Diff{}, fmt.Errorf("diffscope: added line past the hunk's new-side count at %q", line)
				}
				newRem--
			case strings.HasPrefix(line, "-"):
				if oldRem == 0 {
					return Diff{}, fmt.Errorf("diffscope: removed line past the hunk's old-side count at %q", line)
				}
				oldRem--
			case strings.HasPrefix(line, " "):
				return Diff{}, fmt.Errorf("diffscope: context line in a -U0 diff at %q", line)
			default:
				return Diff{}, fmt.Errorf("diffscope: hunk body overran its declared length at %q", line)
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "--- "):
			oldPath = stripDiffPrefix(strings.TrimPrefix(line, "--- "))
			haveFile = false
		case strings.HasPrefix(line, "+++ "):
			newPath := stripDiffPrefix(strings.TrimPrefix(line, "+++ "))
			deleted := newPath == ""
			curPath = newPath
			if deleted {
				curPath = oldPath // a deleted file is keyed by its old path
			}
			if curPath == "" {
				return Diff{}, fmt.Errorf("diffscope: file header names no path: %q", line)
			}
			fc := files[curPath]
			fc.deleted = deleted
			files[curPath] = fc
			haveFile = true
		case strings.HasPrefix(line, "@@"):
			if !haveFile {
				return Diff{}, fmt.Errorf("diffscope: hunk header before any file header: %q", line)
			}
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				return Diff{}, fmt.Errorf("diffscope: malformed hunk header: %q", line)
			}
			oldCount, newStart, newCount := 1, 0, 1
			if m[2] != "" {
				oldCount, _ = strconv.Atoi(m[2])
			}
			newStart, _ = strconv.Atoi(m[3])
			if m[4] != "" {
				newCount, _ = strconv.Atoi(m[4])
			}
			if newCount > 0 {
				// With no context lines (rejected in the body consumer above), the
				// hunk's new-side span is exactly its added lines: removed lines take
				// no new-side number, so the additions occupy newStart..+newCount-1
				// contiguously. Recording the whole span therefore records only added
				// lines, and Overlaps never accepts a finding on an unchanged line.
				fc := files[curPath]
				fc.ranges = append(fc.ranges, lineRange{start: newStart, end: newStart + newCount - 1})
				files[curPath] = fc
			}
			oldRem, newRem = oldCount, newCount
		}
	}
	if oldRem > 0 || newRem > 0 {
		// The last hunk's body was cut short, so the diff is truncated and later
		// hunks or files may be missing entirely. Fail closed rather than return
		// header-declared ranges the body never backed: a finding in the
		// truncated-away part must read as unvalidatable, not as non-overlapping.
		return Diff{}, fmt.Errorf("diffscope: truncated hunk body, %d old and %d new line(s) missing", oldRem, newRem)
	}
	return Diff{files: files}, nil
}

// stripDiffPrefix normalizes a `---`/`+++` path operand: it decodes git's
// C-style path quoting (core.quotePath default, used for non-ASCII or special
// bytes), returns "" for the /dev/null sentinel (an added or deleted side), and
// otherwise drops the git a/ or b/ prefix and any trailing tab-delimited
// timestamp.
func stripDiffPrefix(operand string) string {
	operand = strings.TrimRight(operand, "\r")
	if strings.HasPrefix(operand, `"`) {
		if unquoted, ok := gitUnquote(operand); ok {
			operand = unquoted
		}
	} else if i := strings.IndexByte(operand, '\t'); i >= 0 {
		operand = operand[:i]
	}
	if operand == "/dev/null" {
		return ""
	}
	if len(operand) >= 2 && (operand[:2] == "a/" || operand[:2] == "b/") {
		return operand[2:]
	}
	return operand
}

// gitUnquote decodes one C-style-quoted diff path operand (git core.quotePath
// default): a leading double quote, C escapes and three-digit octal \NNN byte
// escapes, then a closing double quote (any trailing tab-timestamp is ignored).
// It returns the decoded path and whether the operand was a well-formed quoted
// string; a malformed operand returns ok=false so the caller keeps the raw
// text rather than a half-decoded path.
func gitUnquote(operand string) (string, bool) {
	if len(operand) < 2 || operand[0] != '"' {
		return "", false
	}
	var b []byte
	for i := 1; i < len(operand); {
		c := operand[i]
		if c == '"' {
			// The closing quote must end the operand, or be followed only by the
			// tab-delimited timestamp git may append. Anything else is garbage after
			// a quoted path (a malformed header): fail the decode so the caller keeps
			// the raw operand, which then matches no finding path and Overlaps fails
			// closed, rather than indexing the hunk under a truncated path.
			if rest := operand[i+1:]; rest != "" && rest[0] != '\t' {
				return "", false
			}
			return string(b), true
		}
		if c != '\\' {
			b = append(b, c)
			i++
			continue
		}
		i++
		if i >= len(operand) {
			return "", false
		}
		switch e := operand[i]; e {
		case '\\', '"':
			b = append(b, e)
		case 'a':
			b = append(b, '\a')
		case 'b':
			b = append(b, '\b')
		case 'f':
			b = append(b, '\f')
		case 'n':
			b = append(b, '\n')
		case 'r':
			b = append(b, '\r')
		case 't':
			b = append(b, '\t')
		case 'v':
			b = append(b, '\v')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			if i+2 >= len(operand) {
				return "", false
			}
			v := 0
			for k := range 3 {
				d := operand[i+k]
				if d < '0' || d > '7' {
					return "", false
				}
				v = v*8 + int(d-'0')
			}
			b = append(b, byte(v))
			i += 2
		default:
			return "", false
		}
		i++
	}
	return "", false // unterminated quote
}

// Overlaps reports whether a finding location resolves into the reviewed diff.
// It fails closed: a nil location, a path the diff does not touch, or a line
// range that intersects no changed new-side range returns false. The whole-file
// location (0,0) is accepted iff the diff touches the path — including a
// deleted path, whose only valid location is whole-file. A concrete line range
// on a deleted path has no new-side line to overlap and is rejected.
func (d Diff) Overlaps(loc *domain.FindingLocation) bool {
	if loc == nil {
		return false
	}
	// This exported predicate is the fail-closed acceptance decision, so it
	// re-validates the location rather than trusting the caller to have done
	// so. A malformed range (a partial range such as {0, 12}, a non-positive
	// or inverted endpoint) would otherwise satisfy the interval test below and
	// approve a finding whose location domain validation rejects.
	if err := loc.Validate(); err != nil {
		return false
	}
	fc, ok := d.files[loc.Path]
	if !ok {
		return false
	}
	if loc.StartLine == 0 && loc.EndLine == 0 {
		return true // whole-file location: the touched path is enough
	}
	if fc.deleted {
		return false
	}
	for _, r := range fc.ranges {
		if loc.StartLine <= r.end && r.start <= loc.EndLine {
			return true
		}
	}
	return false
}
