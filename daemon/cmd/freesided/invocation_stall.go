package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const invocationStalledItemPrefix = "invocation-stalled-"

func invocationStalledPrefix(id domain.InvocationID) string {
	return invocationStalledItemPrefix + string(id) + "-"
}

// invocationStallNotice returns the stage driver's Stall hook: it files one
// advisory invocation_stalled health notice when ward reports a running
// writer stalled, and resolves it when ward reports recovery or the end of
// the writer wait (plan §5.12). The notice holds nothing and never touches
// the writer's hard budget; ward calls this off its wait path.
//
// The subject is the system, not the run: an open run-subject item with a
// requested decision reads as attention_required to run supervision, and a
// self-resolving advisory notice must not end a supervised run. The reason
// names the invocation instead.
func invocationStallNotice(
	st *store.Store, interval time.Duration, now func() time.Time,
) func(context.Context, domain.InvocationID, bool) error {
	return func(ctx context.Context, id domain.InvocationID, stalled bool) error {
		return st.Write(ctx, func(tx *store.WriteTx) error {
			open, err := openInvocationStallNotices(ctx, tx, func(itemID string) bool {
				// The remainder is the filing revision alone, so invocation
				// "inv-a" never matches the notice of "inv-a-b".
				revision, ok := strings.CutPrefix(itemID, invocationStalledPrefix(id))
				return ok && revision != "" && strings.Trim(revision, "0123456789") == ""
			})
			if err != nil {
				return err
			}
			if !stalled {
				return resolveInvocationStallNotices(ctx, tx, open)
			}
			if len(open) > 0 {
				return nil
			}
			item, err := invocationStalledItem(ctx, tx, id, interval, now().UTC())
			if err != nil {
				return err
			}
			return tx.PutAttentionItem(ctx, item)
		})
	}
}

func invocationStalledItem(
	ctx context.Context, tx *store.WriteTx, id domain.InvocationID, interval time.Duration, createdAt time.Time,
) (domain.AttentionItem, error) {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	names, err := tx.DisplayNamesFor(ctx, "project-system", subject)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	posture := domain.HealthPostureAdvisory
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        domain.ItemID(fmt.Sprintf("%s%d", invocationStalledPrefix(id), state.Revision+1)),
		ProjectID: "project-system", Subject: subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason: fmt.Sprintf(
			"Invocation %s has had no provider response for %s. The writer is still running, "+
				"possibly a long tool command, and its hard budget is unchanged. "+
				"This notice resolves itself when provider responses resume or the writer stops.",
			id, interval),
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: "invocation_stalled", Impairs: domain.ImpairedCapabilityNone,
		},
		DisplayNames: names, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional,
		CreatedAt:         &createdAt, Posture: &posture, Status: domain.StatusOpen,
	}, nil)
}

// concludeInvocationStallNotices resolves every open stall notice at
// startup. No writer wait survives a daemon restart, so any open notice is
// stale. A held-work notice raised by `freesided raise-item` goes in the
// same write, so a start costs one revision, not two.
func concludeInvocationStallNotices(ctx context.Context, st *store.Store) error {
	return st.Write(ctx, func(tx *store.WriteTx) error {
		open, err := openInvocationStallNotices(ctx, tx, func(itemID string) bool {
			return strings.HasPrefix(itemID, invocationStalledItemPrefix) || isRaisedHeldWorkNotice(itemID)
		})
		if err != nil {
			return err
		}
		return resolveInvocationStallNotices(ctx, tx, open)
	})
}

func openInvocationStallNotices(
	ctx context.Context, tx *store.WriteTx, match func(itemID string) bool,
) ([]domain.AttentionItem, error) {
	items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
	if err != nil {
		return nil, err
	}
	matching := items[:0]
	for _, item := range items {
		if match(string(item.ID)) {
			matching = append(matching, item)
		}
	}
	return matching, nil
}

func resolveInvocationStallNotices(ctx context.Context, tx *store.WriteTx, items []domain.AttentionItem) error {
	for _, item := range items {
		item.ItemVersion++
		item.Status = domain.StatusResolved
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

// stallInterval resolves a zero flag to ward's default, so the notice names
// the interval ward actually applies.
func stallInterval(configured time.Duration) time.Duration {
	if configured == 0 {
		return ward.DefaultStallInterval
	}
	return configured
}
