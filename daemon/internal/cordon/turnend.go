package cordon

// turnEnds watches a streamed launch's stdout for the end of each turn: a
// line whose top-level "type" member is the string "result", the record
// the CLI writes once a turn's reply is done. It reads the stream as it
// passes and keeps no line, so a line of any length is seen whole. The CLI
// doesn't put "type" first in a result record, so the scan follows the
// line's JSON structure instead of matching a prefix.
//
// What it reports decides only when the supervisor closes the agent's
// input (inputFeed). A line the agent forges, or a result that arrives
// early, can close the input early and do nothing else: the run still ends
// by its exit status, and the bytes reach the transcript unchanged.
type turnEnds struct {
	ended func()

	depth    int
	inString bool
	escaped  bool
	// member is where the scan stands in the line's top-level object.
	member memberPhase
	// capture collects a top-level key, or the value of the "type" key,
	// while it is a string; overflow marks one longer than any word it
	// compares with, or one holding an escape.
	capture  bool
	captured []byte
	overflow bool
	// typeKey is whether the member being read is "type", and result
	// whether this line's "type" was "result".
	typeKey, result bool
	stringIsKey     bool
}

type memberPhase int

const (
	memberNone memberPhase = iota
	memberKey
	memberColon
	memberValue
	memberAfterValue
)

// maxCaptured bounds a captured key or value: the words it compares with
// are "type" and "result".
const maxCaptured = len("result")

func newTurnEnds(ended func()) *turnEnds {
	return &turnEnds{ended: ended, captured: make([]byte, 0, maxCaptured)}
}

// Write never fails, so the stream it watches is never cut short.
func (t *turnEnds) Write(p []byte) (int, error) {
	for _, c := range p {
		t.next(c)
	}
	return len(p), nil
}

func (t *turnEnds) next(c byte) {
	if c == '\n' {
		// A JSON string can't hold a raw newline, so a newline ends the
		// record whatever the scan's state.
		if t.result {
			t.ended()
		}
		*t = turnEnds{ended: t.ended, captured: t.captured[:0]}
		return
	}
	if t.inString {
		t.stringByte(c)
		return
	}
	switch c {
	case '"':
		t.inString = true
		top := t.depth == 1
		t.stringIsKey = top && t.member == memberKey
		t.capture = t.stringIsKey || (top && t.member == memberValue && t.typeKey)
		t.captured, t.overflow = t.captured[:0], false
		if top && t.member == memberValue {
			t.member = memberAfterValue
		}
	case '{', '[':
		if t.depth == 1 && t.member == memberValue {
			t.member = memberAfterValue
		}
		t.depth++
		if t.depth == 1 && c == '{' {
			t.member = memberKey
		}
	case '}', ']':
		t.depth--
	case ':':
		if t.depth == 1 && t.member == memberColon {
			t.member = memberValue
		}
	case ',':
		if t.depth == 1 {
			t.member, t.typeKey = memberKey, false
		}
	case ' ', '\t', '\r':
	default:
		// A number, true, false, or null.
		if t.depth == 1 && t.member == memberValue {
			t.member = memberAfterValue
		}
	}
}

func (t *turnEnds) stringByte(c byte) {
	switch {
	case t.escaped:
		t.escaped = false
	case c == '\\':
		t.escaped, t.overflow = true, true
	case c == '"':
		t.inString = false
		if t.capture {
			word := string(t.captured)
			if t.stringIsKey {
				t.typeKey = !t.overflow && word == "type"
				t.member = memberColon
			} else if !t.overflow && word == "result" {
				t.result = true
			}
		} else if t.stringIsKey {
			t.member = memberColon
		}
		t.capture = false
	case t.capture:
		if len(t.captured) < maxCaptured {
			t.captured = append(t.captured, c)
		} else {
			t.overflow = true
		}
	}
}
