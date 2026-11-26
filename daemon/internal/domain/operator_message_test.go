package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

var (
	operatorMessageRecordedAt = time.Date(2026, 11, 26, 17, 0, 0, 0, time.UTC)
	operatorMessageSettledAt  = operatorMessageRecordedAt.Add(2 * time.Second)
)

func operatorMessageRequest() domain.OperatorMessageRequest {
	return domain.OperatorMessageRequest{
		CommandID: "cmd-1", Sender: "device:dev-1", RunID: "run-1", InvocationID: "inv-1",
		Text: "Stop editing the parser; the failing test is in the lexer.",
	}
}

func recordedOperatorMessage(t *testing.T) domain.OperatorMessage {
	t.Helper()
	m, err := domain.NewOperatorMessage(operatorMessageRequest(), 1, operatorMessageRecordedAt)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestOperatorMessageGoldens pins the record in each state; every fixture
// is also a validation-positive case.
func TestOperatorMessageGoldens(t *testing.T) {
	t.Parallel()
	recorded := recordedOperatorMessage(t)
	delivered, err := recorded.Settle(nil, operatorMessageSettledAt)
	if err != nil {
		t.Fatal(err)
	}
	notTaken := domain.OperatorMessageRefusalNotTaken
	refused, err := recorded.Settle(&notTaken, operatorMessageSettledAt)
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]domain.OperatorMessage{
		"operator_message_recorded": recorded, "operator_message_delivered": delivered,
		"operator_message_refused": refused,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := m.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			body, err := json.MarshalIndent(m, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, name, append(body, '\n'))
		})
	}
}

func TestOperatorMessageRefusesBadText(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"empty": "", "only whitespace": " \n\t", "not UTF-8": "go \xff on",
		"over the limit": strings.Repeat("x", domain.MaxOperatorMessageBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			request := operatorMessageRequest()
			request.Text = text
			if _, err := domain.NewOperatorMessage(request, 1, operatorMessageRecordedAt); !errors.Is(err,
				domain.ErrInvalidOperatorMessageText) {
				t.Fatalf("NewOperatorMessage = %v, want ErrInvalidOperatorMessageText", err)
			}
		})
	}
	request := operatorMessageRequest()
	request.Text = strings.Repeat("x", domain.MaxOperatorMessageBytes)
	if _, err := domain.NewOperatorMessage(request, 1, operatorMessageRecordedAt); err != nil {
		t.Fatalf("a message at the limit: %v", err)
	}
}

func TestOperatorMessageBoundsItsCommandID(t *testing.T) {
	t.Parallel()
	request := operatorMessageRequest()
	request.CommandID = strings.Repeat("c", domain.MaxOperatorMessageCommandIDBytes)
	if _, err := domain.NewOperatorMessage(request, 1, operatorMessageRecordedAt); err != nil {
		t.Fatalf("a command ID at the limit: %v", err)
	}
	request.CommandID += "c"
	if _, err := domain.NewOperatorMessage(request, 1, operatorMessageRecordedAt); !errors.Is(err,
		domain.ErrInvalidOperatorMessageCommandID) {
		t.Fatalf("NewOperatorMessage = %v, want ErrInvalidOperatorMessageCommandID", err)
	}
}

func TestOperatorMessageRefusesAnInconsistentRecord(t *testing.T) {
	t.Parallel()
	settledAt := operatorMessageSettledAt
	early := operatorMessageRecordedAt.Add(-time.Second)
	runEnded := domain.OperatorMessageRefusalRunEnded
	unknown := domain.OperatorMessageRefusal("lost")
	for name, tc := range map[string]struct {
		edit func(*domain.OperatorMessage)
		want error
	}{
		"no sender":                {func(m *domain.OperatorMessage) { m.Sender = "" }, domain.ErrEmptyID},
		"ordinal zero":             {func(m *domain.OperatorMessage) { m.Ordinal = 0 }, domain.ErrInvalidOperatorMessageSettlement},
		"no recorded time":         {func(m *domain.OperatorMessage) { m.RecordedAt = time.Time{} }, domain.ErrInvalidOperatorMessageSettlement},
		"an unknown state":         {func(m *domain.OperatorMessage) { m.State = "sent" }, domain.ErrInvalidOperatorMessageState},
		"recorded with a time":     {func(m *domain.OperatorMessage) { m.SettledAt = &settledAt }, domain.ErrInvalidOperatorMessageSettlement},
		"delivered without a time": {func(m *domain.OperatorMessage) { m.State = domain.OperatorMessageDelivered }, domain.ErrInvalidOperatorMessageSettlement},
		"settled before recorded":  {func(m *domain.OperatorMessage) { m.State, m.SettledAt = domain.OperatorMessageDelivered, &early }, domain.ErrInvalidOperatorMessageSettlement},
		"delivered with a refusal": {func(m *domain.OperatorMessage) {
			m.State, m.SettledAt, m.Refusal = domain.OperatorMessageDelivered, &settledAt, &runEnded
		}, domain.ErrInvalidOperatorMessageSettlement},
		"refused without a refusal": {func(m *domain.OperatorMessage) { m.State, m.SettledAt = domain.OperatorMessageRefused, &settledAt }, domain.ErrInvalidOperatorMessageSettlement},
		"an unknown refusal": {func(m *domain.OperatorMessage) {
			m.State, m.SettledAt, m.Refusal = domain.OperatorMessageRefused, &settledAt, &unknown
		}, domain.ErrInvalidOperatorMessageRefusal},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := recordedOperatorMessage(t)
			tc.edit(&m)
			if err := m.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOperatorMessageSettlesOnce(t *testing.T) {
	t.Parallel()
	delivered, err := recordedOperatorMessage(t).Settle(nil, operatorMessageSettledAt)
	if err != nil {
		t.Fatal(err)
	}
	notTaken := domain.OperatorMessageRefusalNotTaken
	if _, err := delivered.Settle(&notTaken, operatorMessageSettledAt); !errors.Is(err, domain.ErrOperatorMessageSettled) {
		t.Fatalf("a second settle = %v, want ErrOperatorMessageSettled", err)
	}
	// A clock stepped back while the offer ran still records the answer.
	stepped, err := recordedOperatorMessage(t).Settle(&notTaken, operatorMessageRecordedAt.Add(-time.Minute))
	if err != nil || !stepped.SettledAt.Equal(operatorMessageRecordedAt) {
		t.Fatalf("settle before RecordedAt = %+v, %v; want settled at RecordedAt", stepped, err)
	}
}
