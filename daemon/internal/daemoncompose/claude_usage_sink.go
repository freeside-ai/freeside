package daemoncompose

import (
	"context"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// usageCollectorsFor returns the usage collectors the daemon runs by harness
// client. With none injected, as in production, that is the Claude collector,
// returned too so the engine's sink can feed it. Injected collectors, as in
// tests, run alone, and no capture is recorded.
func usageCollectorsFor(
	injected map[domain.HarnessClientKind]ward.UsageCollector,
) (map[domain.HarnessClientKind]ward.UsageCollector, *ward.ClaudeUsageCollector) {
	if injected != nil {
		return injected, nil
	}
	claude := ward.NewClaudeUsageCollector()
	return map[domain.HarnessClientKind]ward.UsageCollector{domain.HarnessClientClaudeCode: claude}, claude
}

// claudeUsageSink is the engine's ClaudeUsageSink in production: it holds a
// writer turn's capture for the Claude usage collector, and only when the
// collector kept it, as newer than what it held, starts the pool's
// collection, so a replayed terminal starts none.
type claudeUsageSink struct {
	collector *ward.ClaudeUsageCollector
	usage     *usagecollect.Coordinator
}

func (s claudeUsageSink) ClaudeUsageCaptured(
	ctx context.Context, pool domain.UsagePoolID, enrollment domain.ClientEnrollmentID, generation int,
	capture exec.ClaudeUsageCapture,
) error {
	if !s.collector.Record(enrollment, generation, capture) {
		return nil
	}
	return s.usage.CaptureArrived(ctx, pool, enrollment, generation)
}
