package stage

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// quotaClassifyingProvider is testProvider with the Claude classifier's hook.
type quotaClassifyingProvider struct{ testProvider }

func (quotaClassifyingProvider) ClassifyQuotaRefusal(transcript []byte) *domain.QuotaRefusal {
	return exec.ClassifyClaudeQuotaRefusal(transcript, ward.RejectDuplicateJSONKeys)
}

// refusedTranscript is a refused turn's transcript: one rejected window with
// a reset, and the error result the CLI writes before it exits 1.
const refusedTranscript = `{"type":"system","subtype":"init"}
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1794400000,"rateLimitType":"five_hour","isUsingOverage":false}}
{"type":"result","subtype":"success","is_error":true}
`

var refusedWindow = domain.QuotaRefusal{Window: "five_hour", ResetsAt: new(time.Unix(1794400000, 0).UTC())}

func failedWithEvidence(status int, body []byte, kill bool) func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
	return func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
		return &ward.RecoveryResult{
			Outcome: ward.RecoveryFailed, FailureStatus: status, MemoryLimitKill: kill,
			FailureEvidence: &ward.FailureEvidence{Body: body, Digest: contentaddr.Sum(body)},
		}, nil
	}
}

// A refused writer turn fails with the quota cause and its window and reset,
// alike on the live handoff and on restart recovery from the same evidence.
func TestFailedWriterQuotaRefusalClassifies(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, d *Driver)
	}{
		{"live", func(t *testing.T, d *Driver) {
			d.seeder = &recordingSeeder{}
			d.runPipeline(context.Background(), orphan(t, d, phaseSeeding, nil))
		}},
		{"restart", func(t *testing.T, d *Driver) {
			orphan(t, d, phaseRunning, nil)
			reconcileClean(t, context.Background(), d)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			outcomes := newStubExports()
			gate := &stubGate{
				handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) { return nil, ward.ErrWriterFailed },
				recoverFn: failedWithEvidence(1, []byte(refusedTranscript), false),
			}
			d := newTestDriver(t, gate, outcomes)
			d.provider = quotaClassifyingProvider{d.provider.(testProvider)}
			tc.run(t, d)
			got, err := d.Collect(ctx, testInvoke)
			if err != nil || got.Status != exec.StatusFailed || got.Cause != domain.FailureCauseQuota ||
				got.QuotaRefusal == nil || !reflect.DeepEqual(*got.QuotaRefusal, refusedWindow) {
				t.Fatalf("result = %+v, %v; want failed with the five_hour refusal", got, err)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("result is invalid: %v", err)
			}
			if !strings.HasPrefix(got.Summary, "Provider refused the turn on its five_hour quota window until 2026-11-11T12:26:40Z.") ||
				!strings.Contains(got.Summary, "status 1") || len(got.Artifacts) != 1 {
				t.Fatalf("summary %q, artifacts %v", got.Summary, got.Artifacts)
			}
			outcome, found, err := outcomes.LookupExecutionOutcome(ctx, testInvoke)
			if err != nil || !found || outcome.Cause != domain.FailureCauseQuota {
				t.Fatalf("outcome = (%+v, %v, %v), want cause quota", outcome, found, err)
			}
		})
	}
}

// Everything but a readable refusal on a provider with the hook commits
// exactly the result a provider without it gives.
func TestFailedWriterQuotaRefusalFallsBack(t *testing.T) {
	size := ward.DefaultLaunchSize(ward.LaunchWriter)
	unavailable := func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
		return &ward.RecoveryResult{
			Outcome: ward.RecoveryFailed, FailureStatus: 1,
			FailureEvidence: &ward.FailureEvidence{Unavailable: true},
		}, nil
	}
	noEvidence := func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
		return &ward.RecoveryResult{Outcome: ward.RecoveryFailed, FailureStatus: 1}, nil
	}
	// A failed transcript carries the CLI's stderr after its JSONL; any
	// non-JSON line there is doubt.
	withStderr := refusedTranscript + "[claude-code:unrecognized_model] {\"model\":\"x\"}\n"
	result := func(t *testing.T, classify bool, recoverFn func(string, ward.HandoffSpec) (*ward.RecoveryResult, error)) exec.StageResult {
		t.Helper()
		d := newTestDriver(t, &stubGate{recoverFn: recoverFn}, newStubExports())
		if classify {
			d.provider = quotaClassifyingProvider{d.provider.(testProvider)}
		}
		recovered, err := recoverFn("", ward.HandoffSpec{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.failedWriterResult(context.Background(), orphan(t, d, phaseRunning, nil), recovered, size)
		if err != nil {
			t.Fatal(err)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("result is invalid: %v", err)
		}
		return got
	}
	for _, tc := range []struct {
		name      string
		recoverFn func(string, ward.HandoffSpec) (*ward.RecoveryResult, error)
		cause     domain.FailureCause
	}{
		{"memory-limit kill wins", failedWithEvidence(137, []byte(refusedTranscript), true), domain.FailureCauseMemoryLimit},
		{"no rejected window", failedWithEvidence(1, []byte(strings.Replace(refusedTranscript, `"rejected"`, `"allowed"`, 1)), false), ""},
		{"truncated transcript", failedWithEvidence(1, []byte(refusedTranscript[:len(refusedTranscript)-10]), false), ""},
		{"stderr appended", failedWithEvidence(1, []byte(withStderr), false), ""},
		{"unavailable evidence", unavailable, ""},
		{"no evidence", noEvidence, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := result(t, true, tc.recoverFn)
			if want := result(t, false, tc.recoverFn); !reflect.DeepEqual(got, want) {
				t.Fatalf("result = %+v, want the hookless %+v", got, want)
			}
			if got.Cause != tc.cause || got.QuotaRefusal != nil {
				t.Fatalf("result = %+v; want cause %q and no refusal", got, tc.cause)
			}
		})
	}
	t.Run("provider without the hook", func(t *testing.T) {
		got := result(t, false, failedWithEvidence(1, []byte(refusedTranscript), false))
		if got.Cause != "" || got.QuotaRefusal != nil || !strings.HasPrefix(got.Summary, "Test writer exited with status 1.") {
			t.Fatalf("result = %+v; want today's plain failure", got)
		}
	})
}

// A rejected event with no reset is still a refusal, with a nil reset the
// park takes from a usage reading.
func TestFailedWriterQuotaRefusalWithoutReset(t *testing.T) {
	d := newTestDriver(t, &stubGate{}, newStubExports())
	d.provider = quotaClassifyingProvider{d.provider.(testProvider)}
	body := []byte(strings.Replace(refusedTranscript, `"resetsAt":1794400000,`, "", 1))
	recovered, err := failedWithEvidence(1, body, false)("", ward.HandoffSpec{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.failedWriterResult(context.Background(), orphan(t, d, phaseRunning, nil), recovered, ward.ContainerSize{})
	if err != nil || got.Cause != domain.FailureCauseQuota ||
		!reflect.DeepEqual(got.QuotaRefusal, &domain.QuotaRefusal{Window: "five_hour"}) {
		t.Fatalf("result = %+v, %v; want the five_hour refusal with no reset", got, err)
	}
	if !strings.HasPrefix(got.Summary, "Provider refused the turn on its five_hour quota window; it gave no reset.") {
		t.Fatalf("summary %q", got.Summary)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("result is invalid: %v", err)
	}
}

// A restart between recording the outcome and committing the result rebuilds
// it from the durable outcome, which keeps the quota cause but not the
// window, so that run raises the ordinary card until #3089 keeps it.
func TestFailedWriterQuotaRefusalOutcomeRestoreKeepsOnlyTheCause(t *testing.T) {
	ctx := context.Background()
	gate := &stubGate{
		handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) { return nil, ward.ErrWriterFailed },
		recoverFn: failedWithEvidence(1, []byte(refusedTranscript), false),
	}
	d := newTestDriver(t, gate, newStubExports())
	d.provider = quotaClassifyingProvider{d.provider.(testProvider)}
	d.seeder = &recordingSeeder{}
	in := orphan(t, d, phaseSeeding, nil)
	d.runPipeline(ctx, in)
	in.Phase = phaseRunning
	restored, ok, err := d.restoreDurableOutcome(ctx, in)
	if err != nil || !ok || restored.Result == nil {
		t.Fatalf("restored: %+v, %v, %v", restored, ok, err)
	}
	if restored.Result.Cause != domain.FailureCauseQuota || restored.Result.QuotaRefusal != nil {
		t.Fatalf("restored result = %+v; want cause quota and no window", *restored.Result)
	}
}
