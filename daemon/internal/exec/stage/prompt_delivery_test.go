package stage

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestPromptDeliveryModesAndLegacyRecord(t *testing.T) {
	for _, mode := range AllPromptDeliveries {
		if !mode.valid() {
			t.Fatalf("registered mode %q is invalid", mode)
		}
	}
	if PromptDelivery("").valid() {
		t.Fatal("zero mode must not be valid protocol vocabulary")
	}
	d := newTestDriver(t, &stubGate{}, newStubExports())
	in := testHandoffIntent(t, d)
	if in.delivery() != PromptArgument {
		t.Fatal("legacy record changed delivery")
	}
	for _, mode := range AllPromptDeliveries {
		in.PromptDelivery = mode
		body, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var restored intent
		if err := json.Unmarshal(body, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.delivery() != mode || providerHandoffInputFrom(restored).PromptDelivery != mode {
			t.Fatal("durable mode did not reach reconstruction")
		}
	}
	in.PromptDelivery = "unrecognized"
	if in.validate() == nil {
		t.Fatal("unknown delivery accepted")
	}
}

func TestProviderCannotSubstitutePromptFile(t *testing.T) {
	for _, change := range []string{"missing", "wrong_bytes", "wrong_digest", "legacy_mode"} {
		t.Run(change, func(t *testing.T) {
			d := newTestDriver(t, &stubGate{}, newStubExports())
			in := testHandoffIntent(t, d)
			in.PromptDelivery = PromptFileV1
			if change == "legacy_mode" {
				in.PromptDelivery = ""
			}
			d.provider = testProvider{handoffMutate: func(hs *ward.HandoffSpec) {
				if change == "missing" {
					return
				}
				hs.Agent.PromptFile = ward.NewPromptFile([]byte(in.Prompt))
				if change == "wrong_bytes" {
					hs.Agent.PromptFile = ward.NewPromptFile([]byte("other prompt"))
				}
				if change == "wrong_digest" {
					hs.Agent.PromptFile.Digest = "sha256:bad"
				}
			}}
			if _, err := d.handoffSpec(t.Context(), in); !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("provider prompt substitution accepted: %v", err)
			}
		})
	}
}
