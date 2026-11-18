package stage

import (
	"encoding/json"
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

// TestDecodeRefusesExportWithoutRecordedAt proves an exported state file with
// no export stamp fails to decode instead of falling back to the start time.
// The case removes only the stamp from a current exported intent, so the
// refusal has that one cause.
func TestDecodeRefusesExportWithoutRecordedAt(t *testing.T) {
	t.Parallel()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	in := testHandoffIntent(t, d)
	in.Phase = phaseExported
	in.Export = &releasedExport{RecordedAt: in.RecordedAt, Dir: "export"}
	fields := intentFields(t, in)
	if err := decodeFields(t, fields); err != nil {
		t.Fatalf("current exported intent refused: %v", err)
	}
	var export map[string]json.RawMessage
	if err := json.Unmarshal(fields["export"], &export); err != nil {
		t.Fatal(err)
	}
	delete(export, "recorded_at")
	body, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	fields["export"] = body
	if err := decodeFields(t, fields); err == nil || !strings.Contains(err.Error(), "recorded_at") {
		t.Fatalf("export without recorded_at = %v, want refused for its stamp", err)
	}
}
