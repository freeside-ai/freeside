package projectimage

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

const (
	// maxFailingTestLines bounds the failing-test lines a proof error lists.
	maxFailingTestLines = 20
	// maxFailingTestLineBytes cuts each listed line, so a long test name or
	// message can't crowd out the rest.
	maxFailingTestLineBytes = 240
	// maxPendingLineBytes bounds the partial line carried between writes. The
	// match needs only a line's start, so the rest of a longer line is dropped.
	maxPendingLineBytes = 4 << 10
)

// failingTestLine matches the lines `go test` prints for a failed test, at
// any subtest indent, and for a failed package. A bare FAIL line, which only
// closes a package's verbose output, doesn't name anything and isn't kept.
var failingTestLine = regexp.MustCompile(`^(\s*--- FAIL: |FAIL\t\S)`)

// failingTests collects the failing Go test lines from a command's whole
// output stream, which the bounded tail can't hold. Write never returns an
// error (see commandSpec.Output). It is not safe for concurrent use.
type failingTests struct {
	pending []byte
	lines   []string
	more    int
}

func (f *failingTests) Write(p []byte) (int, error) {
	n := len(p)
	for {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			f.carry(p)
			return n, nil
		}
		f.carry(p[:end])
		f.endLine()
		p = p[end+1:]
	}
}

func (f *failingTests) carry(part []byte) {
	room := maxPendingLineBytes - len(f.pending)
	if len(part) > room {
		part = part[:room]
	}
	f.pending = append(f.pending, part...)
}

func (f *failingTests) endLine() {
	line := strings.TrimRight(string(f.pending), "\r")
	f.pending = f.pending[:0]
	if !failingTestLine.MatchString(line) {
		return
	}
	if len(f.lines) == maxFailingTestLines {
		f.more++
		return
	}
	if len(line) > maxFailingTestLineBytes {
		line = strings.ToValidUTF8(line[:maxFailingTestLineBytes], "") + "…"
	}
	f.lines = append(f.lines, line)
}

// summary lists the kept lines and a count of the rest, or returns "" when
// none matched. A last line with no newline counts.
func (f *failingTests) summary() string {
	if len(f.pending) > 0 {
		f.endLine()
	}
	if len(f.lines) == 0 {
		return ""
	}
	summary := strings.Join(f.lines, "\n")
	if f.more > 0 {
		summary += fmt.Sprintf("\n… and %d more", f.more)
	}
	return summary
}
