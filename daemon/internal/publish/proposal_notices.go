package publish

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// proposalNotices raises and resolves the advisory system_health items one
// approved-proposal dispatcher keeps beside its proposals: the follow-up
// filer and the issue comment dispatcher each hold one under their own item
// prefix and codes. The caller serializes passes, so holding is never
// touched concurrently.
type proposalNotices struct {
	// prefix starts every item ID this dispatcher raises.
	prefix string
	// transient are the codes that describe a condition a proposal can
	// leave. The others record a terminal outcome and stay open.
	transient []string
	// resolution is the sentence a resolved transient notice carries.
	resolution string
	now        func() time.Time
	// holding is the transient notices whose condition the current pass saw
	// still holding; the pass resolves every other open one.
	holding map[filingNotice]bool
}

// filingNotice names one notice kind on one proposal.
type filingNotice struct {
	instanceID domain.ProposalInstanceID
	code       string
}

// filingRef is what a health item needs about one proposal: where the
// operator saw it. It comes from the effect_proposal item, which reads even
// when the proposal instance behind it no longer does.
type filingRef struct {
	instanceID domain.ProposalInstanceID
	projectID  domain.ProjectID
	subject    domain.Subject
}

func newProposalNotices(prefix string, transient []string, resolution string, now func() time.Time) *proposalNotices {
	return &proposalNotices{
		prefix: prefix, transient: transient, resolution: resolution, now: now,
		holding: map[filingNotice]bool{},
	}
}

// hold marks a transient notice as still holding in this pass.
func (n *proposalNotices) hold(id domain.ProposalInstanceID, code string) {
	n.holding[filingNotice{id, code}] = true
}

// startPass forgets which notices held in the previous pass.
func (n *proposalNotices) startPass() { clear(n.holding) }

// put opens one advisory, acknowledge-only system_health item on the
// proposal's own subject. Its ID is fixed by the proposal and the code, so
// raising a notice that already exists writes nothing. A transient notice
// that was resolved is final, so the condition's return opens the next
// numbered one.
func (n *proposalNotices) put(ctx context.Context, tx *store.WriteTx, ref filingRef, code, reason string) error {
	if err := tx.RequireProposalWorkflowOpen(ctx, ref.instanceID); errors.Is(err, store.ErrTaskWorkflowClosed) {
		return nil
	}
	first := n.prefix + string(ref.instanceID) + "/" + code
	id := domain.ItemID(first)
	for occurrence := 2; ; occurrence++ {
		existing, err := tx.GetAttentionItem(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			return err
		}
		if existing.Status == domain.StatusOpen || !slices.Contains(n.transient, code) {
			return nil
		}
		id = domain.ItemID(first + "/" + strconv.Itoa(occurrence))
	}
	names, err := tx.DisplayNamesFor(ctx, ref.projectID, ref.subject)
	if err != nil {
		return err
	}
	createdAt := n.now().UTC()
	advisory := domain.HealthPostureAdvisory
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: ref.projectID, Subject: ref.subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason:            reason,
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		// Advisory: publication and review continue; only this proposal is affected.
		HealthDiagnostic: &domain.HealthDiagnostic{Code: code, Impairs: domain.ImpairedCapabilityNone},
		ItemVersion:      1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt, DisplayNames: names, Status: domain.StatusOpen, Posture: &advisory,
	}, nil)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, item)
}

// resolveCleared resolves every open transient notice whose condition this
// pass did not see: the proposal opened its intent, stopped failing, reads
// again, or finished. It is the only resolver because each condition ends in
// several places. The read comes first so the ordinary pass, with no notice
// to resolve, opens no write transaction.
func (n *proposalNotices) resolveCleared(ctx context.Context, st *store.Store) error {
	var cleared []domain.AttentionItem
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		cleared, err = n.cleared(ctx, tx)
		return err
	}); err != nil || len(cleared) == 0 {
		return err
	}
	return st.Write(ctx, func(tx *store.WriteTx) error {
		// Recomputed inside the write, so each resolve advances the item's
		// current version, not the one the read saw.
		cleared, err := n.cleared(ctx, &tx.ReadTx)
		if err != nil {
			return err
		}
		for _, item := range cleared {
			item.ItemVersion++
			item, err := item.ConcludeByDaemon(domain.ConcludedByDaemon, n.now().UTC(), n.resolution, nil)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	})
}

func (n *proposalNotices) cleared(ctx context.Context, tx *store.ReadTx) ([]domain.AttentionItem, error) {
	items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
	if err != nil {
		return nil, err
	}
	var cleared []domain.AttentionItem
	for _, item := range items {
		if err := tx.RequireItemWorkflowOpen(ctx, item); errors.Is(err, store.ErrTaskWorkflowClosed) {
			continue
		} else if err != nil {
			return nil, err
		}
		if notice, ok := n.transientNotice(item); ok && !n.holding[notice] {
			cleared = append(cleared, item)
		}
	}
	return cleared, nil
}

// transientNotice reads the proposal and code out of a transient notice this
// dispatcher raised; ok is false for any other item.
func (n *proposalNotices) transientNotice(item domain.AttentionItem) (filingNotice, bool) {
	rest, ok := strings.CutPrefix(string(item.ID), n.prefix)
	if !ok {
		return filingNotice{}, false
	}
	for _, code := range n.transient {
		// The code is followed by nothing or by an occurrence number.
		if at := strings.LastIndex(rest, "/"+code); at >= 0 {
			return filingNotice{domain.ProposalInstanceID(rest[:at]), code}, true
		}
	}
	return filingNotice{}, false
}
