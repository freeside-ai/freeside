package stage

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

// intentFields returns the top-level JSON fields of in's state file, so a
// test can write the same file in a removed shape.
func intentFields(t *testing.T, in intent) map[string]json.RawMessage {
	t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func decodeFields(t *testing.T, fields map[string]json.RawMessage) error {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	_, err = decodeIntent(body)
	return err
}

// TestDecodeRefusesRemovedPromptDeliveries proves a state file with no prompt
// delivery, or with a delivery the daemon no longer launches, fails to
// decode instead of being read as the current one. Each case changes only
// the delivery of a current intent, so the refusal has that one cause.
func TestDecodeRefusesRemovedPromptDeliveries(t *testing.T) {
	t.Parallel()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	fields := intentFields(t, testHandoffIntent(t, d))
	if err := decodeFields(t, fields); err != nil {
		t.Fatalf("current intent refused: %v", err)
	}
	delete(fields, "prompt_delivery")
	if err := decodeFields(t, fields); err == nil || !strings.Contains(err.Error(), "prompt delivery") {
		t.Fatalf("intent without a prompt delivery = %v, want refused for its delivery", err)
	}
	for _, removed := range []string{
		"argument", "file_v1", "file_v2", "file_v3", "file_v4", "file_v5", "file_v6", "file_v7",
	} {
		fields["prompt_delivery"] = json.RawMessage(strconv.Quote(removed))
		if err := decodeFields(t, fields); err == nil || !strings.Contains(err.Error(), "prompt delivery") {
			t.Errorf("intent with prompt delivery %q = %v, want refused for its delivery", removed, err)
		}
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
	fields["prompt_delivery"] = json.RawMessage(`"file_v8"`)
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
