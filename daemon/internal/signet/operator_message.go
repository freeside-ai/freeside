package signet

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// SendWriterMessagePayload mirrors the API's SendWriterMessagePayload: text
// for one writer invocation of one run.
type SendWriterMessagePayload struct {
	RunID        domain.RunID        `json:"run_id"`
	InvocationID domain.InvocationID `json:"invocation_id"`
	Text         string              `json:"text"`
}

// WriterMessageSender offers a recorded message to its run (the stage
// driver's SendMessage, through an adapter). It answers nil, nil once the
// run's supervisor took the message, and a refusal when the run refused it.
// An error means neither is known: the record stays recorded, and the next
// start offers it again under the same ordinal.
type WriterMessageSender interface {
	OfferWriterMessage(ctx context.Context, m domain.OperatorMessage) (*domain.OperatorMessageRefusal, error)
}

// WithWriterMessages supplies the sender a send_writer_message command
// offers its message through. The func is read on each command so the
// composition can bind the driver after the service is built. With no
// option, a nil func, or a nil sender, the command is refused before
// anything is recorded.
func WithWriterMessages(sender func() WriterMessageSender) Option {
	return func(s *Service) { s.writerMessages = sender }
}

// ErrWriterMessageUnavailable answers a send_writer_message on a service
// whose sender isn't bound. Nothing is recorded, and the HTTP surface
// answers 400 (isCommandRequestError).
var ErrWriterMessageUnavailable = errors.New("writer messages are not available")

// ErrNotACordonedWriter refuses a message to an invocation the run doesn't
// record as a writer, or one admitted to run warded: only a cordoned
// writer reads streamed input. Nothing is recorded.
var ErrNotACordonedWriter = errors.New("invocation is not a cordoned writer of the run")

func (s *Service) sendWriterMessage(ctx context.Context, in ClientCommand) (CommandResult, error) {
	if !in.operatorArmOnly() {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, ErrInvalidOperatorCommandPayload)
	}
	p := in.SendWriterMessage
	request := domain.OperatorMessageRequest{
		CommandID: in.CommandID, Sender: "device:" + string(in.DeviceID),
		RunID: p.RunID, InvocationID: p.InvocationID, Text: p.Text,
	}
	m, revision, err := s.sendOperatorMessage(ctx, request, func(tx *store.WriteTx) error {
		return gateActiveDevice(ctx, tx, in.DeviceID)
	})
	if err != nil {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, err)
	}
	return CommandResult{OperatorMessage: &m, Revision: revision}, nil
}

// SendOperatorMessage sends a message from the operator's shell, whose
// control socket already authenticated it, so it passes no device gate.
// It is otherwise the send_writer_message command.
func (s *Service) SendOperatorMessage(
	ctx context.Context, request domain.OperatorMessageRequest,
) (domain.OperatorMessage, error) {
	m, _, err := s.sendOperatorMessage(ctx, request, nil)
	return m, err
}

// sendOperatorMessage records the message, offers it to the run, and
// settles it with the answer. Every refusal it can decide alone comes
// first and records nothing: bad text, no bound sender, and an invocation
// that isn't a cordoned writer of the run. A command_id already recorded
// with the same request answers with its record as it stands and offers
// nothing again; under another request, or held by another command kind,
// it is store.ErrImmutableConflict.
//
// The offer runs outside any transaction, because the run's answer can
// take seconds (cordon's take timeout). It doesn't follow the caller's
// cancellation, so a client that hangs up still gets its message settled.
// An offer that errors leaves the record recorded, and the response says
// so; the next start's settle pass offers it again.
func (s *Service) sendOperatorMessage(
	ctx context.Context, request domain.OperatorMessageRequest, gate func(*store.WriteTx) error,
) (domain.OperatorMessage, int64, error) {
	if err := request.Validate(); err != nil {
		return domain.OperatorMessage{}, 0, err
	}
	var m domain.OperatorMessage
	var sender WriterMessageSender
	err := s.store.Write(ctx, func(tx *store.WriteTx) error {
		if gate != nil {
			if err := gate(tx); err != nil {
				return err
			}
		}
		if old, err := tx.GetOperatorMessage(ctx, request.CommandID); err == nil {
			if old.OperatorMessageRequest != request {
				return fmt.Errorf("command %q: %w", request.CommandID, store.ErrImmutableConflict)
			}
			m = old
			return errReplay
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// Another kind's id is refused before anything else can answer for
		// it, as commitReceipt does.
		if err := tx.RequireCommandIDFree(ctx, request.CommandID); err != nil {
			return err
		}
		if s.writerMessages != nil {
			sender = s.writerMessages()
		}
		if sender == nil {
			return ErrWriterMessageUnavailable
		}
		if err := requireCordonedWriter(ctx, &tx.ReadTx, request.RunID, request.InvocationID); err != nil {
			return err
		}
		var err error
		m, err = tx.RecordOperatorMessage(ctx, request, s.now().UTC())
		return err
	})
	switch {
	case errors.Is(err, errReplay):
		return s.withRevision(ctx, m)
	case err != nil:
		return domain.OperatorMessage{}, 0, err
	}
	refusal, err := sender.OfferWriterMessage(context.WithoutCancel(ctx), m)
	if err != nil {
		s.logger.Warn("operator message left recorded; the next start offers it again",
			"command_id", m.CommandID, "invocation_id", m.InvocationID, "ordinal", m.Ordinal, "error", err)
		return s.withRevision(ctx, m)
	}
	if err := s.store.WriteInternal(context.WithoutCancel(ctx), func(tx *store.InternalTx) error {
		var err error
		m, err = tx.SettleOperatorMessage(ctx, m.CommandID, refusal, s.now().UTC())
		return err
	}); err != nil {
		return domain.OperatorMessage{}, 0, err
	}
	return s.withRevision(ctx, m)
}

// withRevision pairs a record with the server revision now. The record
// isn't synchronized, so the revision only dates the answer.
func (s *Service) withRevision(ctx context.Context, m domain.OperatorMessage) (domain.OperatorMessage, int64, error) {
	state, err := s.store.ServerState(ctx)
	if err != nil {
		return domain.OperatorMessage{}, 0, err
	}
	return m, state.Revision, nil
}

// requireCordonedWriter refuses an invocation the run doesn't record as a
// writer, or whose admission ran it on any backend but the cordon. An
// admission naming another run reads as not found, as the transcript read
// does.
func requireCordonedWriter(ctx context.Context, tx *store.ReadTx, runID domain.RunID, id domain.InvocationID) error {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, id)
	if err != nil {
		return err
	}
	if admission.RunID != runID {
		return fmt.Errorf("invocation %s is admitted for another run: %w", id, store.ErrNotFound)
	}
	if !recordedWriter(run, id) || admission.Backend != string(domain.BackendCordonedHostProcess) {
		return fmt.Errorf("invocation %s: %w", id, ErrNotACordonedWriter)
	}
	return nil
}
