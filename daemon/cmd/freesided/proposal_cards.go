package main

import (
	"context"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// proposalCardsVersion names the read's JSON shape; a shape change changes
// it. The task record embeds proposalCard, so a change to that type also
// changes taskRecordVersion.
const proposalCardsVersion = "freeside-proposal-cards-v1"

// proposalCards is what `freesided inspect proposals` prints: every open
// task_proposal and effect_proposal card on the root, read under one store
// revision. No other read lists them all: a card's subject is its proposal
// batch, so a task's record shows only the closure cards of its runs.
type proposalCards struct {
	Version      string                   `json:"version"`
	AsOfRevision int64                    `json:"as_of_revision"`
	AsOf         time.Time                `json:"as_of"`
	Cards        []proposalCard           `json:"cards"`
	RefusedRows  []systemHealthRefusedRow `json:"refused_rows"`
}

// proposalCard says what a card proposes and what it is bound to: its kind,
// the work unit it resolves to, and for a closure the merge it would close
// the issue on. Like taskRecordItem it leaves out the item's reason,
// evidence, and claims, and it prints none of the proposal's parameters.
//
// The item's own fields are null exactly when the store refused the item, and
// every later field from the first supporting read that failed; Error says
// which read and why. Merge, Decision, and SnoozedUntil are otherwise null
// when the card has none.
type proposalCard struct {
	ItemID            domain.ItemID               `json:"item_id"`
	Type              *domain.AttentionType       `json:"type"`
	ProjectID         *domain.ProjectID           `json:"project_id"`
	Status            *domain.ItemStatus          `json:"status"`
	CreatedAt         *time.Time                  `json:"created_at"`
	RequestedDecision []domain.Action             `json:"requested_decision"`
	InstanceID        *domain.ProposalInstanceID  `json:"instance_id"`
	Kind              *domain.EffectKind          `json:"kind"`
	SubjectHandle     *domain.OpaqueSubjectHandle `json:"subject_handle"`
	RunID             *domain.RunID               `json:"run_id"`
	TaskID            *domain.TaskID              `json:"task_id"`
	Merge             *domain.ProspectiveMerge    `json:"merge"`
	Decision          *proposalCardDecision       `json:"decision"`
	SnoozedUntil      *time.Time                  `json:"snoozed_until"`
	Error             *taskRecordError            `json:"error"`
}

// proposalCardDecision is the instance's ledger decision. A policy closure
// approval has no card, so it never appears here.
type proposalCardDecision struct {
	CommandID      string         `json:"command_id"`
	Action         domain.Action  `json:"action"`
	SelectedDigest *domain.Digest `json:"selected_digest"`
	DecidedAt      time.Time      `json:"decided_at"`
}

// hasErrorKind reports whether a card or refused row was left unread by an
// error of the given kind.
func (c proposalCards) hasErrorKind(kind taskRecordErrorKind) bool {
	for _, row := range c.RefusedRows {
		if row.Error.Kind == kind {
			return true
		}
	}
	return cardsHaveErrorKind(c.Cards, kind)
}

func cardsHaveErrorKind(cards []proposalCard, kind taskRecordErrorKind) bool {
	for _, card := range cards {
		if card.Error != nil && card.Error.Kind == kind {
			return true
		}
	}
	return false
}

func readProposalCardsFrom(ctx context.Context, st *store.Store) (proposalCards, error) {
	var cards proposalCards
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		cards, err = readProposalCards(ctx, tx, time.Now().UTC())
		return err
	})
	return cards, err
}

// readProposalCards builds the read in the caller's transaction. Only a
// failed enumeration fails it: a refused row and a card whose supporting
// rows fail a check are reported in place.
func readProposalCards(ctx context.Context, tx *store.ReadTx, asOf time.Time) (proposalCards, error) {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return proposalCards{}, err
	}
	rows, err := tx.ListAttentionItemRows(ctx)
	if err != nil {
		return proposalCards{}, err
	}
	cards := proposalCards{
		Version: proposalCardsVersion, AsOfRevision: state.Revision, AsOf: asOf,
		Cards: []proposalCard{}, RefusedRows: []systemHealthRefusedRow{},
	}
	for _, row := range rows {
		if row.Err != nil {
			cards.RefusedRows = append(cards.RefusedRows, systemHealthRefusedRow{ID: row.ID, Error: newTaskRecordError(row.Err)})
			continue
		}
		item := *row.Item
		if (item.Type != domain.AttentionTaskProposal && item.Type != domain.AttentionEffectProposal) ||
			item.Status != domain.StatusOpen {
			continue
		}
		cards.Cards = append(cards.Cards, projectProposalCard(ctx, tx, item))
	}
	return cards, nil
}

// readProposalCard reads one card by id, as the task record lists a run's
// closure cards: an item the store refuses is the card's id and error.
func readProposalCard(ctx context.Context, tx *store.ReadTx, id domain.ItemID) proposalCard {
	item, err := tx.GetAttentionItem(ctx, id)
	if err != nil {
		return proposalCard{ItemID: id, Error: newTaskRecordError(err)}
	}
	return projectProposalCard(ctx, tx, item)
}

// projectProposalCard reads a card's binding through the store's gated
// reads, in order, and stops at the first that fails.
func projectProposalCard(ctx context.Context, tx *store.ReadTx, item domain.AttentionItem) proposalCard {
	card := proposalCard{
		ItemID: item.ID, Type: &item.Type, ProjectID: &item.ProjectID, Status: &item.Status,
		CreatedAt: item.CreatedAt, RequestedDecision: item.RequestedDecision,
	}
	instance, proposal, err := tx.ProposalForItem(ctx, item.ID)
	if err != nil {
		card.Error = newTaskRecordError(err)
		return card
	}
	handle, err := store.ProposalSubjectHandle(proposal)
	if err != nil {
		card.Error = newTaskRecordError(err)
		return card
	}
	card.InstanceID, card.Kind, card.SubjectHandle = &instance.ID, &proposal.Kind, &handle
	declaration, _, err := tx.ResolveProposalSubject(ctx, handle)
	if err != nil {
		card.Error = newTaskRecordError(err)
		return card
	}
	card.RunID = &declaration.RunID
	run, err := tx.GetRun(ctx, declaration.RunID)
	if err != nil {
		card.Error = newTaskRecordError(err)
		return card
	}
	card.TaskID = &run.TaskID
	if card.Merge, err = tx.ProspectiveMergeForItem(ctx, item.ID); err != nil {
		card.Error = newTaskRecordError(err)
		return card
	}
	decision, err := tx.ProposalDecisionForInstance(ctx, instance.ID)
	if err != nil {
		card.Error = newTaskRecordError(err)
		return card
	}
	if decision != nil {
		card.Decision = &proposalCardDecision{
			CommandID: decision.CommandID, Action: decision.Action,
			SelectedDigest: decision.SelectedDigest, DecidedAt: decision.DecidedAt,
		}
	}
	if card.SnoozedUntil, err = tx.ProposalSnoozeUntil(ctx, item.ID); err != nil {
		card.Error = newTaskRecordError(err)
	}
	return card
}
