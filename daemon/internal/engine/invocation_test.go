package engine

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestDecodeInvocationRequestRejectsMalformedPayloads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload string
		wantErr error
	}{
		{"empty", ``, nil},
		{"trailing value", `{"invocation_id":"inv-1","conversation_id":"conv-1","item_id":"item-1","item_version":1} {}`, nil},
		{"unknown field", `{"invocation_id":"inv-1","conversation_id":"conv-1","item_id":"item-1","item_version":1,"run_id":"run-foreign"}`, nil},
		{"missing invocation", `{"conversation_id":"conv-1","item_id":"item-1","item_version":1}`, domain.ErrEmptyID},
		{"missing conversation", `{"invocation_id":"inv-1","item_id":"item-1","item_version":1}`, domain.ErrEmptyID},
		{"missing item", `{"invocation_id":"inv-1","conversation_id":"conv-1","item_version":1}`, domain.ErrEmptyID},
		{"zero item version", `{"invocation_id":"inv-1","conversation_id":"conv-1","item_id":"item-1","item_version":0}`, domain.ErrNonPositive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeInvocationRequest([]byte(tc.payload))
			if err == nil {
				t.Fatal("decode accepted malformed payload")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("decode error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestDecodeInvocationRequestAcceptsCanonicalPayload(t *testing.T) {
	t.Parallel()
	got, err := decodeInvocationRequest([]byte(
		`{"invocation_id":"inv-1","conversation_id":"conv-1","item_id":"item-1","item_version":2}`,
	))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.InvocationID != "inv-1" || got.ConversationID != "conv-1" ||
		got.ItemID != "item-1" || got.ItemVersion != 2 {
		t.Fatalf("decoded request = %#v", got)
	}
}
