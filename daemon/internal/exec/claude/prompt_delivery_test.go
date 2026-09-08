package claude

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestPromptDeliveryPreservesLegacyRefusalAndSupportsCompleteFile(t *testing.T) {
	inputs := stage.ProviderPromptInputs{PromptPackage: bytes.Repeat([]byte("'α\n"), 12000)}
	inputs.Delivery = stage.PromptArgument
	if _, err := renderPromptParts(inputs); !errors.Is(err, ErrUnsupportedStart) || !strings.Contains(err.Error(), "limit 31744") {
		t.Fatalf("legacy protocol no longer gives its original refusal: %v", err)
	}
	inputs.Delivery = stage.PromptFileV1
	prompt, err := renderPromptParts(inputs)
	if err != nil || !strings.Contains(prompt, string(inputs.PromptPackage)) {
		t.Fatalf("file protocol lost complete input: %v", err)
	}
	in := testProviderHandoffInput()
	in.Prompt, in.PromptDelivery = prompt, stage.PromptFileV1
	hs, err := (claudeProvider{volumes: testAuthStoreVolumes{}}).HandoffSpec(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if hs.Agent.PromptFile == nil || !bytes.Equal(hs.Agent.PromptFile.Body, []byte(prompt)) {
		t.Fatal("handoff lost rendered prompt bytes")
	}
	command := strings.Join(hs.Agent.Command, " ")
	if len(command) >= linuxMaxArgumentBytes || strings.Contains(command, string(inputs.PromptPackage)) ||
		!strings.Contains(command, "claude -p < "+shellQuote(ward.PromptFilePath)) {
		t.Fatal("new transport is not bounded user stdin delivery")
	}
	inputs.PromptPackage = bytes.Repeat([]byte("x"), ward.MaxPromptFileBytes)
	if _, err := renderPromptParts(inputs); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatal("new transport did not enforce its own finite limit")
	}
}

func TestPromptFileDoesNotChangeLegacyHandoff(t *testing.T) {
	in := testProviderHandoffInput()
	provider := claudeProvider{volumes: testAuthStoreVolumes{}}
	legacy, err := provider.HandoffSpec(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	in.PromptDelivery = stage.PromptArgument
	explicit, err := provider.HandoffSpec(t.Context(), in)
	if err != nil || legacy.Agent.PromptFile != nil || explicit.Agent.PromptFile != nil ||
		strings.Join(legacy.Agent.Command, "\x00") != strings.Join(explicit.Agent.Command, "\x00") {
		t.Fatalf("missing legacy mode changed command: %v", err)
	}
}

func TestUnknownPromptDeliveryIsRejected(t *testing.T) {
	if _, err := renderPromptParts(stage.ProviderPromptInputs{Delivery: "unrecognized"}); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatalf("renderer accepted unknown prompt delivery: %v", err)
	}
	in := testProviderHandoffInput()
	in.PromptDelivery = "unrecognized"
	if _, err := (claudeProvider{volumes: testAuthStoreVolumes{}}).HandoffSpec(t.Context(), in); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatalf("handoff accepted unknown prompt delivery: %v", err)
	}
}
