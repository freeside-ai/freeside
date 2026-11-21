package stage

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/credentialpattern"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// The live transcript tailer serves a running writer's transcript to the
// operator's clients (plan §5.7, "Live view of a running writer"; devlog
// 2026-11-20-2253-live-transcript-read.md). Tier A: it decides which
// untrusted bytes, read while a credential-bearing writer runs, leave the
// daemon. It checks, before any byte is served:
//
//   - every pulled byte, in stream order with an overlap carried across
//     pulls, against the credential rules (credentialpattern.Match), so a
//     token split across two pulls or two records still matches;
//   - that a record is complete (a trailing partial line waits), within the
//     per-record bound, and valid UTF-8; any other record is withheld whole
//     and counted, never truncated and shown.
//
// A match ends the view for the invocation: the tailer drops everything it
// holds, never pulls again, and answers withheld until the handoff returns.
// The tailer exists only from just before the ward handoff to just after it
// returns (handoffAndImport), holds what it serves in memory only, bounded,
// and writes nothing anywhere. It never feeds Stream, so nothing reads it as
// outcome or evidence.

const (
	// liveTranscriptPullInterval is the least time between two pulls for one
	// invocation, however many clients ask. Nothing pulls unless asked.
	liveTranscriptPullInterval = 2 * time.Second
	// liveTranscriptPullBytes caps one pull. A pull that fills it is
	// followed by another at once, up to liveTranscriptCatchUpPulls, so a
	// view opened late on a long transcript reaches the writer's present in
	// a few reads instead of creeping toward it a megabyte at a time.
	liveTranscriptPullBytes    = 1 << 20
	liveTranscriptCatchUpPulls = liveTranscriptWindowBytes / liveTranscriptPullBytes
	// liveTranscriptRecordBytes bounds one record, its newline included. A
	// longer record is withheld whole.
	liveTranscriptRecordBytes = 1 << 20
	// liveTranscriptWindowBytes and liveTranscriptWindowRecords bound what
	// the tailer keeps; whole records leave from the front past either.
	liveTranscriptWindowBytes   = 8 << 20
	liveTranscriptWindowRecords = 1 << 16
	// liveTranscriptScanOverlap is carried from one pull's scan into the
	// next, as the export scan carries it between chunks
	// (internal/exportscan), so a match straddling a pull still matches.
	liveTranscriptScanOverlap = 4 << 10
)

// ErrLiveTranscriptOffset means the requested offset is past what the
// tailer has scanned or isn't a record boundary in its window. Clients only
// ever send an offset an earlier answer ended at.
var ErrLiveTranscriptOffset = errors.New("live transcript offset is not a record boundary")

// LiveTranscriptState says what a live transcript answer carries. The zero
// value is invalid.
type LiveTranscriptState string

const (
	// LiveTranscriptLive is a running handoff's scanned, complete records.
	LiveTranscriptLive LiveTranscriptState = "live"
	// LiveTranscriptWithheld means a credential rule matched; nothing more
	// is served for the invocation.
	LiveTranscriptWithheld LiveTranscriptState = "withheld"
)

// AllLiveTranscriptStates registers every state.
var AllLiveTranscriptStates = []LiveTranscriptState{LiveTranscriptLive, LiveTranscriptWithheld}

func (s LiveTranscriptState) valid() bool {
	switch s {
	case LiveTranscriptLive, LiveTranscriptWithheld:
		return true
	default:
		return false
	}
}

// LiveTranscript is one answer. Records holds the complete, clean records
// in the byte range [Start, End) of the transcript file, newline-terminated
// and concatenated; withheld records in that range are left out and only
// counted. OmittedBefore says records between the requested offset and
// Start left the window. UnreadableRecords counts every record withheld for
// size or encoding since the writer started. Only State is set when it is
// withheld.
type LiveTranscript struct {
	State             LiveTranscriptState
	Start, End        int64
	Records           []byte
	OmittedBefore     bool
	UnreadableRecords int
}

// liveRecord is one complete record. line is nil when it was withheld.
type liveRecord struct {
	start int64
	line  []byte
}

// liveTailer is one running handoff's tail state. mu guards every field;
// it is never held across a gate read.
type liveTailer struct {
	runID string

	mu       sync.Mutex
	closed   bool
	withheld bool
	pulling  bool
	lastPull time.Time
	failing  bool
	// offset is how many transcript bytes have been pulled and scanned.
	offset int64
	carry  []byte
	// pending is the trailing partial record, which starts at completeEnd;
	// oversize marks it past the bound, dropped until its newline arrives.
	pending  []byte
	oversize bool
	// records is the window; windowStart is the offset it begins at, and
	// completeEnd is where the last complete record ended.
	records     []liveRecord
	windowStart int64
	windowBytes int
	completeEnd int64
	unreadable  int
}

// openLiveTranscript registers a tailer for the invocation's handoff and
// returns its removal, which drops everything the tailer holds. A tailer
// already registered (a handoff restarted in this process) is replaced, so
// a new handoff always scans its transcript from the start.
func (d *Driver) openLiveTranscript(id domain.InvocationID, runID string) func() {
	t := &liveTailer{runID: runID}
	d.liveMu.Lock()
	if d.live == nil {
		d.live = map[domain.InvocationID]*liveTailer{}
	}
	if old := d.live[id]; old != nil {
		old.close()
	}
	d.live[id] = t
	d.liveMu.Unlock()
	return func() {
		t.close()
		d.liveMu.Lock()
		if d.live[id] == t {
			delete(d.live, id)
		}
		d.liveMu.Unlock()
	}
}

func (t *liveTailer) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	t.drop()
}

// drop releases everything the tailer holds.
func (t *liveTailer) drop() {
	t.carry, t.pending, t.records = nil, nil, nil
	t.windowBytes = 0
}

// LiveTranscript answers a client's read of a running writer's transcript
// from byte offset after, which must be 0 or an End an earlier answer
// carried. found is false when no handoff for the invocation runs in this
// process. It starts pulling from the gate at most once per
// liveTranscriptPullInterval per invocation, and keeps pulling while each
// pull fills its cap, up to liveTranscriptCatchUpPulls; a read that
// arrives sooner, or while another pull is in flight, answers from what is
// held. A failed
// pull serves what is held: the view is display-only, so it never fails a
// read or affects the writer.
func (d *Driver) LiveTranscript(
	ctx context.Context, id domain.InvocationID, after int64,
) (out LiveTranscript, found bool, err error) {
	if after < 0 {
		return LiveTranscript{}, false, ErrLiveTranscriptOffset
	}
	d.liveMu.Lock()
	t := d.live[id]
	d.liveMu.Unlock()
	if t == nil {
		return LiveTranscript{}, false, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return LiveTranscript{}, false, nil
	}
	if !t.withheld && !t.pulling && (t.lastPull.IsZero() || d.now().Sub(t.lastPull) >= liveTranscriptPullInterval) {
		t.pulling, t.lastPull = true, d.now()
		for range liveTranscriptCatchUpPulls {
			if !d.pullLiveTranscript(ctx, id, t) {
				break
			}
		}
		t.pulling = false
	}
	if t.closed {
		return LiveTranscript{}, false, nil
	}
	if t.withheld {
		return LiveTranscript{State: LiveTranscriptWithheld}, true, nil
	}
	out, err = t.serve(after)
	return out, err == nil, err
}

// pullLiveTranscript runs one pull with t.mu held on entry and exit, but
// not across the gate read. It reports whether the pull filled its cap and
// the tailer can take more, so the caller pulls again.
func (d *Driver) pullLiveTranscript(ctx context.Context, id domain.InvocationID, t *liveTailer) bool {
	offset := t.offset
	t.mu.Unlock()
	data, pullErr := d.gate.ReadLiveTranscript(ctx, t.runID, offset, liveTranscriptPullBytes)
	t.mu.Lock()
	switch {
	case t.closed || t.withheld:
		return false
	case pullErr == nil:
		t.failing = false
		t.ingest(data)
		return len(data) == liveTranscriptPullBytes && !t.withheld
	case errors.Is(pullErr, ward.ErrNoLiveWriter):
		return false
	default:
		// The error is the runtime's fixed text: ward withholds the
		// exec's stderr, which can carry guest bytes.
		if !t.failing {
			d.logger.Warn("live transcript pull failed", "invocation", string(id), "error", pullErr)
		}
		t.failing = true
		return false
	}
}

// ingest scans data, then files its complete records. A match withholds
// the invocation and keeps nothing.
func (t *liveTailer) ingest(data []byte) {
	if len(data) == 0 {
		return
	}
	scanned := make([]byte, 0, len(t.carry)+len(data))
	scanned = append(append(scanned, t.carry...), data...)
	if credentialpattern.Match(scanned) {
		t.withheld = true
		t.drop()
		return
	}
	t.carry = append(t.carry[:0:0], scanned[max(0, len(scanned)-liveTranscriptScanOverlap):]...)
	t.offset += int64(len(data))
	end := t.offset - int64(len(data))
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			t.extend(data)
			return
		}
		t.extend(data[:i+1])
		end += int64(i + 1)
		t.complete(end)
		data = data[i+1:]
	}
}

// extend adds part of the current record, abandoning it past the bound.
func (t *liveTailer) extend(part []byte) {
	if t.oversize {
		return
	}
	if len(t.pending)+len(part) > liveTranscriptRecordBytes {
		t.oversize, t.pending = true, nil
		return
	}
	t.pending = append(t.pending, part...)
}

// complete files the current record, which ends at end, and trims the
// window's front.
func (t *liveTailer) complete(end int64) {
	rec := liveRecord{start: t.completeEnd}
	if !t.oversize && utf8.Valid(t.pending) {
		rec.line = t.pending
	} else {
		t.unreadable++
	}
	t.pending, t.oversize = nil, false
	t.completeEnd = end
	t.records = append(t.records, rec)
	t.windowBytes += len(rec.line)
	for len(t.records) > 0 &&
		(t.windowBytes > liveTranscriptWindowBytes || len(t.records) > liveTranscriptWindowRecords) {
		t.windowBytes -= len(t.records[0].line)
		t.records = t.records[1:]
	}
	if len(t.records) > 0 {
		t.windowStart = t.records[0].start
	} else {
		t.windowStart = t.completeEnd
	}
}

// serve answers from the window for a client that holds everything before
// after.
func (t *liveTailer) serve(after int64) (LiveTranscript, error) {
	if after > t.completeEnd {
		return LiveTranscript{}, ErrLiveTranscriptOffset
	}
	start, omitted := after, after < t.windowStart
	if omitted {
		start = t.windowStart
	}
	i := sort.Search(len(t.records), func(i int) bool { return t.records[i].start >= start })
	if start != t.completeEnd && (i == len(t.records) || t.records[i].start != start) {
		return LiveTranscript{}, ErrLiveTranscriptOffset
	}
	var records []byte
	for _, rec := range t.records[i:] {
		records = append(records, rec.line...)
	}
	return LiveTranscript{
		State: LiveTranscriptLive, Start: start, End: t.completeEnd, Records: records,
		OmittedBefore: omitted, UnreadableRecords: t.unreadable,
	}, nil
}
