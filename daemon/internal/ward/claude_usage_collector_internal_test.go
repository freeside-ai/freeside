package ward

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// TestClaudeFableWindowMatchesTheDecoder pins the collector's spelling of the
// Fable window to the decoder's, so a capture's Fable reading is never left
// out as unknown.
func TestClaudeFableWindowMatchesTheDecoder(t *testing.T) {
	t.Parallel()
	if claudeFableWindow != exec.ClaudeFableWindow {
		t.Fatalf("collector Fable window = %q, decoder's = %q", claudeFableWindow, exec.ClaudeFableWindow)
	}
}
