package daemoncompose

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// liveTranscriptReader adapts the stage driver's tailer to the transcript
// read, so signet needs no import of the driver. It only translates: which
// invocations are read live is signet's rule, and what is served is the
// tailer's.
type liveTranscriptReader struct{ driver *stage.Driver }

func (r liveTranscriptReader) LiveTranscript(ctx context.Context, id domain.InvocationID, after int64) (
	signet.InvocationTranscriptAvailability, *signet.LiveTranscript, error,
) {
	out, found, err := r.driver.LiveTranscript(ctx, id, after)
	if errors.Is(err, stage.ErrLiveTranscriptOffset) {
		return "", nil, fmt.Errorf("%w: %w", signet.ErrLiveTranscriptOffset, err)
	}
	if err != nil {
		return "", nil, err
	}
	if !found {
		return signet.InvocationTranscriptUnavailable, nil, nil
	}
	switch out.State {
	case stage.LiveTranscriptLive:
		return signet.InvocationTranscriptLive, &signet.LiveTranscript{
			Start: out.Start, End: out.End, Records: out.Records,
			OmittedBefore: out.OmittedBefore, UnreadableRecords: out.UnreadableRecords,
		}, nil
	case stage.LiveTranscriptWithheld:
		return signet.InvocationTranscriptWithheld, nil, nil
	}
	return "", nil, fmt.Errorf("live transcript state %q", out.State)
}
