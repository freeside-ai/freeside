package stage

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestStartRecordsProtectedPathsAndReconstructs: a new start renders the
// authority's list into the prompt and records it in the intent, which then
// reconstructs; an intent recorded before the list existed carries no field
// and reconstructs its original prompt.
func TestStartRecordsProtectedPathsAndReconstructs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("new intent", func(t *testing.T) {
		t.Parallel()
		entered := make(chan struct{})
		gate := &stubGate{handoffCtxFn: func(ctx context.Context, _ ward.HandoffSpec) (*ward.HandoffResult, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		d := newTestDriver(t, gate, newStubExports())
		protected := []string{"**/.gitattributes", ".github/workflows/**"}
		d.authority = stubAuthority{protected: protected}
		spec := testStartSpec()
		inputs := stageInputs(t, &spec)
		if err := d.StartWithInputs(ctx, testInvoke, spec,
			func(context.Context) (exec.StageInputs, error) { return inputs, nil },
		); err != nil {
			t.Fatalf("StartWithInputs: %v", err)
		}
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("handoff did not start")
		}
		t.Cleanup(func() { _ = d.Close(context.Background()) })
		raw, err := os.ReadFile(d.intentPath(testRunIDFor(testInvoke)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"protected_paths":["**/.gitattributes",".github/workflows/**"]`) {
			t.Fatalf("intent does not record the list:\n%s", raw)
		}
		in, err := d.loadIntent(ctx, testInvoke)
		if err != nil {
			t.Fatalf("reconstruct: %v", err)
		}
		if !slices.Equal(in.Inputs.ProtectedPaths, protected) ||
			!strings.HasSuffix(in.Prompt, "--- Protected paths ---\n\n**/.gitattributes\n.github/workflows/**\n") {
			t.Fatalf("reconstructed list %q, prompt:\n%s", in.Inputs.ProtectedPaths, in.Prompt)
		}
	})
	t.Run("intent recorded before the list", func(t *testing.T) {
		t.Parallel()
		d := newTestDriver(t, &stubGate{}, newStubExports())
		recorded := orphan(t, d, phaseSeeding, nil)
		raw, err := os.ReadFile(d.intentPath(recorded.RunID))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "protected_paths") {
			t.Fatalf("legacy fixture carries the field:\n%s", raw)
		}
		in, err := d.loadIntent(ctx, testInvoke)
		if err != nil {
			t.Fatalf("reconstruct: %v", err)
		}
		if in.Prompt != recorded.Prompt || strings.Contains(in.Prompt, "Protected paths") {
			t.Fatalf("legacy prompt changed:\n%s", in.Prompt)
		}
	})
	t.Run("refused read commits no intent", func(t *testing.T) {
		t.Parallel()
		d := newTestDriver(t, &stubGate{}, newStubExports())
		refused := errors.New("trust profile unavailable")
		d.authority = stubAuthority{protectedErr: refused}
		spec := testStartSpec()
		inputs := stageInputs(t, &spec)
		err := d.StartWithInputs(ctx, testInvoke, spec,
			func(context.Context) (exec.StageInputs, error) { return inputs, nil })
		if !errors.Is(err, refused) {
			t.Fatalf("StartWithInputs = %v, want the authority's refusal", err)
		}
		if _, err := os.Stat(d.intentPath(testRunIDFor(testInvoke))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused start left an intent: %v", err)
		}
	})
}
