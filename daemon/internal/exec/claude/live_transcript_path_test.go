package claude

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestLiveReadTargetsTheLaunchTranscript pins the file ward's live
// transcript read opens to the one this launcher streams the transcript to.
// ward can't import this package, so the path is written twice.
func TestLiveReadTargetsTheLaunchTranscript(t *testing.T) {
	t.Parallel()
	if ward.WriterTranscriptPath != launchTranscriptPath {
		t.Fatalf("ward reads %q, the launcher writes %q", ward.WriterTranscriptPath, launchTranscriptPath)
	}
}
