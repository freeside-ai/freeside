package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxOperatorMessageBytes bounds an operator message's text. It is the
// bound the cordon's inbox takes (cordon.MaxMessageBytes is defined from
// it), so a message the record accepts is never refused for its size at
// the run.
const MaxOperatorMessageBytes = 8 << 10

// MaxOperatorMessageCommandIDBytes bounds a message's command ID, the
// sender's idempotency key. The run's inbox record carries it beside the
// text, so the bound keeps the longest message's record, escaped, inside
// the record bound the cordon reads it through.
const MaxOperatorMessageCommandIDBytes = 256

// OperatorMessageState is where an operator's message to a running
// cordoned writer stands. A message is recorded before it is offered to the
// run and settles once, as delivered or refused. The zero value is invalid.
type OperatorMessageState string

const (
	// OperatorMessageRecorded is a message not yet settled: the daemon
	// hasn't offered it, or stopped before the offer answered. The next
	// start offers it again under the same ordinal.
	OperatorMessageRecorded OperatorMessageState = "recorded"
	// OperatorMessageDelivered means the run's supervisor took the message
	// for the writer's input, never that the writer read it. It is at most
	// once: a writer that exits before reading its input never sees it, and
	// neither does one whose supervisor dies after the take, a run that
	// ends lost.
	OperatorMessageDelivered OperatorMessageState = "delivered"
	// OperatorMessageRefused means the message never reached the writer,
	// for the reason the record carries.
	OperatorMessageRefused OperatorMessageState = "refused"
)

// AllOperatorMessageStates registers every OperatorMessageState.
var AllOperatorMessageStates = []OperatorMessageState{
	OperatorMessageRecorded, OperatorMessageDelivered, OperatorMessageRefused,
}

func (s OperatorMessageState) valid() bool {
	switch s {
	case OperatorMessageRecorded, OperatorMessageDelivered, OperatorMessageRefused:
		return true
	default:
		return false
	}
}

// OperatorMessageRefusal is why a recorded message settled refused. The
// zero value is invalid.
type OperatorMessageRefusal string

const (
	// OperatorMessageRefusalRunEnded means the writer's run had ended.
	OperatorMessageRefusalRunEnded OperatorMessageRefusal = "run_ended"
	// OperatorMessageRefusalInputClosed means the writer's turn had ended
	// with nothing waiting, so its input was closed for good.
	OperatorMessageRefusalInputClosed OperatorMessageRefusal = "input_closed"
	// OperatorMessageRefusalNotStreamed means the writer reads no streamed
	// input: a warded writer, or a run an older build started.
	OperatorMessageRefusalNotStreamed OperatorMessageRefusal = "not_streamed"
	// OperatorMessageRefusalNotTaken means the run's supervisor didn't take
	// the message in time, so the daemon withdrew it.
	OperatorMessageRefusalNotTaken OperatorMessageRefusal = "not_taken"
	// OperatorMessageRefusalNotStarted means the writer's launch hadn't
	// reached its supervisor yet.
	OperatorMessageRefusalNotStarted OperatorMessageRefusal = "not_started"
	// OperatorMessageRefusalDeliveryUnknown means a restart found the run's
	// files gone, so nothing says whether the supervisor took it.
	OperatorMessageRefusalDeliveryUnknown OperatorMessageRefusal = "delivery_unknown"
)

// AllOperatorMessageRefusals registers every OperatorMessageRefusal.
var AllOperatorMessageRefusals = []OperatorMessageRefusal{
	OperatorMessageRefusalRunEnded, OperatorMessageRefusalInputClosed, OperatorMessageRefusalNotStreamed,
	OperatorMessageRefusalNotTaken, OperatorMessageRefusalNotStarted, OperatorMessageRefusalDeliveryUnknown,
}

func (r OperatorMessageRefusal) valid() bool {
	switch r {
	case OperatorMessageRefusalRunEnded, OperatorMessageRefusalInputClosed, OperatorMessageRefusalNotStreamed,
		OperatorMessageRefusalNotTaken, OperatorMessageRefusalNotStarted, OperatorMessageRefusalDeliveryUnknown:
		return true
	default:
		return false
	}
}

// ValidateOperatorMessageText refuses text that is empty or only
// whitespace, isn't UTF-8, or is over MaxOperatorMessageBytes.
func ValidateOperatorMessageText(text string) error {
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) || len(text) > MaxOperatorMessageBytes {
		return fmt.Errorf("%w: it must be UTF-8 text of 1 to %d bytes", ErrInvalidOperatorMessageText,
			MaxOperatorMessageBytes)
	}
	return nil
}

// OperatorMessageRequest is a message as its sender sent it, to one writer
// invocation of one run. CommandID is the sender's idempotency key, and
// Sender names who sent it: "device:" and a paired device's id, or "cli:"
// and the account that ran the shell verb.
type OperatorMessageRequest struct {
	CommandID    string       `json:"command_id"`
	Sender       string       `json:"sender"`
	RunID        RunID        `json:"run_id"`
	InvocationID InvocationID `json:"invocation_id"`
	Text         string       `json:"text"`
}

// Validate checks every field is set, the command ID is within its bound,
// and the text is one a writer may be sent.
func (r OperatorMessageRequest) Validate() error {
	if r.CommandID == "" || r.Sender == "" || r.RunID == "" || r.InvocationID == "" {
		return fmt.Errorf("operator message: %w", ErrEmptyID)
	}
	if len(r.CommandID) > MaxOperatorMessageCommandIDBytes {
		return fmt.Errorf("operator message: %w", ErrInvalidOperatorMessageCommandID)
	}
	return ValidateOperatorMessageText(r.Text)
}

// OperatorMessage is the record of one operator message. Ordinal orders an
// invocation's messages from 1 and is the key the run's inbox holds it
// under. SettledAt is set exactly when State is no longer recorded, and
// Refusal exactly when it is refused.
type OperatorMessage struct {
	OperatorMessageRequest
	Ordinal    int64                   `json:"ordinal"`
	RecordedAt time.Time               `json:"recorded_at"`
	State      OperatorMessageState    `json:"state"`
	SettledAt  *time.Time              `json:"settled_at"`
	Refusal    *OperatorMessageRefusal `json:"refusal"`
}

// NewOperatorMessage records a request as the invocation's message ordinal,
// not yet settled.
func NewOperatorMessage(request OperatorMessageRequest, ordinal int64, now time.Time) (OperatorMessage, error) {
	m := OperatorMessage{
		OperatorMessageRequest: request, Ordinal: ordinal, RecordedAt: now, State: OperatorMessageRecorded,
	}
	return m, m.Validate()
}

// Validate checks the request, the ordinal, and that the settled fields
// match the state.
func (m OperatorMessage) Validate() error {
	if err := m.OperatorMessageRequest.Validate(); err != nil {
		return err
	}
	if m.Ordinal < 1 || m.RecordedAt.IsZero() {
		return fmt.Errorf("operator message %q: %w", m.CommandID, ErrInvalidOperatorMessageSettlement)
	}
	if !m.State.valid() {
		return fmt.Errorf("operator message %q: %w", m.CommandID, ErrInvalidOperatorMessageState)
	}
	settled := m.State != OperatorMessageRecorded
	if settled != (m.SettledAt != nil) || (m.SettledAt != nil && m.SettledAt.Before(m.RecordedAt)) {
		return fmt.Errorf("operator message %q: %w", m.CommandID, ErrInvalidOperatorMessageSettlement)
	}
	if (m.State == OperatorMessageRefused) != (m.Refusal != nil) {
		return fmt.Errorf("operator message %q: %w", m.CommandID, ErrInvalidOperatorMessageSettlement)
	}
	if m.Refusal != nil && !m.Refusal.valid() {
		return fmt.Errorf("operator message %q: %w", m.CommandID, ErrInvalidOperatorMessageRefusal)
	}
	return nil
}

// Settle returns the record settled as delivered (refusal nil) or refused
// for refusal. Only a recorded message settles. A now before the record's
// RecordedAt, from a wall clock stepped back while the offer ran, settles
// at RecordedAt: the run already answered, and refusing the settlement
// would leave the answer unrecorded.
func (m OperatorMessage) Settle(refusal *OperatorMessageRefusal, now time.Time) (OperatorMessage, error) {
	if m.State != OperatorMessageRecorded {
		return OperatorMessage{}, fmt.Errorf("operator message %q is %s: %w", m.CommandID, m.State,
			ErrOperatorMessageSettled)
	}
	if now.Before(m.RecordedAt) {
		now = m.RecordedAt
	}
	m.State, m.SettledAt, m.Refusal = OperatorMessageDelivered, &now, nil
	if refusal != nil {
		reason := *refusal
		m.State, m.Refusal = OperatorMessageRefused, &reason
	}
	return m, m.Validate()
}
