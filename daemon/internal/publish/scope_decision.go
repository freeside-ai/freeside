package publish

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	scopeDecisionMarkerName       = "freeside:scope-decision"
	scopeDecisionHeading          = "## Freeside Scope Decision"
	maxRenderedScopeDecisionBytes = 12 << 10
)

func containsScopeDecisionMarker(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, scopeDecisionMarkerName) || strings.Contains(lower, strings.ToLower(scopeDecisionHeading))
}

func renderScopeDecision(f domain.ScopeDecisionFacts) (string, error) {
	if err := f.Validate(); err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "<!-- %s -->\n\n%s\n\nThe operator kept the approved scope. Required work on these paths remains unmet; they were left unchanged:\n", scopeDecisionMarkerName, scopeDecisionHeading)
	for _, p := range f.Paths {
		fmt.Fprintf(&out, "- %s\n", boundedClaim(p, 768))
	}
	fmt.Fprintf(&out, "\nDeclared paths: %s\n\nCandidate: %s\n\nDecision: %s at %s\n\nOperator answer: %s\n\n<!-- /%s -->",
		boundedClaim(strings.Join(f.DeclaredPaths, ", "), 1024), boundedClaim(f.HeadSHA, 256), screenedScopeCommandClaim(f.CommandID, 256),
		f.DecidedAt.UTC().Format(time.RFC3339Nano), screenedScopeCommandClaim(f.Answer, 3072), scopeDecisionMarkerName)
	if out.Len() > maxRenderedScopeDecisionBytes {
		return "", fmt.Errorf("scope decision exceeds reserved body budget")
	}
	return out.String(), nil
}

func screenedScopeCommandClaim(value string, limit int) string {
	// Commands are local conversation input. Screen the complete value before
	// truncation can split a credential that would otherwise reach the forge.
	if importer.ContainsSecret([]byte(value)) {
		return "[redacted: credential detected]"
	}
	return boundedClaim(value, limit)
}

func validateCurrentScopeDecision(ctx context.Context, tx *store.ReadTx, c Candidate) error {
	expected, err := tx.ScopeDecisionForCandidate(ctx, c.RunID, c.HeadSHA)
	if err != nil {
		return fmt.Errorf("scope decision authority: %w: %w", ErrUnauthorizedPublication, err)
	}
	if !reflect.DeepEqual(expected, c.ScopeDecision) {
		return fmt.Errorf("scope decision differs from durable command: %w", ErrUnauthorizedPublication)
	}
	return nil
}

func validatePublicationScopeHistory(ctx context.Context, tx *store.ReadTx, c Candidate, identity Identity) error {
	// Publication identity is shared across attempts. An earlier intent owns
	// the scope statement even when the live PR's text has been edited away.
	for _, list := range []func(context.Context, string) ([]store.QueueEntry, error){
		tx.ListPendingOutbox, tx.ListDispatchedOutbox, tx.ListQuarantinedOutbox,
	} {
		entries, err := list(ctx, IntentKindPublication)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			var locator struct {
				Identity domain.Digest `json:"identity"`
			}
			if err := strictjson.DecodeAllowingUnknownFields(entry.Payload, &locator, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
				return fmt.Errorf("locate publication scope history: %w", err)
			}
			if locator.Identity != identity.Digest() {
				continue
			}
			intent, err := DecodeStoredIntent(entry)
			if err != nil {
				return err
			}
			key, err := IntentKey(intent.InvocationID, IntentKindPublication)
			if err != nil {
				return err
			}
			if entry.IdempotencyKey != key || entry.Kind != IntentKindPublication || entry.Quarantined() ||
				intent.Repo != c.Repo || intent.BaseRef != c.BaseRef || intent.SourceHeadSHA != c.HeadSHA {
				return ErrPublicationConflict
			}
			var prior *domain.ScopeDecisionFacts
			if intent.ProducingInvocationID != "" {
				admission, err := tx.GetExecutionAdmissionRecord(ctx, intent.ProducingInvocationID)
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("scope owner has no execution admission: %w", ErrExecutionExportMissing)
				}
				if err != nil {
					return err
				}
				exported, err := tx.GetExecutionExportRecord(ctx, intent.ProducingInvocationID)
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("scope owner has no execution export: %w", ErrExecutionExportMissing)
				}
				if err != nil {
					return err
				}
				if admission.RunID != intent.ReservationRunID || exported.HeadSHA != intent.SourceHeadSHA {
					return ErrPublicationConflict
				}
				prior, err = tx.ScopeDecisionForCandidate(ctx, intent.ReservationRunID, intent.SourceHeadSHA)
				if err != nil {
					return err
				}
			}
			if !reflect.DeepEqual(prior, c.ScopeDecision) {
				return fmt.Errorf("publication scope differs from its earlier intent: %w", ErrPublicationConflict)
			}
		}
	}
	return nil
}

// Repair must recover the original run binding before checking optional body
// facts. Publication identity alone does not include the run or those facts.
func validateRepairIntent(ctx context.Context, tx *store.ReadTx, c Candidate, identity Identity) error {
	if err := validatePublicationScopeHistory(ctx, tx, c, identity); err != nil {
		return err
	}
	key, err := IntentKey(c.InvocationID, IntentKindPublication)
	if err != nil {
		return err
	}
	entry, err := tx.GetOutbox(ctx, key)
	if err != nil {
		return err
	}
	if entry.IdempotencyKey != key || entry.Kind != IntentKindPublication || entry.Quarantined() {
		return ErrPublicationConflict
	}
	intent, err := DecodeStoredIntent(entry)
	if err != nil {
		return err
	}
	if intent.Identity != identity.Digest() || intent.InvocationID != c.InvocationID ||
		intent.Repo != c.Repo || intent.BaseRef != c.BaseRef || intent.SourceHeadSHA != c.HeadSHA ||
		c.AuthorizationID == nil || intent.AuthorizationID != *c.AuthorizationID {
		return ErrPublicationConflict
	}
	if intent.ProducingInvocationID != "" {
		admission, err := tx.GetExecutionAdmissionRecord(ctx, intent.ProducingInvocationID)
		if err != nil {
			return err
		}
		exported, err := tx.GetExecutionExportRecord(ctx, intent.ProducingInvocationID)
		if err != nil {
			return err
		}
		if c.RunID != intent.ReservationRunID || admission.RunID != intent.ReservationRunID || exported.HeadSHA != c.HeadSHA {
			return ErrPublicationConflict
		}
	}
	return ValidateIntentDispositionHistory(intent, c)
}
