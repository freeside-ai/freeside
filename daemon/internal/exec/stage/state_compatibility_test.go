package stage

import (
	"bytes"
	"os"
	"testing"
)

func TestDecodePreExtractionIntentFixture(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("testdata/pre-extraction-intent.json")
	if err != nil {
		t.Fatalf("read pre-extraction intent: %v", err)
	}
	in, err := decodeIntent(body)
	if err != nil {
		t.Fatalf("decode pre-extraction intent: %v", err)
	}
	if in.RunID != "c0123456789abcdef0123456789abcde" {
		t.Fatalf("run ID = %q, want persisted pre-extraction identity", in.RunID)
	}
	if len(in.Preparation) != 0 {
		t.Fatalf("omitted preparation decoded as %v, want empty", in.Preparation)
	}
	if !bytes.Equal(in.Inputs.Specification, []byte("specification")) ||
		!bytes.Equal(in.Inputs.PromptPackage, []byte("prompt package")) ||
		!bytes.Equal(in.Inputs.Policy, []byte("policy")) {
		t.Fatalf("decoded durable inputs = %#v, want fixture bytes", in.Inputs)
	}
}
