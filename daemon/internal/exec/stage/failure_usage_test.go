package stage

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// usageReadingProvider is testProvider with the Claude provider's
// failure-usage hook.
type usageReadingProvider struct{ testProvider }

func (usageReadingProvider) ExtractFailureUsage(transcript []byte, observedAt time.Time) []exec.UsageMeasurement {
	return exec.ExtractClaudeUsage(transcript, observedAt, domain.UsageSourceAdapterTranscript, ward.RejectDuplicateJSONKeys)
}

// killedTranscript is a writer turn killed mid-run: one message split over
// two lines, a second message, and no result.
const killedTranscript = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":3,"cache_read_input_tokens":100,"output_tokens":1}}}
{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":3,"cache_read_input_tokens":100,"output_tokens":1}}}
{"type":"assistant","message":{"id":"msg_2","usage":{"input_tokens":4,"cache_read_input_tokens":200,"output_tokens":1}}}
{"type":"assistant","message":{"id":"msg_3","usa`

// A killed writer's retained transcript commits its partial usage, observed
// at the intent's recorded instant on the live handoff and on restart
// recovery alike, whatever the driver's clock reads by then.
func TestFailedWriterRecordsPartialUsage(t *testing.T) {
	partial := func(metric string, quantity int64) exec.UsageMeasurement {
		return exec.UsageMeasurement{
			Source: domain.UsageSourcePartialTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: metric, Unit: "tokens", Quantity: quantity, Sequence: 1, ObservedAt: fixedNow,
		}
	}
	want := []exec.UsageMeasurement{partial("input_tokens", 7), partial("cache_read_input_tokens", 300)}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, d *Driver)
	}{
		{"live", func(t *testing.T, d *Driver) {
			d.seeder = &recordingSeeder{}
			in := orphan(t, d, phaseSeeding, nil)
			d.now = func() time.Time { return fixedNow.Add(time.Hour) }
			d.runPipeline(context.Background(), in)
		}},
		{"restart", func(t *testing.T, d *Driver) {
			orphan(t, d, phaseRunning, nil)
			d.now = func() time.Time { return fixedNow.Add(2 * time.Hour) }
			reconcileClean(t, context.Background(), d)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := &stubGate{
				handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) { return nil, ward.ErrWriterFailed },
				recoverFn: failedWithEvidence(137, []byte(killedTranscript), false),
			}
			d := newTestDriver(t, gate, newStubExports())
			d.provider = usageReadingProvider{d.provider.(testProvider)}
			tc.run(t, d)
			got, err := d.Collect(context.Background(), testInvoke)
			if err != nil || got.Status != exec.StatusFailed || len(got.Artifacts) != 1 {
				t.Fatalf("result = %+v, %v; want failed with the retained transcript", got, err)
			}
			if !reflect.DeepEqual(got.Usage, want) {
				t.Fatalf("usage = %#v, want %#v", got.Usage, want)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("result is invalid: %v", err)
			}
		})
	}
}

// With no transcript to read, a provider with the hook commits no usage.
func TestFailedWriterWithoutTranscriptRecordsNoUsage(t *testing.T) {
	unavailable := func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
		return &ward.RecoveryResult{
			Outcome: ward.RecoveryFailed, FailureStatus: 1,
			FailureEvidence: &ward.FailureEvidence{Unavailable: true},
		}, nil
	}
	d := newTestDriver(t, &stubGate{recoverFn: unavailable}, newStubExports())
	d.provider = usageReadingProvider{d.provider.(testProvider)}
	recovered, err := unavailable("", ward.HandoffSpec{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.failedWriterResult(context.Background(), orphan(t, d, phaseRunning, nil), recovered,
		ward.DefaultLaunchSize(ward.LaunchWriter))
	if err != nil || got.Usage != nil {
		t.Fatalf("result = %+v, %v; want no usage", got, err)
	}
}
