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

// Both file protocols bind the provider's prompt file to the durable prompt,
// and the argument protocol refuses one. A stored file_v1 intent must keep
// passing after file_v2 becomes what new launches select.
func TestProviderCannotSubstitutePromptFile(t *testing.T) {
	for _, mode := range []PromptDelivery{PromptFileV1, PromptFileV2} {
		for _, change := range []string{"none", "missing", "wrong_bytes", "wrong_digest", "legacy_mode"} {
			t.Run(string(mode)+"/"+change, func(t *testing.T) {
				d := newTestDriver(t, &stubGate{}, newStubExports())
				in := testHandoffIntent(t, d)
				in.PromptDelivery = mode
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
				hs, err := d.handoffSpec(t.Context(), in)
				if change == "none" {
					if err != nil || hs.Agent.PromptFile == nil {
						t.Fatalf("stored %s intent lost its prompt file: %v", mode, err)
					}
					return
				}
				if !errors.Is(err, ErrUnsupportedStart) {
					t.Fatalf("provider prompt substitution accepted: %v", err)
				}
			})
		}
	}
}
