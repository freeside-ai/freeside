package stage

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// The token fixtures are structurally valid, non-live credentials: the
// rules key on structure (internal/credentialpattern).
var liveTestToken = "sk-ant-oat01-" + strings.Repeat("Q", 40)

const liveTestInvocation = domain.InvocationID("inv-live")

// liveHarness drives one tailer through a scripted gate and a clock the
// test advances, so the pull rate is deterministic.
type liveHarness struct {
	d     *Driver
	close func()
	mu    sync.Mutex
	// file is the transcript as the writer has written it so far; each pull
	// returns up to its cap from the requested offset.
	file  []byte
	pulls int
	now   time.Time
	// pullErr, when set, fails the next pull.
	pullErr error
}

func newLiveHarness(t *testing.T) *liveHarness {
	t.Helper()
	h := &liveHarness{now: fixedNow}
	gate := &stubGate{}
	gate.liveReadFn = func(_ context.Context, runID string, offset int64, maxBytes int) ([]byte, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if runID != "run-live" {
			t.Errorf("pull for run %q", runID)
		}
		h.pulls++
		if h.pullErr != nil {
			err := h.pullErr
			h.pullErr = nil
			return nil, err
		}
		if offset > int64(len(h.file)) {
			t.Fatalf("pull from %d past the file's %d bytes", offset, len(h.file))
		}
		rest := h.file[offset:]
		return bytes.Clone(rest[:min(len(rest), maxBytes)]), nil
	}
	h.d = newTestDriver(t, gate, newStubExports())
	h.d.now = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.now
	}
	h.close = h.d.openLiveTranscript(liveTestInvocation, "run-live")
	t.Cleanup(h.close)
	return h
}

// write appends to the writer's transcript and moves the clock past the
// pull interval, so the next read pulls.
func (h *liveHarness) write(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.file = append(h.file, s...)
	h.now = h.now.Add(liveTranscriptPullInterval)
}

func (h *liveHarness) read(t *testing.T, after int64) LiveTranscript {
	t.Helper()
	out, found, err := h.d.LiveTranscript(context.Background(), liveTestInvocation, after)
	if err != nil || !found {
		t.Fatalf("LiveTranscript(%d) = found %v, %v", after, found, err)
	}
	return out
}

// caughtUp says the tailer has pulled everything written so far.
func (h *liveHarness) caughtUp() bool {
	tailer := h.d.live[liveTestInvocation]
	tailer.mu.Lock()
	offset := tailer.offset
	tailer.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	return offset == int64(len(h.file))
}

// follow reads as a client does, from the last End, until the tailer has
// pulled everything written, and returns everything served.
func (h *liveHarness) follow(t *testing.T) (string, LiveTranscript) {
	t.Helper()
	var served strings.Builder
	var after int64
	var last LiveTranscript
	for range 64 {
		last = h.read(t, after)
		if last.State != LiveTranscriptLive {
			return served.String(), last
		}
		served.Write(last.Records)
		after = last.End
		if h.caughtUp() {
			return served.String(), last
		}
		h.mu.Lock()
		h.now = h.now.Add(liveTranscriptPullInterval)
		h.mu.Unlock()
	}
	t.Fatal("follow did not settle")
	return "", last
}

func TestLiveTranscriptServesCompleteRecordsAndHoldsAPartialLine(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	h.write(`{"a":1}` + "\n" + `{"b":`)
	got := h.read(t, 0)
	if got.State != LiveTranscriptLive || string(got.Records) != `{"a":1}`+"\n" || got.Start != 0 || got.End != 8 {
		t.Fatalf("first read = %+v", got)
	}
	h.write("2}\n")
	got = h.read(t, got.End)
	if string(got.Records) != `{"b":2}`+"\n" || got.Start != 8 || got.End != 16 {
		t.Fatalf("second read = %+v", got)
	}
}

// TestLiveTranscriptMatchesATokenSplitAcrossPulls: a token whose halves
// arrive in two pulls ends the view, and the first half's record is never
// served.
func TestLiveTranscriptMatchesATokenSplitAcrossPulls(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	h.write(`{"ok":1}` + "\n" + `{"t":"` + liveTestToken[:20])
	if got := h.read(t, 0); got.State != LiveTranscriptLive || string(got.Records) != `{"ok":1}`+"\n" {
		t.Fatalf("before the second half = %+v", got)
	}
	h.write(liveTestToken[20:] + `"}` + "\n" + `{"clean":2}` + "\n")
	if got := h.read(t, 9); got.State != LiveTranscriptWithheld || got.Records != nil {
		t.Fatalf("after the second half = %+v, want withheld", got)
	}
}

// TestLiveTranscriptMatchesATokenAcrossARecordBoundary: the scan runs over
// the stream, not record by record, so a rule that matches across a newline
// (the GCP key id allows whitespace) still matches, as the export scan's
// whole-file scan would.
func TestLiveTranscriptMatchesATokenAcrossARecordBoundary(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	h.write(`"private_key_id":` + "\n")
	if got := h.read(t, 0); got.State != LiveTranscriptLive {
		t.Fatalf("first record = %+v", got)
	}
	h.write(`"` + strings.Repeat("a", 40) + `"` + "\n")
	if got := h.read(t, 0); got.State != LiveTranscriptWithheld {
		t.Fatalf("second record = %+v, want withheld", got)
	}
}

// TestLiveTranscriptWithholdsAnOversizeRecordAndScansIt: a record past the
// bound is never shown, its tail is still scanned, and a token in it ends
// the view.
func TestLiveTranscriptWithholdsAnOversizeRecordAndScansIt(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	big := strings.Repeat("x", liveTranscriptRecordBytes+10)
	h.write(big + "\n" + `{"after":1}` + "\n")
	served, last := h.follow(t)
	if served != `{"after":1}`+"\n" || last.UnreadableRecords != 1 {
		t.Fatalf("served %d bytes, %d unreadable; want only the next record and one unreadable",
			len(served), last.UnreadableRecords)
	}

	h = newLiveHarness(t)
	h.write(strings.Repeat("x", liveTranscriptRecordBytes+10) + liveTestToken + "\n")
	if _, last := h.follow(t); last.State != LiveTranscriptWithheld {
		t.Fatalf("an oversize record carrying a token = %+v, want withheld", last)
	}
}

// TestLiveTranscriptWithholdsInvalidUTF8: a record that isn't valid UTF-8 is
// withheld whole and counted; with a token beside it, the view ends.
func TestLiveTranscriptWithholdsInvalidUTF8(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	h.write("{\"bad\":\"\xff\"}\n{\"good\":1}\n")
	served, last := h.follow(t)
	if served != `{"good":1}`+"\n" || last.UnreadableRecords != 1 {
		t.Fatalf("served %q, %d unreadable", served, last.UnreadableRecords)
	}

	h = newLiveHarness(t)
	h.write("{\"bad\":\"\xff" + liveTestToken + "\"}\n")
	if _, last := h.follow(t); last.State != LiveTranscriptWithheld {
		t.Fatalf("invalid UTF-8 carrying a token = %+v, want withheld", last)
	}
}

// TestLiveTranscriptStaysWithheldAfterAMatch: clean records after a match
// are never served, no pull runs again, and the tailer holds nothing.
func TestLiveTranscriptStaysWithheldAfterAMatch(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	h.write(`{"ok":1}` + "\n" + `{"t":"` + liveTestToken + `"}` + "\n")
	if got := h.read(t, 0); got.State != LiveTranscriptWithheld {
		t.Fatalf("read = %+v, want withheld", got)
	}
	pulls := h.pulls
	for range 3 {
		h.write(`{"clean":1}` + "\n")
		if got := h.read(t, 0); got.State != LiveTranscriptWithheld || got.Records != nil {
			t.Fatalf("read after the match = %+v", got)
		}
	}
	if h.pulls != pulls {
		t.Errorf("%d pulls after the match, want none", h.pulls-pulls)
	}
	tailer := h.d.live[liveTestInvocation]
	tailer.mu.Lock()
	defer tailer.mu.Unlock()
	if tailer.records != nil || tailer.pending != nil || tailer.carry != nil {
		t.Error("the tailer kept bytes after the match")
	}
}

// TestLiveTranscriptWindowTrimsItsFront: past the window's bound whole
// records leave from the front, and a client behind the window is told
// earlier records were left out.
func TestLiveTranscriptWindowTrimsItsFront(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	record := `{"r":"` + strings.Repeat("y", 64<<10) + `"}` + "\n"
	count := liveTranscriptWindowBytes/len(record) + 8
	for range count {
		h.write(record)
	}
	served, _ := h.follow(t)
	if len(served) != count*len(record) {
		t.Fatalf("a client following along got %d bytes, want %d", len(served), count*len(record))
	}
	got := h.read(t, 0)
	if !got.OmittedBefore || got.Start == 0 || got.Start%int64(len(record)) != 0 {
		t.Fatalf("a client from 0 = start %d, omitted %v; want a later record start and omitted", got.Start, got.OmittedBefore)
	}
	if len(got.Records) > liveTranscriptWindowBytes || int64(len(got.Records)) != got.End-got.Start {
		t.Fatalf("window serves %d bytes over [%d,%d)", len(got.Records), got.Start, got.End)
	}
}

// TestLiveTranscriptPullsAtMostEveryInterval: reads inside the interval
// answer from what is held and don't pull.
func TestLiveTranscriptPullsAtMostEveryInterval(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	h.write(`{"a":1}` + "\n")
	h.read(t, 0)
	h.mu.Lock()
	h.file = append(h.file, `{"b":2}`+"\n"...)
	h.now = h.now.Add(liveTranscriptPullInterval - time.Millisecond)
	h.mu.Unlock()
	for range 5 {
		if got := h.read(t, 8); len(got.Records) != 0 {
			t.Fatalf("read inside the interval served %q", got.Records)
		}
	}
	if h.pulls != 1 {
		t.Fatalf("%d pulls inside one interval, want 1", h.pulls)
	}
	h.mu.Lock()
	h.now = h.now.Add(time.Millisecond)
	h.mu.Unlock()
	if got := h.read(t, 8); string(got.Records) != `{"b":2}`+"\n" || h.pulls != 2 {
		t.Fatalf("read at the interval = %q after %d pulls", got.Records, h.pulls)
	}
}

// TestLiveTranscriptCatchesUpInOneRead: a view opened late on a long
// transcript pulls again while each pull fills its cap, so one read
// reaches the writer's present, and stops at the first short pull.
func TestLiveTranscriptCatchesUpInOneRead(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	record := `{"line":"` + strings.Repeat("x", 1000) + `"}` + "\n"
	h.write(strings.Repeat(record, (3*liveTranscriptPullBytes+liveTranscriptPullBytes/2)/len(record)))
	got := h.read(t, 0)
	if got.End != int64(len(h.file)) || !h.caughtUp() {
		t.Fatalf("one read ended at %d of %d bytes", got.End, len(h.file))
	}
	if h.pulls != 4 {
		t.Fatalf("%d pulls, want 4: three full and one short", h.pulls)
	}
}

// TestLiveTranscriptRefusesAnOffsetOffTheRecords: an offset past what was
// scanned, or inside a record, is refused rather than served from a guess.
func TestLiveTranscriptRefusesAnOffsetOffTheRecords(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	h.write(`{"a":1}` + "\n" + `{"b":2}` + "\n")
	h.read(t, 0)
	for _, after := range []int64{-1, 3, 17} {
		if _, _, err := h.d.LiveTranscript(context.Background(), liveTestInvocation, after); !errors.Is(err, ErrLiveTranscriptOffset) {
			t.Errorf("after %d = %v, want ErrLiveTranscriptOffset", after, err)
		}
	}
}

// TestLiveTranscriptFailedPullServesWhatIsHeld: a failed pull or a writer
// that isn't running yet still answers live from the window.
func TestLiveTranscriptFailedPullServesWhatIsHeld(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	h.mu.Lock()
	h.pullErr = ward.ErrNoLiveWriter
	h.mu.Unlock()
	if got := h.read(t, 0); got.State != LiveTranscriptLive || got.End != 0 {
		t.Fatalf("before the writer starts = %+v", got)
	}
	h.write(`{"a":1}` + "\n")
	h.read(t, 0)
	h.mu.Lock()
	h.pullErr = errors.New("container exec: exit status 1")
	h.mu.Unlock()
	h.write(`{"b":2}` + "\n")
	if got := h.read(t, 0); string(got.Records) != `{"a":1}`+"\n" {
		t.Fatalf("after a failed pull = %+v", got)
	}
}

// TestLiveTranscriptClosesWithTheHandoff: removal drops the tailer, later
// reads find no handoff, and a new handoff starts again from offset 0.
func TestLiveTranscriptClosesWithTheHandoff(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	h.write(`{"a":1}` + "\n")
	h.read(t, 0)
	h.close()
	if _, found, err := h.d.LiveTranscript(context.Background(), liveTestInvocation, 0); found || err != nil {
		t.Fatalf("after close = found %v, %v", found, err)
	}
	reopen := h.d.openLiveTranscript(liveTestInvocation, "run-live")
	defer reopen()
	h.write("")
	if got := h.read(t, 0); got.Start != 0 || string(got.Records) != `{"a":1}`+"\n" {
		t.Fatalf("a new handoff's first read = %+v, want the transcript from 0", got)
	}
}

func TestLiveTranscriptStatesAreValid(t *testing.T) {
	t.Parallel()
	for _, s := range AllLiveTranscriptStates {
		if !s.valid() {
			t.Errorf("%q is registered but invalid", s)
		}
	}
	if LiveTranscriptState("").valid() {
		t.Error("the zero state is valid")
	}
}

// TestLiveTranscriptPullsOneAtATime: a read that arrives while a pull is in
// flight answers from what is held instead of starting a second pull.
func TestLiveTranscriptPullsOneAtATime(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t)
	h.write(`{"a":1}` + "\n")
	h.read(t, 0)
	inPull, release := make(chan struct{}), make(chan struct{})
	gate := h.d.gate.(*stubGate)
	inner := gate.liveReadFn
	gate.liveReadFn = func(ctx context.Context, runID string, offset int64, maxBytes int) ([]byte, error) {
		close(inPull)
		<-release
		return inner(ctx, runID, offset, maxBytes)
	}
	h.write(`{"b":2}` + "\n")
	var wg sync.WaitGroup
	wg.Go(func() { h.read(t, 8) })
	<-inPull
	if got := h.read(t, 0); string(got.Records) != `{"a":1}`+"\n" {
		t.Errorf("read during a pull = %q, want what is held", got.Records)
	}
	close(release)
	wg.Wait()
	if h.pulls != 2 {
		t.Errorf("%d pulls, want 2", h.pulls)
	}
}

// TestLiveTranscriptLastsOnlyAsLongAsTheHandoff: the pipeline opens the
// tailer before the ward handoff and drops it when the handoff returns.
func TestLiveTranscriptLastsOnlyAsLongAsTheHandoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var d *Driver
	var duringHandoff bool
	gate := &stubGate{
		handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
			_, found, err := d.LiveTranscript(ctx, testInvoke, 0)
			duringHandoff = found && err == nil
			return nil, errors.New("scripted handoff failure")
		},
		liveReadFn: func(context.Context, string, int64, int) ([]byte, error) {
			return []byte(`{"a":1}` + "\n"), nil
		},
	}
	d = newTestDriver(t, gate, newStubExports())
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(ctx, testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil }); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSessionDone(t, d, testInvoke)
	if !duringHandoff {
		t.Error("no live transcript while the handoff ran")
	}
	if _, found, _ := d.LiveTranscript(ctx, testInvoke, 0); found {
		t.Error("the live transcript outlived the handoff")
	}
	d.liveMu.Lock()
	defer d.liveMu.Unlock()
	if len(d.live) != 0 {
		t.Errorf("%d tailers held after the handoff", len(d.live))
	}
}
