package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func TestGoldenOperatorFeedbackIntent(t *testing.T) {
	request := OperatorFeedbackInvocationIntent{
		Version:      OperatorFeedbackInvocationIntentVersion,
		InvocationID: "inv-operator-feedback-command", RunID: "run-1",
		StageID:   "operator-feedback-inv-operator-feedback-command",
		CommandID: "command", ItemID: "ready", SourceInvocationID: "inv-implement-run-1",
		InputArtifactID:     "operator-feedback-command",
		InputArtifactDigest: Digest("sha256:" + strings.Repeat("a", 64)),
	}
	for _, withHeads := range []bool{false, true} {
		name := "operator_feedback_invocation_intent"
		fixture := request
		if withHeads {
			name += "_with_heads"
			fixture.BaseSHA = strings.Repeat("1", 40)
			fixture.HeadSHA = strings.Repeat("2", 40)
		}
		t.Run(name, func(t *testing.T) {
			if err := fixture.Validate(); err != nil {
				t.Fatalf("invalid golden fixture: %v", err)
			}
			body, err := json.MarshalIndent(fixture, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, name, append(body, '\n'))
		})
	}
}

func TestOperatorFeedbackIntentAuthentication(t *testing.T) {
	request := OperatorFeedbackInvocationIntent{
		Version:      OperatorFeedbackInvocationIntentVersion,
		InvocationID: "inv-operator-feedback-command", RunID: "run-1",
		StageID:   "operator-feedback-inv-operator-feedback-command",
		CommandID: "command", ItemID: "ready", SourceInvocationID: "inv-implement-run-1",
		InputArtifactID:     "operator-feedback-command",
		InputArtifactDigest: Digest("sha256:" + strings.Repeat("a", 64)),
		BaseSHA:             strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
	}
	authenticate := func(r OperatorFeedbackInvocationIntent, mutate func(*InvocationDispatchIntent)) error {
		payload, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		entry := InvocationDispatchIntent{
			Kind:           string(OperatorFeedbackInvocationRequestedKind),
			IdempotencyKey: string(request.InvocationID), Payload: payload,
		}
		if mutate != nil {
			mutate(&entry)
		}
		return AuthenticateInvocationDispatchIntent(entry, request.InvocationID, request.RunID, request.StageID)
	}
	if err := authenticate(request, nil); err != nil {
		t.Fatal(err)
	}
	err := authenticate(request, func(e *InvocationDispatchIntent) {
		e.Payload = bytes.Replace(e.Payload, []byte(`"item_id":"ready"`), []byte{'"', 'i', 't', 'e', 'm', '_', 'i', 'd', '"', ':', '"', 0xff, '"'}, 1)
	})
	if !errors.Is(err, strictjson.ErrInvalidUTF8) {
		t.Fatalf("invalid UTF-8 inside string: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*OperatorFeedbackInvocationIntent)
	}{
		{"version", func(r *OperatorFeedbackInvocationIntent) { r.Version = "v2" }},
		{"run", func(r *OperatorFeedbackInvocationIntent) { r.RunID = "other" }},
		{"stage", func(r *OperatorFeedbackInvocationIntent) { r.StageID = "other" }},
		{"invocation", func(r *OperatorFeedbackInvocationIntent) { r.InvocationID = "other" }},
		{"command", func(r *OperatorFeedbackInvocationIntent) { r.CommandID = "other" }},
		{"artifact", func(r *OperatorFeedbackInvocationIntent) { r.InputArtifactID = "other" }},
		{"digest", func(r *OperatorFeedbackInvocationIntent) { r.InputArtifactDigest = "untrusted" }},
		{"missing item", func(r *OperatorFeedbackInvocationIntent) { r.ItemID = "" }},
		{"missing source", func(r *OperatorFeedbackInvocationIntent) { r.SourceInvocationID = "" }},
		{"partial head", func(r *OperatorFeedbackInvocationIntent) { r.HeadSHA = "" }},
		{"invalid head", func(r *OperatorFeedbackInvocationIntent) { r.HeadSHA = strings.Repeat("z", 40) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request
			tc.change(&r)
			if err := authenticate(r, nil); err == nil {
				t.Fatal("accepted substituted or incomplete intent")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*InvocationDispatchIntent)
	}{
		{"key", func(e *InvocationDispatchIntent) { e.IdempotencyKey = "other" }},
		{"kind", func(e *InvocationDispatchIntent) { e.Kind = "unknown" }},
		{"unknown field", func(e *InvocationDispatchIntent) {
			e.Payload = append(e.Payload[:len(e.Payload)-1], []byte(`,"trusted":true}`)...)
		}},
		{"noncanonical", func(e *InvocationDispatchIntent) { e.Payload = append([]byte(" "), e.Payload...) }},
		{"invalid UTF8", func(e *InvocationDispatchIntent) { e.Payload = append(e.Payload[:len(e.Payload)-1], 0xff, '}') }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := authenticate(request, tc.change); err == nil {
				t.Fatal("accepted malformed intent")
			}
		})
	}
}
