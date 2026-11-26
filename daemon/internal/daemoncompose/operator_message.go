package daemoncompose

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// messageDriver is the stage driver's message offer, the one method the
// sender needs.
type messageDriver interface {
	SendMessage(ctx context.Context, id domain.InvocationID, ordinal int64, commandID, text string) error
}

// writerMessageSender adapts the stage driver's SendMessage to the
// send_writer_message command and the restart settle pass, so signet needs
// no import of the driver. It only translates the driver's refusals.
//
// noRun is the refusal for a run whose directory is gone. On the command
// path the message was recorded just now and never offered before, so the
// run had ended (run_ended). On the settle pass an earlier offer may have
// been taken before the directory went, so nothing says whether it was
// delivered (delivery_unknown).
type writerMessageSender struct {
	driver messageDriver
	noRun  domain.OperatorMessageRefusal
}

func (s writerMessageSender) OfferWriterMessage(
	ctx context.Context, m domain.OperatorMessage,
) (*domain.OperatorMessageRefusal, error) {
	err := s.driver.SendMessage(ctx, m.InvocationID, m.Ordinal, m.CommandID, m.Text)
	refusal, ok := messageRefusal(err, s.noRun)
	if !ok {
		return nil, err
	}
	return refusal, nil
}

// messageRefusal maps the driver's answer to a settlement: nil, true for a
// message the run took; a refusal, true for one the run refused; and
// false for an error that leaves the delivery unknown. An invocation the
// driver holds no intent for was never started, and intents outlive their
// runs, so it is not_started on either path.
func messageRefusal(err error, noRun domain.OperatorMessageRefusal) (*domain.OperatorMessageRefusal, bool) {
	var refusal domain.OperatorMessageRefusal
	switch {
	case err == nil:
		return nil, true
	case errors.Is(err, stage.ErrCordonRunEnded):
		refusal = domain.OperatorMessageRefusalRunEnded
	case errors.Is(err, stage.ErrCordonInputClosed):
		refusal = domain.OperatorMessageRefusalInputClosed
	case errors.Is(err, stage.ErrCordonNotStreamed):
		refusal = domain.OperatorMessageRefusalNotStreamed
	case errors.Is(err, stage.ErrCordonNotTaken):
		refusal = domain.OperatorMessageRefusalNotTaken
	case errors.Is(err, stage.ErrCordonNotStarted), errors.Is(err, exec.ErrUnknownInvocation):
		refusal = domain.OperatorMessageRefusalNotStarted
	case errors.Is(err, stage.ErrCordonNoRun):
		refusal = noRun
	default:
		return nil, false
	}
	return &refusal, true
}

// settleOperatorMessages offers every message a stopped daemon left
// recorded once more, under its own ordinal, and settles it with the
// answer. The cordon settles an ordinal it saw before from its files, so a
// message the run already took settles delivered and is never written to
// the writer twice. It runs after the driver's Reconcile and before the
// command surface serves, so no command races it for a record. An offer
// that errors, or a settle that fails, leaves that record recorded for the
// next start and is logged; it never stops startup.
func settleOperatorMessages(
	ctx context.Context, st *store.Store, driver messageDriver, now func() time.Time, logger *slog.Logger,
) error {
	var pending []domain.OperatorMessage
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.UnsettledOperatorMessages(ctx)
		return err
	}); err != nil {
		return err
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	sender := writerMessageSender{driver: driver, noRun: domain.OperatorMessageRefusalDeliveryUnknown}
	for _, m := range pending {
		refusal, err := sender.OfferWriterMessage(ctx, m)
		if err != nil {
			logger.Warn("operator message left recorded after restart",
				"command_id", m.CommandID, "invocation_id", m.InvocationID, "ordinal", m.Ordinal, "error", err)
			continue
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, err := tx.SettleOperatorMessage(ctx, m.CommandID, refusal, startupNow(now))
			return err
		}); err != nil {
			logger.Warn("operator message settle failed after restart",
				"command_id", m.CommandID, "invocation_id", m.InvocationID, "ordinal", m.Ordinal, "error", err)
		}
	}
	return nil
}
