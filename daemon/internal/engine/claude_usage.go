package engine

import (
	"context"
	"errors"
	"log/slog"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ClaudeUsageSink receives a finished Claude writer turn's usage capture
// (exec.StageResult.ClaudeUsage) with the enrollment and generation the turn
// was admitted under and the usage pool that enrollment's identity meters
// (plan §10, usage collection rules). The daemon's sink holds it for the
// pool's Claude usage collector and starts a collection. The capture is
// advisory: an error is logged and never fails the terminal it rode on.
type ClaudeUsageSink interface {
	ClaudeUsageCaptured(
		ctx context.Context, pool domain.UsagePoolID, enrollment domain.ClientEnrollmentID, generation int,
		capture exec.ClaudeUsageCapture,
	) error
}

// WithClaudeUsageSink installs the sink a writer turn's usage capture goes to
// once its terminal is collected. Without it a capture is dropped with a
// warning.
func WithClaudeUsageSink(sink ClaudeUsageSink) Option {
	return func(e *Engine) error {
		if sink == nil {
			return errors.New("claude usage sink: nil sink")
		}
		e.claudeUsage = sink
		return nil
	}
}

// reportClaudeUsage hands a collected terminal's usage capture to the sink.
// Every collection of the terminal reports it again; the sink drops a capture
// no newer than the one it holds.
func (e *Engine) reportClaudeUsage(
	ctx context.Context, invocationID domain.InvocationID, capture *exec.ClaudeUsageCapture,
) {
	reportClaudeUsageCapture(ctx, e.store, e.claudeUsage, e.logger, invocationID, capture)
}

// reportClaudeUsageCapture hands a writer turn's usage capture to the sink,
// under the enrollment and generation the invocation's stored admission
// binds. It runs outside any store transaction: the sink reads the store.
// Nothing here fails the caller. A nil capture is no usage answer and returns
// quietly; every other drop (no sink installed, an admission with no agent
// binding or no pool, a read error, a sink error) is logged naming the
// invocation, so a lost answer is visible.
func reportClaudeUsageCapture(
	ctx context.Context, st *store.Store, sink ClaudeUsageSink, logger *slog.Logger,
	invocationID domain.InvocationID, capture *exec.ClaudeUsageCapture,
) {
	if capture == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	if sink == nil {
		logger.Warn("claude usage capture dropped: no usage sink installed", "invocation", invocationID)
		return
	}
	var (
		admission domain.ExecutionAdmission
		pool      domain.UsagePoolID
		found     bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, pool, found, err = admittedPool(ctx, tx, invocationID)
		return err
	}); err != nil {
		logger.Warn("claude usage capture dropped", "invocation", invocationID, "error", err)
		return
	}
	binding := admission.AgentBinding
	if !found || binding == nil {
		logger.Warn("claude usage capture dropped: no admitted enrollment and pool", "invocation", invocationID)
		return
	}
	if err := sink.ClaudeUsageCaptured(
		ctx, pool, binding.EnrollmentID, binding.EnrollmentGeneration, *capture,
	); err != nil {
		logger.Warn("claude usage capture not collected", "invocation", invocationID, "pool", pool, "error", err)
	}
}
