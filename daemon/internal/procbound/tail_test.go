package procbound

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"
)

// writeAll writes each chunk and fails unless every write reports full
// consumption: a short write would block the child on its pipe.
func writeAll(t *testing.T, b *TailBuffer, chunks ...[]byte) {
	t.Helper()
	for _, chunk := range chunks {
		n, err := b.Write(chunk)
		if n != len(chunk) || err != nil {
			t.Fatalf("Write(%d bytes) = %d, %v; want %d, nil", len(chunk), n, err, len(chunk))
		}
	}
}

func checkBound(t *testing.T, b *TailBuffer, limit int) {
	t.Helper()
	if len(b.buf) > limit || cap(b.buf) > limit {
		t.Fatalf("buffer holds len %d cap %d, bound %d", len(b.buf), cap(b.buf), limit)
	}
}

func TestTailBufferKeepsAnUnderCapStreamWhole(t *testing.T) {
	t.Parallel()
	b := NewTailBuffer(16)
	writeAll(t, b, []byte("hello "), []byte("world"))
	if got := string(b.Bytes()); got != "hello world" || b.Truncated() {
		t.Fatalf("Bytes = %q, truncated %v; want whole stream, not truncated", got, b.Truncated())
	}
	checkBound(t, b, 16)
}

func TestTailBufferKeepsAnExactlyFullStreamUntruncated(t *testing.T) {
	t.Parallel()
	for _, chunks := range [][][]byte{
		{[]byte("0123456789")},
		{[]byte("01234"), []byte("56789")},
	} {
		b := NewTailBuffer(10)
		writeAll(t, b, chunks...)
		if got := string(b.Bytes()); got != "0123456789" || b.Truncated() {
			t.Fatalf("Bytes = %q, truncated %v; want the full stream untruncated", got, b.Truncated())
		}
	}
}

func TestTailBufferKeepsTheLastBytesOfAnOverCapStream(t *testing.T) {
	t.Parallel()
	b := NewTailBuffer(8)
	writeAll(t, b, []byte("noisy early output\n"), []byte("FAIL: x\n"))
	if got := string(b.Bytes()); got != "FAIL: x\n" || !b.Truncated() {
		t.Fatalf("Bytes = %q, truncated %v; want the last 8 bytes, truncated", got, b.Truncated())
	}
	checkBound(t, b, 8)
}

func TestTailBufferKeepsTheEndOfASingleWriteLargerThanTheBound(t *testing.T) {
	t.Parallel()
	b := NewTailBuffer(4)
	writeAll(t, b, []byte("abcdefgh"))
	if got := string(b.Bytes()); got != "efgh" || !b.Truncated() {
		t.Fatalf("Bytes = %q, truncated %v; want %q, truncated", got, b.Truncated(), "efgh")
	}
	checkBound(t, b, 4)
}

// Many small writes wrap the ring repeatedly; the result must equal the
// tail of the concatenated stream at every point, whatever the chunk sizes.
func TestTailBufferMatchesTheStreamTailAcrossWraps(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: deterministic chunk sizes for a test, not security-sensitive
	for _, limit := range []int{1, 3, 7, 64} {
		b := NewTailBuffer(int64(limit))
		var stream []byte
		for i := range 500 {
			chunk := []byte(fmt.Sprintf("%d:%s;", i, bytes.Repeat([]byte{'a' + byte(i%26)}, rng.IntN(2*limit+1))))
			writeAll(t, b, chunk)
			stream = append(stream, chunk...)
			want := stream[max(0, len(stream)-limit):]
			if got := b.Bytes(); !bytes.Equal(got, want) {
				t.Fatalf("limit %d after write %d: Bytes = %q, want %q", limit, i, got, want)
			}
			if b.Truncated() != (len(stream) > limit) {
				t.Fatalf("limit %d after write %d: truncated %v with %d bytes written", limit, i, b.Truncated(), len(stream))
			}
			checkBound(t, b, limit)
		}
	}
}

func TestTailBufferBytesIsACopy(t *testing.T) {
	t.Parallel()
	b := NewTailBuffer(4)
	writeAll(t, b, []byte("abcd"))
	got := b.Bytes()
	got[0] = 'X'
	if string(b.Bytes()) != "abcd" {
		t.Fatal("mutating the result of Bytes changed the buffer")
	}
}

func TestTailBufferWithNoBoundKeepsNothing(t *testing.T) {
	t.Parallel()
	b := NewTailBuffer(0)
	writeAll(t, b, nil)
	if b.Truncated() {
		t.Fatal("an empty write marked a zero-bound buffer truncated")
	}
	writeAll(t, b, []byte("x"))
	if len(b.Bytes()) != 0 || !b.Truncated() {
		t.Fatalf("Bytes = %q, truncated %v; want nothing kept, truncated", b.Bytes(), b.Truncated())
	}
}
