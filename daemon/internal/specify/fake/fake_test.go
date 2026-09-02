package fake_test

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
)

func TestScriptUsesSharedCanonicalContract(t *testing.T) {
	driver := fake.NewStageDriver()
	want := specify.Output{Specification: &specify.Specification{
		Summary: "Ready", Body: "# Specification\n\nImplement it.", Addressals: []specify.Addressal{},
	}}
	if err := specifyfake.Script(driver, "inv-1", 0, 0, want); err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(t.Context(), "inv-1", exec.StartSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Inspect(t.Context(), "inv-1"); err != nil {
		t.Fatal(err)
	}
	result, err := driver.Collect(t.Context(), "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary == "" {
		t.Fatal("result summary is empty")
	}
	transcript, err := driver.Stream(t.Context(), "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := specify.DecodeTranscript(transcript)
	if closeErr := transcript.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.Specification.Body != want.Specification.Body {
		t.Fatalf("specification body = %q", got.Specification.Body)
	}
}
