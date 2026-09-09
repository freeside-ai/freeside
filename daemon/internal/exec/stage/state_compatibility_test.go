package stage

import (
	"bytes"
	"encoding/json"
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

func TestDecodeLegacyExportRecordedAt(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("testdata/pre-extraction-intent.json")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	fields["phase"] = json.RawMessage(`"exported"`)
	fields["export"] = json.RawMessage(`{"dir":"legacy-export"}`)
	body, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	in, err := decodeIntent(body)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Export.RecordedAt.IsZero() || !in.exportRecordedAt().Equal(in.RecordedAt) {
		t.Fatalf("legacy export time = %v, want start time %v", in.exportRecordedAt(), in.RecordedAt)
	}
	exportBody, err := json.Marshal(in.Export)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(exportBody, []byte(`"recorded_at"`)) {
		t.Fatal("zero export timestamp must remain omitted on legacy records")
	}
}
