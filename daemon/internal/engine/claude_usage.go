package engine

import (
	"context"
	"errors"

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
// once its terminal is collected. Without it a capture is dropped.
func WithClaudeUsageSink(sink ClaudeUsageSink) Option {
	return func(e *Engine) error {
		if sink == nil {
			return errors.New("claude usage sink: nil sink")
		}
		e.claudeUsage = sink
		return nil
	}
}

// reportClaudeUsage hands a collected terminal's usage capture to the sink,
// under the enrollment and generation its stored admission binds. Every
// collection of the terminal reports it again; the sink drops a capture no
// newer than the one it holds. Nothing here fails the terminal: a capture
// whose admission has no agent binding or no pool, or a read error, is
// logged and dropped.
func (e *Engine) reportClaudeUsage(
	ctx context.Context, invocationID domain.InvocationID, capture *exec.ClaudeUsageCapture,
) {
	if e.claudeUsage == nil || capture == nil {
		return
	}
	var (
		admission domain.ExecutionAdmission
		pool      domain.UsagePoolID
		found     bool
	)
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, pool, found, err = admittedPool(ctx, tx, invocationID)
		return err
	}); err != nil {
		e.logger.Warn("claude usage capture dropped", "invocation", invocationID, "error", err)
		return
	}
	binding := admission.AgentBinding
	if !found || binding == nil {
		e.logger.Warn("claude usage capture dropped: no admitted enrollment and pool", "invocation", invocationID)
		return
	}
	if err := e.claudeUsage.ClaudeUsageCaptured(
		ctx, pool, binding.EnrollmentID, binding.EnrollmentGeneration, *capture,
	); err != nil {
		e.logger.Warn("claude usage capture not collected", "invocation", invocationID, "pool", pool, "error", err)
	}
}
