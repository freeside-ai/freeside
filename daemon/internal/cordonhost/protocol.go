package cordonhost

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

var (
	// ErrBuildMismatch is a client and a launcher of different builds
	// refusing each other at the hello. The protocol's version is the
	// build's, so neither reads anything the other sends after it.
	ErrBuildMismatch = errors.New("cordonhost: the client and the launcher are different builds")
	// ErrExportUnsupported is the launcher's refusal of an export, a
	// released export's authentication, and a recovery that would export:
	// no export returns from a host yet (#3624).
	ErrExportUnsupported = errors.New("cordonhost: the launcher returns no export yet")
	// ErrMessageUnsupported is the client's refusal of an operator's
	// message: the protocol carries none yet (#3702). It wraps
	// cordon.ErrNotStreamed, so the gate settles the message refused as
	// not_streamed rather than leaving it to be offered again at every
	// start.
	ErrMessageUnsupported = fmt.Errorf("cordonhost: the launcher takes no message yet: %w", cordon.ErrNotStreamed)
	// ErrProtocol is a frame that breaks the protocol: too large, not
	// strict JSON, the wrong kind, or a field its kind doesn't carry.
	ErrProtocol = errors.New("cordonhost: protocol violation")
)

// maxFrame bounds one frame. The largest is a failed run's end, which
// carries the transcript and stderr, each bounded at
// ward.MaxFailureTranscriptBytes and base64 in the JSON.
const maxFrame = 3 * ward.MaxFailureTranscriptBytes

// maxText bounds a host's error text, reason, and every short string.
const maxText = 4096

// kind is a frame's kind. A connection carries one operation: the
// client's hello and the launcher's, then the client's request, then the
// launcher's reply, with record and recorded frames between them while the
// launcher's suite writes a conformance record, and a launch's cancel.
type kind string

const (
	kindHello               kind = "hello"
	kindLaunch              kind = "launch"
	kindCancel              kind = "cancel"
	kindRecord              kind = "record"
	kindRecorded            kind = "recorded"
	kindStarted             kind = "started"
	kindRequestCancellation kind = "request_cancellation"
	kindRecover             kind = "recover"
	kindReleaseFinished     kind = "release_finished"
	kindExport              kind = "export"
	kindAuthenticate        kind = "authenticate_released_export"
	kindFull                kind = "full"
	kindProbeBeforeJob      kind = "probe_before_job"
	kindReply               kind = "reply"
)

// allKinds is the single registration point for kind.
var allKinds = []kind{
	kindHello, kindLaunch, kindCancel, kindRecord, kindRecorded, kindStarted, kindRequestCancellation,
	kindRecover, kindReleaseFinished, kindExport, kindAuthenticate, kindFull, kindProbeBeforeJob, kindReply,
}

func (k kind) valid() bool {
	switch k {
	case kindHello, kindLaunch, kindCancel, kindRecord, kindRecorded, kindStarted, kindRequestCancellation,
		kindRecover, kindReleaseFinished, kindExport, kindAuthenticate, kindFull, kindProbeBeforeJob, kindReply:
		return true
	default:
		return false
	}
}

// frame is one message. Each kind carries exactly the fields fieldsOf
// names; validate refuses any other.
type frame struct {
	Kind     kind          `json:"kind"`
	Hello    *helloBody    `json:"hello,omitempty"`
	Launch   *launchBody   `json:"launch,omitempty"`
	Recover  *recoverBody  `json:"recover,omitempty"`
	RunID    string        `json:"run_id,omitempty"`
	RunIDs   []string      `json:"run_ids,omitempty"`
	Dir      string        `json:"dir,omitempty"`
	Record   *recordBody   `json:"record,omitempty"`
	Recorded *recordedBody `json:"recorded,omitempty"`
	Reply    *replyBody    `json:"reply,omitempty"`
}

// helloBody is each side's first frame. The client's carries its build;
// the launcher's adds what a daemon needs to know about the host before it
// sends anything: the configuration digest its records are bound to, the
// run account, and the facts its backend's approval check reads (nil when
// they can't be read, which refuses every launch).
type helloBody struct {
	Build               string        `json:"build"`
	ConfigurationDigest domain.Digest `json:"configuration_digest,omitempty"`
	Account             *Account      `json:"account,omitempty"`
	Facts               *factsBody    `json:"facts,omitempty"`
}

// Account is the run account a launcher serves as.
type Account struct {
	Name string `json:"name"`
	UID  int    `json:"uid"`
}

type factsBody struct {
	ScreenRecording bool `json:"screen_recording"`
	Accessibility   bool `json:"accessibility"`
	DesktopSession  bool `json:"desktop_session"`
}

// launchBody is a launch's request. The token is its one crossing of the
// connection; the base follows the frame as a seed stream (seed.go).
type launchBody struct {
	RunID        string   `json:"run_id"`
	Repository   string   `json:"repository"`
	BaseRef      string   `json:"base_ref"`
	BaseSHA      string   `json:"base_sha"`
	Prompt       []byte   `json:"prompt"`
	Args         []string `json:"args"`
	Token        []byte   `json:"token"`
	RequireProof bool     `json:"require_proof"`
}

type recoverBody struct {
	RunID      string                  `json:"run_id"`
	Transcript export.EvidenceSource   `json:"transcript"`
	Sources    []export.EvidenceSource `json:"sources"`
}

// recordBody is a conformance record the launcher's suite writes, without
// the store's generation, which no input path sets.
type recordBody struct {
	Backend             domain.RunnerBackendClass `json:"backend"`
	Outcome             domain.ConformanceOutcome `json:"outcome"`
	ConfigurationDigest domain.Digest             `json:"configuration_digest"`
	Capabilities        domain.CapabilitySnapshot `json:"capabilities"`
	ProvedAt            time.Time                 `json:"proved_at"`
}

// recordedBody answers a record: Error is empty once the client's recorder
// kept it.
type recordedBody struct {
	Error string `json:"error"`
}

// replyBody ends an operation: an error, a run's end, or Started's answer.
type replyBody struct {
	Error   *wireError `json:"error,omitempty"`
	End     *endBody   `json:"end,omitempty"`
	Started *bool      `json:"started,omitempty"`
}

// endBody is a launch's or a recovery's end. Outcome is a recovery's
// alone. Transcript and Stderr come only with a failed end, each absent
// when the host's file was absent or larger than
// ward.MaxFailureTranscriptBytes.
type endBody struct {
	Outcome    cordon.RecoveryOutcome `json:"outcome,omitempty"`
	EndedBy    cordon.EndedBy         `json:"ended_by"`
	ExitCode   int                    `json:"exit_code"`
	Reason     string                 `json:"reason,omitempty"`
	Transcript *failureBody           `json:"transcript,omitempty"`
	Stderr     *failureBody           `json:"stderr,omitempty"`
}

type failureBody struct {
	Bytes []byte `json:"bytes"`
}

// fieldsOf names the fields each kind carries, in frame's order.
func fieldsOf(k kind) []string {
	switch k {
	case kindHello:
		return []string{"hello"}
	case kindLaunch:
		return []string{"launch"}
	case kindRecover:
		return []string{"recover"}
	case kindStarted, kindRequestCancellation, kindExport:
		return []string{"run_id"}
	case kindAuthenticate:
		return []string{"run_id", "dir"}
	case kindReleaseFinished:
		return []string{"run_ids"}
	case kindRecord:
		return []string{"record"}
	case kindRecorded:
		return []string{"recorded"}
	case kindReply:
		return []string{"reply"}
	case kindCancel, kindFull, kindProbeBeforeJob:
		return nil
	}
	return nil
}

func (f frame) present() []string {
	var names []string
	for _, field := range []struct {
		name string
		set  bool
	}{
		{"hello", f.Hello != nil},
		{"launch", f.Launch != nil},
		{"recover", f.Recover != nil},
		{"run_id", f.RunID != ""},
		{"run_ids", f.RunIDs != nil},
		{"dir", f.Dir != ""},
		{"record", f.Record != nil},
		{"recorded", f.Recorded != nil},
		{"reply", f.Reply != nil},
	} {
		if field.set {
			names = append(names, field.name)
		}
	}
	return names
}

// validate checks the frame's shape: a known kind with exactly its fields.
// What the fields say is checked where they are read.
func (f frame) validate() error {
	if !f.Kind.valid() {
		return fmt.Errorf("%w: kind %q", ErrProtocol, f.Kind)
	}
	if got, want := f.present(), fieldsOf(f.Kind); !slices.Equal(got, want) {
		return fmt.Errorf("%w: a %s frame carries %v, not %v", ErrProtocol, f.Kind, got, want)
	}
	return nil
}

// writeFrame writes f as its 4-byte big-endian length and its JSON. The
// encoded bytes are cleared once written, since a launch's holds its token.
func writeFrame(w io.Writer, f frame) error {
	body, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("cordonhost: encode a %s frame: %w", f.Kind, err)
	}
	defer clear(body)
	if len(body) > maxFrame {
		return fmt.Errorf("%w: a %s frame of %d bytes", ErrProtocol, f.Kind, len(body))
	}
	buf := make([]byte, 4+len(body))
	defer clear(buf)
	binary.BigEndian.PutUint32(buf, uint32(len(body))) //nolint:gosec // G115: bounded by maxFrame above
	copy(buf[4:], body)
	_, err = w.Write(buf)
	return err
}

// readFrame reads one frame and checks its shape. A hello decodes allowing
// unknown fields (lenient), so a peer of another build is still read far
// enough to refuse by its build. The read bytes are cleared once decoded.
func readFrame(r io.Reader, lenient bool) (frame, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return frame{}, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > maxFrame {
		return frame{}, fmt.Errorf("%w: a frame of %d bytes", ErrProtocol, n)
	}
	body := make([]byte, n)
	defer clear(body)
	if _, err := io.ReadFull(r, body); err != nil {
		return frame{}, fmt.Errorf("%w: a short frame: %w", ErrProtocol, err)
	}
	var f frame
	decode := strictjson.Decode
	if lenient {
		decode = strictjson.DecodeAllowingUnknownFields
	}
	if err := decode(body, &f, strictjson.RejectInvalidUTF8, strictjson.Limit(maxFrame)); err != nil {
		return frame{}, fmt.Errorf("%w: %w", ErrProtocol, err)
	}
	if lenient {
		// A peer of another build may send a frame this build reads as a
		// different shape; only its kind and build are read.
		if f.Kind != kindHello && f.Kind != kindReply {
			return frame{}, fmt.Errorf("%w: a %s frame where a hello belongs", ErrProtocol, f.Kind)
		}
		return f, nil
	}
	if err := f.validate(); err != nil {
		return frame{}, err
	}
	return f, nil
}

// errorCode classifies a refusal so the client returns the error the
// backend would have.
type errorCode string

const (
	codeDetached          errorCode = "detached"
	codeBlocked           errorCode = "blocked"
	codeRunEndUnproven    errorCode = "run_end_unproven"
	codeNoRun             errorCode = "no_run"
	codeConformance       errorCode = "conformance"
	codeCanceled          errorCode = "canceled"
	codeExportUnsupported errorCode = "export_unsupported"
	codeBuildMismatch     errorCode = "build_mismatch"
	codeInternal          errorCode = "internal"
)

// allErrorCodes is the single registration point for errorCode.
var allErrorCodes = []errorCode{
	codeDetached, codeBlocked, codeRunEndUnproven, codeNoRun, codeConformance,
	codeCanceled, codeExportUnsupported, codeBuildMismatch, codeInternal,
}

func (c errorCode) valid() bool {
	switch c {
	case codeDetached, codeBlocked, codeRunEndUnproven, codeNoRun, codeConformance,
		codeCanceled, codeExportUnsupported, codeBuildMismatch, codeInternal:
		return true
	default:
		return false
	}
}

// wireError is a refusal on the wire. Check and Reason are a conformance
// failure's; RunID and EndedBy a blocked launch's. RecordNotWritten adds
// cordon.ErrConformanceRecord to whatever the code says, as a pass whose
// record failed joins it to its own outcome.
type wireError struct {
	Code             errorCode      `json:"code"`
	Message          string         `json:"message"`
	Check            string         `json:"check,omitempty"`
	Reason           string         `json:"reason,omitempty"`
	RunID            string         `json:"run_id,omitempty"`
	EndedBy          cordon.EndedBy `json:"ended_by,omitempty"`
	RecordNotWritten bool           `json:"record_not_written,omitempty"`
}

// encodeError classifies a backend's error for the wire, most specific
// first: a blocked launch wraps the unproven end.
func encodeError(err error) *wireError {
	w := &wireError{Code: codeInternal, Message: boundText(err.Error())}
	var blocked *cordon.BlockedError
	var failed *cordon.ConformanceFailure
	switch {
	case errors.Is(err, cordon.ErrDetached):
		w.Code = codeDetached
	case errors.As(err, &blocked):
		w.Code, w.RunID, w.EndedBy = codeBlocked, blocked.RunID, blocked.EndedBy
	case errors.Is(err, cordon.ErrRunEndUnproven):
		w.Code = codeRunEndUnproven
	case errors.Is(err, cordon.ErrNoRun):
		w.Code = codeNoRun
	case errors.As(err, &failed):
		w.Code, w.Check, w.Reason = codeConformance, boundText(failed.Check), boundText(failed.Reason)
	case errors.Is(err, ErrExportUnsupported):
		w.Code = codeExportUnsupported
	case errors.Is(err, cordon.ErrCanceled):
		w.Code = codeCanceled
	case errors.Is(err, ErrBuildMismatch):
		w.Code = codeBuildMismatch
	}
	w.RecordNotWritten = errors.Is(err, cordon.ErrConformanceRecord)
	return w
}

// decodeError is the error the client returns for a launcher's refusal of
// runID's operation (empty for one that names no run). It wraps what the
// backend's own error would, so the gate maps it the same way.
func decodeError(w *wireError, runID string) error {
	if !w.Code.valid() || !shortText(w.Message) || !shortText(w.Check) || !shortText(w.Reason) {
		return fmt.Errorf("%w: a malformed refusal", ErrProtocol)
	}
	if w.EndedBy != "" && !slices.Contains(cordon.AllEndedBy, w.EndedBy) {
		return fmt.Errorf("%w: a refusal ended by %q", ErrProtocol, w.EndedBy)
	}
	host := fmt.Errorf("the launcher refused: %s", w.Message)
	var err error
	switch w.Code {
	case codeDetached:
		err = fmt.Errorf("%w: %w", cordon.ErrDetached, host)
	case codeBlocked:
		if w.RunID != runID || runID == "" {
			return fmt.Errorf("%w: a blocked refusal for run %q, not %q", ErrProtocol, w.RunID, runID)
		}
		err = &cordon.BlockedError{RunID: w.RunID, EndedBy: w.EndedBy, Err: fmt.Errorf("%w: %w", cordon.ErrRunEndUnproven, host)}
	case codeRunEndUnproven:
		err = fmt.Errorf("%w: %w", cordon.ErrRunEndUnproven, host)
	case codeNoRun:
		err = fmt.Errorf("%w: %w", cordon.ErrNoRun, host)
	case codeConformance:
		if w.Check == "" {
			return fmt.Errorf("%w: a conformance refusal names no check", ErrProtocol)
		}
		err = &cordon.ConformanceFailure{Check: w.Check, Reason: w.Reason}
	case codeCanceled:
		err = fmt.Errorf("%w: %w: %w", cordon.ErrCanceled, cordon.ErrNotExportable, host)
	case codeExportUnsupported:
		err = fmt.Errorf("%w: %w", ErrExportUnsupported, host)
	case codeBuildMismatch:
		err = fmt.Errorf("%w: %w", ErrBuildMismatch, host)
	case codeInternal:
		err = host
	}
	if err == nil {
		return fmt.Errorf("%w: a refusal coded %q", ErrProtocol, w.Code)
	}
	if w.RecordNotWritten {
		err = errors.Join(err, fmt.Errorf("%w: on the launcher", cordon.ErrConformanceRecord))
	}
	return err
}

// boundText is s cut to maxText bytes of valid UTF-8, for text the
// launcher sends.
func boundText(s string) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) <= maxText {
		return s
	}
	cut := maxText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func shortText(s string) bool { return len(s) <= maxText }
