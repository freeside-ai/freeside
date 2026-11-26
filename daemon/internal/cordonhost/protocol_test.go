package cordonhost

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestFramesCarryOnlyTheirKindsFields(t *testing.T) {
	t.Parallel()
	good := []frame{
		{Kind: kindHello, Hello: &helloBody{Build: "b"}},
		{Kind: kindStarted, RunID: "run-1"},
		{Kind: kindExport, Export: &exportRequestBody{RunID: "run-1"}},
		{Kind: kindReleaseFinished, RunIDs: []string{"run-1"}},
		{Kind: kindCancel},
		{Kind: kindReply, Reply: &replyBody{}},
	}
	for _, f := range good {
		var buf bytes.Buffer
		if err := writeFrame(&buf, f); err != nil {
			t.Fatal(err)
		}
		if _, err := readFrame(&buf, false); err != nil {
			t.Errorf("a %s frame: %v", f.Kind, err)
		}
	}
	bad := []frame{
		{Kind: "launch_now"},
		{Kind: kindStarted},
		{Kind: kindStarted, RunID: "run-1", Export: &exportRequestBody{RunID: "run-1"}},
		{Kind: kindExport, RunID: "run-1"},
		{Kind: kindCancel, RunID: "run-1"},
		{Kind: kindReply, Reply: &replyBody{}, Hello: &helloBody{Build: "b"}},
	}
	for _, f := range bad {
		var buf bytes.Buffer
		if err := writeFrame(&buf, f); err != nil {
			t.Fatal(err)
		}
		if _, err := readFrame(&buf, false); !errors.Is(err, ErrProtocol) {
			t.Errorf("a %s frame carrying %v = %v, want ErrProtocol", f.Kind, f.present(), err)
		}
	}
}

// TestEnumsRegisterEveryMember pins each registration list to its valid
// predicate, and the zero value to invalid.
func TestEnumsRegisterEveryMember(t *testing.T) {
	t.Parallel()
	for _, k := range allKinds {
		if !k.valid() {
			t.Errorf("kind %q is registered but not valid", k)
		}
	}
	for _, c := range allErrorCodes {
		if !c.valid() {
			t.Errorf("error code %q is registered but not valid", c)
		}
	}
	if kind("").valid() || errorCode("").valid() {
		t.Error("an empty kind or error code is valid")
	}
}

func TestReadFrameRefusesOversizedAndUnknownFields(t *testing.T) {
	t.Parallel()
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], maxFrame+1)
	if _, err := readFrame(bytes.NewReader(size[:]), false); !errors.Is(err, ErrProtocol) {
		t.Fatalf("an oversized frame = %v", err)
	}
	body := []byte(`{"kind":"cancel","extra":1}`)
	binary.BigEndian.PutUint32(size[:], uint32(len(body))) //nolint:gosec // G115: a short literal
	if _, err := readFrame(bytes.NewReader(append(size[:], body...)), false); !errors.Is(err, ErrProtocol) {
		t.Fatalf("an unknown field = %v", err)
	}
	// A hello from another build may carry fields this one doesn't know.
	body = []byte(`{"kind":"hello","hello":{"build":"other","new_field":true}}`)
	binary.BigEndian.PutUint32(size[:], uint32(len(body))) //nolint:gosec // G115: a short literal
	f, err := readFrame(bytes.NewReader(append(size[:], body...)), true)
	if err != nil || f.Hello.Build != "other" {
		t.Fatalf("a newer hello = %+v, %v", f, err)
	}
}

// TestRefusalsCrossAsTheBackendsErrors shows each backend error the gate
// maps (cordonPortError in internal/daemoncompose) reaches the client as
// an error that matches the same sentinel or type, so the gate gives a
// host's refusal the class it gives a local one.
func TestRefusalsCrossAsTheBackendsErrors(t *testing.T) {
	t.Parallel()
	blocked := &cordon.BlockedError{RunID: "run-1", EndedBy: cordon.EndedByStalled, Err: cordon.ErrRunEndUnproven}
	cases := map[string]struct {
		err   error
		match func(error) bool
	}{
		"detached": {cordon.ErrDetached, func(e error) bool { return errors.Is(e, cordon.ErrDetached) }},
		"unproven": {fmt.Errorf("x: %w", cordon.ErrRunEndUnproven), func(e error) bool { return errors.Is(e, cordon.ErrRunEndUnproven) }},
		"no run":   {cordon.ErrNoRun, func(e error) bool { return errors.Is(e, cordon.ErrNoRun) }},
		"canceled": {fmt.Errorf("%w: %w: run-1", cordon.ErrCanceled, cordon.ErrNotExportable), func(e error) bool {
			return errors.Is(e, cordon.ErrCanceled) && errors.Is(e, cordon.ErrNotExportable)
		}},
		"worktree replaced": {fmt.Errorf("%w: run/ is not a directory", cordon.ErrWorktreeReplaced), func(e error) bool {
			return errors.Is(e, cordon.ErrWorktreeReplaced)
		}},
		"linked file": {fmt.Errorf("cordon: export run-1: %w", export.ErrLinkedFile), func(e error) bool {
			return errors.Is(e, export.ErrLinkedFile) && !errors.Is(e, cordon.ErrNotExportable)
		}},
		"not exportable": {fmt.Errorf("%w: run-1 stalled", cordon.ErrNotExportable), func(e error) bool {
			return errors.Is(e, cordon.ErrNotExportable) && !errors.Is(e, cordon.ErrCanceled)
		}},
		"no end record": {fmt.Errorf("%w: run-1", cordon.ErrNoEndRecord), func(e error) bool { return errors.Is(e, cordon.ErrNoEndRecord) }},
		// The host's reason names an item of its export; the client's is
		// its own sentence.
		"scan refused": {&cordon.ScanRefusalError{Reason: `worktree file "secret-name" matched credential rule x`}, func(e error) bool {
			var scan *cordon.ScanRefusalError
			return errors.As(e, &scan) && errors.Is(e, ward.ErrOutputScanRefused) && !strings.Contains(e.Error(), "secret-name")
		}},
		"build":    {ErrBuildMismatch, func(e error) bool { return errors.Is(e, ErrBuildMismatch) }},
		"record":   {fmt.Errorf("%w: disk", cordon.ErrConformanceRecord), func(e error) bool { return errors.Is(e, cordon.ErrConformanceRecord) }},
		"internal": {errors.New("anything"), func(e error) bool { return strings.Contains(e.Error(), "anything") }},
		"blocked": {blocked, func(e error) bool {
			var b *cordon.BlockedError
			return errors.As(e, &b) && b.RunID == "run-1" && b.EndedBy == cordon.EndedByStalled && errors.Is(e, cordon.ErrRunEndUnproven)
		}},
		"conformance and record": {
			errors.Join(&cordon.ConformanceFailure{Check: "read /x", Reason: "succeeded"}, cordon.ErrConformanceRecord),
			func(e error) bool {
				var f *cordon.ConformanceFailure
				return errors.As(e, &f) && f.Check == "read /x" && f.Reason == "succeeded" && errors.Is(e, cordon.ErrConformanceRecord)
			},
		},
	}
	for name, tc := range cases {
		if got := decodeError(encodeError(tc.err), "run-1"); !tc.match(got) {
			t.Errorf("%s: %v crossed as %v", name, tc.err, got)
		}
	}
	if err := decodeError(encodeError(blocked), "run-2"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("another run's blocked refusal = %v, want ErrProtocol", err)
	}
	if err := decodeError(&wireError{Code: "teapot"}, ""); !errors.Is(err, ErrProtocol) {
		t.Fatalf("an unknown code = %v, want ErrProtocol", err)
	}
	if got := encodeError(errors.New(strings.Repeat("é", maxText))).Message; len(got) > maxText {
		t.Fatalf("a long message crossed at %d bytes", len(got))
	}
}

func TestCheckEndRefusesAnImplausibleReport(t *testing.T) {
	t.Parallel()
	body := &failureBody{Bytes: []byte("transcript")}
	cases := map[string]struct {
		end      endBody
		export   bool
		recovery bool
		ok       bool
	}{
		"clean exit":            {end: endBody{EndedBy: cordon.EndedByExited}, ok: true},
		"failed with body":      {end: endBody{EndedBy: cordon.EndedByExited, ExitCode: 3, Transcript: body}, ok: true},
		"stalled":               {end: endBody{EndedBy: cordon.EndedByStalled, ExitCode: -1, Stderr: body}, ok: true},
		"unknown end":           {end: endBody{EndedBy: "vanished"}},
		"exit status 256":       {end: endBody{EndedBy: cordon.EndedByExited, ExitCode: 256}},
		"launch lost":           {end: endBody{EndedBy: cordon.EndedByLost}},
		"launch outcome":        {end: endBody{Outcome: cordon.RecoveryFailed, EndedBy: cordon.EndedByStalled}},
		"clean exit with body":  {end: endBody{EndedBy: cordon.EndedByExited, Transcript: body}},
		"canceled with body":    {end: endBody{EndedBy: cordon.EndedByCanceled, Transcript: body}},
		"recovery lost":         {end: endBody{Outcome: cordon.RecoveryLost, EndedBy: cordon.EndedByLost}, recovery: true, ok: true},
		"recovery failed":       {end: endBody{Outcome: cordon.RecoveryFailed, EndedBy: cordon.EndedByExited, ExitCode: 1, Transcript: body}, recovery: true, ok: true},
		"recovery exported":     {end: endBody{Outcome: cordon.RecoveryExported, EndedBy: cordon.EndedByExited}, export: true, recovery: true, ok: true},
		"exported, no export":   {end: endBody{Outcome: cordon.RecoveryExported, EndedBy: cordon.EndedByExited}, recovery: true},
		"exported nonzero":      {end: endBody{Outcome: cordon.RecoveryExported, EndedBy: cordon.EndedByExited, ExitCode: 1}, export: true, recovery: true},
		"failed with export":    {end: endBody{Outcome: cordon.RecoveryFailed, EndedBy: cordon.EndedByExited, ExitCode: 1}, export: true, recovery: true},
		"launch with export":    {end: endBody{EndedBy: cordon.EndedByExited}, export: true},
		"recovery no outcome":   {end: endBody{EndedBy: cordon.EndedByExited}, recovery: true},
		"recovery failed clean": {end: endBody{Outcome: cordon.RecoveryFailed, EndedBy: cordon.EndedByExited}, recovery: true},
		"recovery lost body":    {end: endBody{Outcome: cordon.RecoveryLost, EndedBy: cordon.EndedByStalled, Transcript: body}, recovery: true},
		"oversized body": {end: endBody{
			EndedBy: cordon.EndedByExited, ExitCode: 1,
			Transcript: &failureBody{Bytes: make([]byte, ward.MaxFailureTranscriptBytes+1)},
		}},
	}
	for name, tc := range cases {
		end := tc.end
		reply := &replyBody{End: &end}
		if tc.export {
			reply.Export = &exportReplyBody{BaseSHA: strings.Repeat("a", 40)}
		}
		_, err := checkEnd(reply, tc.recovery)
		if tc.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !tc.ok && !errors.Is(err, ErrProtocol) {
			t.Errorf("%s = %v, want ErrProtocol", name, err)
		}
	}
}
