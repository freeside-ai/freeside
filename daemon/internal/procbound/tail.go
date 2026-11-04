package procbound

// TailBuffer is an io.Writer that keeps the last max bytes written to it and
// records whether it dropped any. A child process's failure detail
// conventionally comes last (the failing test, the exit line, the error a
// tool prints before it quits), so a bound on captured output keeps the end.
//
// Write always reports full consumption, so a child never blocks on a full
// pipe once the bound is reached. The buffer grows to at most max bytes and
// then overwrites its oldest bytes in place, so each write costs time in
// proportion to its own length, not to the bound. A TailBuffer is not safe
// for concurrent use; os/exec serializes writes when Stdout and Stderr name
// the same writer.
type TailBuffer struct {
	buf []byte
	// start is the index of the oldest byte once buf has reached max and
	// writes wrap; it is zero until then.
	start     int
	max       int
	truncated bool
}

// NewTailBuffer returns a TailBuffer bounded to limit bytes. A limit of zero
// or less keeps nothing and reports any write as truncation.
func NewTailBuffer(limit int64) *TailBuffer {
	return &TailBuffer{max: int(max(limit, 0))}
}

// Write keeps the end of p within the bound and always returns len(p), nil.
func (b *TailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	if n >= b.max {
		// p alone fills the bound: it replaces everything kept so far.
		b.truncated = b.truncated || n > b.max || len(b.buf) > 0
		if b.max == 0 {
			return n, nil
		}
		if cap(b.buf) < b.max {
			b.buf = make([]byte, b.max)
		}
		b.buf = b.buf[:b.max]
		copy(b.buf, p[n-b.max:])
		b.start = 0
		return n, nil
	}
	if room := b.max - len(b.buf); room > 0 {
		keep := min(room, len(p))
		b.grow(keep)
		b.buf = append(b.buf, p[:keep]...)
		p = p[keep:]
	}
	if len(p) > 0 {
		b.truncated = true
	}
	// The buffer is full: overwrite the oldest bytes, wrapping at most once
	// because len(p) < max.
	for len(p) > 0 {
		c := copy(b.buf[b.start:], p)
		p = p[c:]
		b.start = (b.start + c) % b.max
	}
	return n, nil
}

// grow makes room for n more bytes without letting the capacity pass max,
// which append's own growth would.
func (b *TailBuffer) grow(n int) {
	if len(b.buf)+n <= cap(b.buf) {
		return
	}
	next := min(max(2*cap(b.buf), len(b.buf)+n), b.max)
	grown := make([]byte, len(b.buf), next)
	copy(grown, b.buf)
	b.buf = grown
}

// Bytes returns a copy of the kept bytes in the order they were written.
func (b *TailBuffer) Bytes() []byte {
	return append(append([]byte(nil), b.buf[b.start:]...), b.buf[:b.start]...)
}

// Truncated reports whether any written byte was dropped.
func (b *TailBuffer) Truncated() bool {
	return b.truncated
}
