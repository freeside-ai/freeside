package verify_test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func TestParseReportCanonicalRoundTrip(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/verify_report.golden")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := verify.ParseReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil || !bytes.Equal(append(encoded, '\n'), raw) {
		t.Fatalf("round trip changed the verifier artifact: %v", err)
	}
	for name, invalid := range map[string][]byte{
		"unknown field":     bytes.Replace(raw, []byte(`"head_sha"`), []byte(`"unknown"`), 1),
		"duplicate field":   bytes.Replace(raw, []byte("{\n"), []byte("{\n  \"outcome\": \"passed\",\n"), 1),
		"invalid outcome":   bytes.Replace(raw, []byte(`"passed"`), []byte(`"pending"`), 1),
		"unknown finding":   bytes.Replace(raw, []byte(`"verification_control_path"`), []byte(`"advisory"`), 1),
		"empty finding":     bytes.Replace(raw, []byte(`"verification_control_path"`), []byte(`""`), 1),
		"empty head":        bytes.Replace(raw, []byte(rep.HeadSHA), nil, 1),
		"empty recipe":      bytes.Replace(raw, []byte(rep.RecipeDigest), nil, 1),
		"empty executable":  bytes.Replace(raw, []byte(`"go"`), []byte(`""`), 1),
		"unknown room":      bytes.Replace(raw, []byte(`"project_image"`), []byte(`"linux_vm"`), 1),
		"room case variant": bytes.Replace(raw, []byte(`"project_image"`), []byte(`"Project_Image"`), 1),
		"trailing value":    append(append([]byte(nil), raw...), []byte("{}")...),
		"noncanonical":      bytes.TrimSpace(raw),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verify.ParseReport(invalid); err == nil {
				t.Fatal("accepted invalid report")
			}
		})
	}
}

// TestParseReportAcceptsEveryFindingKind keeps the finding-kind gate from
// rejecting a report the verifier itself writes.
func TestParseReportAcceptsEveryFindingKind(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/verify_report.golden")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := verify.ParseReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	rep.Findings = nil
	for _, kind := range verify.AllFindingKinds {
		rep.Findings = append(rep.Findings, verify.Finding{Path: "go.mod", Kind: kind})
	}
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify.ParseReport(append(encoded, '\n')); err != nil {
		t.Fatalf("rejected a report carrying every finding kind: %v", err)
	}
}

// TestParseReportReadsRoomlessStepsAsProjectImage pins that a report
// stored before steps carried a room still parses with its stored
// bytes, so its artifact digest holds, and reads every step as the
// project image, the only room there was.
func TestParseReportReadsRoomlessStepsAsProjectImage(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/verify_report.golden")
	if err != nil {
		t.Fatal(err)
	}
	legacy := bytes.ReplaceAll(raw, []byte("      \"room\": \"project_image\",\n"), nil)
	if bytes.Equal(legacy, raw) || bytes.Contains(legacy, []byte(`"room"`)) {
		t.Fatal("fixture did not strip every step's room")
	}
	rep, err := verify.ParseReport(legacy)
	if err != nil {
		t.Fatalf("rejected a stored report without step rooms: %v", err)
	}
	if len(rep.Steps) == 0 {
		t.Fatal("legacy report parsed with no steps")
	}
	for i, step := range rep.Steps {
		if step.Room != verify.ClassProjectImage {
			t.Errorf("step %d room = %q, want project_image", i, step.Room)
		}
	}
}

// TestParseReportAcceptsEveryExecutionClass keeps the room gate from
// rejecting a report the verifier itself writes.
func TestParseReportAcceptsEveryExecutionClass(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/verify_report.golden")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := verify.ParseReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	rep.Steps = nil
	for _, class := range verify.AllExecutionClasses {
		rep.Steps = append(rep.Steps, verify.Step{Argv: []string{"true"}, Room: class})
	}
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify.ParseReport(append(encoded, '\n')); err != nil {
		t.Fatalf("rejected a report carrying every execution class: %v", err)
	}
}
