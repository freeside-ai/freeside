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

// The file protocol carries a prompt far past the argument limit whole, on
// stdin, and enforces its own finite limit.
func TestPromptFileCarriesTheCompletePrompt(t *testing.T) {
	t.Parallel()
	inputs := stage.ProviderPromptInputs{
		Delivery: launchDelivery, PromptPackage: bytes.Repeat([]byte("'α\n"), 12000),
	}
	prompt, err := renderPromptParts(inputs)
	if err != nil || !strings.Contains(prompt, string(inputs.PromptPackage)) {
		t.Fatalf("file protocol lost complete input: %v", err)
	}
	in := testProviderHandoffInput()
	in.Prompt = prompt
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
		t.Fatal("transport is not bounded user stdin delivery")
	}
	inputs.PromptPackage = bytes.Repeat([]byte("x"), ward.MaxPromptFileBytes)
	if _, err := renderPromptParts(inputs); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatal("transport did not enforce its own finite limit")
	}
}

// Only file_v8 renders or launches. A removed delivery, including the empty
// value an intent written before deliveries existed carries, is refused and
// never read as the current one.
func TestUnknownPromptDeliveryIsRejected(t *testing.T) {
	t.Parallel()
	// The same fixtures pass on file_v8, so each refusal below is the
	// delivery's.
	current := stage.ProviderPromptInputs{
		Delivery: launchDelivery, PromptPackage: []byte("prompt"),
		Specification: []byte("specification"), Policy: []byte("policy"),
	}
	if _, err := renderPromptParts(current); err != nil {
		t.Fatalf("renderer refused file_v8: %v", err)
	}
	if _, err := (claudeProvider{volumes: testAuthStoreVolumes{}}).HandoffSpec(t.Context(), testProviderHandoffInput()); err != nil {
		t.Fatalf("handoff refused file_v8: %v", err)
	}
	for _, delivery := range []stage.PromptDelivery{
		"", "argument", "file_v1", "file_v2", "file_v3", "file_v4", "file_v5", "file_v6", "file_v7", "unrecognized",
	} {
		inputs := stage.ProviderPromptInputs{
			Delivery: delivery, PromptPackage: []byte("prompt"),
			Specification: []byte("specification"), Policy: []byte("policy"),
		}
		if _, err := renderPromptParts(inputs); !errors.Is(err, ErrUnsupportedStart) {
			t.Errorf("renderer accepted prompt delivery %q: %v", delivery, err)
		}
		in := testProviderHandoffInput()
		in.PromptDelivery = delivery
		if _, err := (claudeProvider{volumes: testAuthStoreVolumes{}}).HandoffSpec(t.Context(), in); !errors.Is(err, ErrUnsupportedStart) {
			t.Errorf("handoff accepted prompt delivery %q: %v", delivery, err)
		}
	}
}
