package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Conclusion records how an attention item closed, so a closed card can say
// what happened without the client reconstructing it from commands it may
// never have seen (#2097). It is daemon-stamped on the transition that moves
// the item off open and immutable after (ValidateAttentionItemTransition).
// An item closed before the field existed keeps a nil Conclusion forever.
//
// A decision conclusion carries the accepting command and an empty Reason; a
// daemon conclusion (expiry, supersession, the daemon's own resolution)
// carries the daemon's one-sentence Reason and no Decision.
type Conclusion struct {
	Cause       ConclusionCause `json:"cause"`
	ConcludedAt time.Time       `json:"concluded_at"`
	Reason      string          `json:"reason"`
	// SuccessorItemID names the item that replaced this one, when the daemon
	// minted a replacement in the same transaction.
	SuccessorItemID *ItemID             `json:"successor_item_id"`
	Decision        *ConclusionDecision `json:"decision"`
}

// ConclusionDecision is the accepted command that concluded an item, as it
// stood at decision time. DeviceDisplayName is copied rather than looked up
// later, so the record reads the same after the device is renamed or
// unpaired.
type ConclusionDecision struct {
	CommandID         string   `json:"command_id"`
	Action            Action   `json:"action"`
	DeviceID          DeviceID `json:"device_id"`
	DeviceDisplayName string   `json:"device_display_name"`
	// FindingRoutes is the per-finding route list an adjudicate_findings or
	// choose_alternative_route command carried, sorted by finding; nil for
	// every other action.
	FindingRoutes []ConcludedRoute `json:"finding_routes"`
	AnswerRoute   *AnswerRoute     `json:"answer_route"`
}

// ConcludedRoute is one finding's route as a concluding command chose it.
type ConcludedRoute struct {
	FindingID FindingID         `json:"finding_id"`
	Route     AdjudicationRoute `json:"route"`
}

// maxConclusionReasonBytes bounds the daemon's close sentence; it is prose
// for a card, not a log.
const maxConclusionReasonBytes = 1024

// NewDecisionConclusion records a concluding command accepted at at.
func NewDecisionConclusion(at time.Time, decision ConclusionDecision) (Conclusion, error) {
	c := Conclusion{Cause: ConcludedByDecision, ConcludedAt: at, Decision: &decision}
	if err := c.Validate(); err != nil {
		return Conclusion{}, err
	}
	return c, nil
}

// NewDaemonConclusion records a close the daemon made on its own: cause is
// any cause but decision, and reason says why in one sentence.
func NewDaemonConclusion(cause ConclusionCause, at time.Time, reason string, successor *ItemID) (Conclusion, error) {
	c := Conclusion{Cause: cause, ConcludedAt: at, Reason: reason, SuccessorItemID: successor}
	if err := c.Validate(); err != nil {
		return Conclusion{}, err
	}
	return c, nil
}

// Validate checks the conclusion's own shape; whether it fits its item's
// status and decided_at is AttentionItem.Validate's check.
func (c Conclusion) Validate() error {
	if !c.Cause.valid() {
		return fmt.Errorf("conclusion cause %q: %w", c.Cause, ErrInvalidConclusion)
	}
	if c.ConcludedAt.IsZero() {
		return fmt.Errorf("conclusion concluded_at: %w", ErrMissingTimestamp)
	}
	// The item body's re-put convergence is a byte compare (mirrors DecidedAt).
	if c.ConcludedAt.Location() != time.UTC {
		return fmt.Errorf("conclusion concluded_at: %w", ErrTimestampNotUTC)
	}
	if c.SuccessorItemID != nil && *c.SuccessorItemID == "" {
		return fmt.Errorf("conclusion successor_item_id empty: %w", ErrInvalidConclusion)
	}
	if c.Cause == ConcludedByDecision {
		if c.Decision == nil || c.Reason != "" {
			return fmt.Errorf("decision conclusion needs a decision and no reason: %w", ErrInvalidConclusion)
		}
		return c.Decision.validate()
	}
	if c.Decision != nil {
		return fmt.Errorf("%s conclusion carries a decision: %w", c.Cause, ErrInvalidConclusion)
	}
	if strings.TrimSpace(c.Reason) == "" || !utf8.ValidString(c.Reason) || len(c.Reason) > maxConclusionReasonBytes {
		return fmt.Errorf("%s conclusion reason: %w", c.Cause, ErrInvalidConclusion)
	}
	return nil
}

func (d ConclusionDecision) validate() error {
	if d.CommandID == "" || d.DeviceID == "" || d.DeviceDisplayName == "" {
		return fmt.Errorf("conclusion decision identity: %w", ErrInvalidConclusion)
	}
	if !d.Action.valid() {
		return fmt.Errorf("conclusion decision action %q: %w", d.Action, ErrInvalidConclusion)
	}
	if d.AnswerRoute != nil && !d.AnswerRoute.valid() {
		return fmt.Errorf("conclusion decision answer_route %q: %w", *d.AnswerRoute, ErrInvalidConclusion)
	}
	if d.FindingRoutes == nil {
		return nil
	}
	if d.Action != ActionAdjudicateFindings && d.Action != ActionChooseAlternativeRoute {
		return fmt.Errorf("conclusion decision %s carries finding routes: %w", d.Action, ErrInvalidConclusion)
	}
	if len(d.FindingRoutes) == 0 {
		return fmt.Errorf("conclusion decision finding_routes empty: %w", ErrInvalidConclusion)
	}
	for index, route := range d.FindingRoutes {
		if route.FindingID == "" || !route.Route.valid() {
			return fmt.Errorf("conclusion decision finding_routes[%d]: %w", index, ErrInvalidConclusion)
		}
	}
	// One canonical order, strictly by finding, so a replayed conclusion
	// encodes to the same bytes and no finding is routed twice.
	for index := 1; index < len(d.FindingRoutes); index++ {
		if d.FindingRoutes[index-1].FindingID >= d.FindingRoutes[index].FindingID {
			return fmt.Errorf("conclusion decision finding_routes not strictly sorted at %d: %w",
				index, ErrInvalidConclusion)
		}
	}
	return nil
}

// validateConclusion checks the item's conclusion against its status and
// decision stamp. It runs on every store decode, so a row whose conclusion
// disagrees with its status is refused rather than repaired. A closed item
// with no conclusion passes: rows closed before the field existed carry none,
// and the requirement that a close records one is the transition's
// (ValidateAttentionItemTransition), not a structural invariant.
func (i AttentionItem) validateConclusion() error {
	if i.Conclusion == nil {
		return nil
	}
	c := *i.Conclusion
	if i.Status == StatusOpen {
		return fmt.Errorf("item %s: %w", i.ID, ErrConclusionOnOpenItem)
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("item %s: %w", i.ID, err)
	}
	if !c.Cause.admits(i.Status) {
		return fmt.Errorf("item %s status %q with conclusion cause %q: %w",
			i.ID, i.Status, c.Cause, ErrConclusionStatusMismatch)
	}
	if c.SuccessorItemID != nil && *c.SuccessorItemID == i.ID {
		return fmt.Errorf("item %s names itself as successor: %w", i.ID, ErrInvalidConclusion)
	}
	if c.Cause == ConcludedByDecision {
		if i.DecidedAt == nil || !i.DecidedAt.Equal(c.ConcludedAt) {
			return fmt.Errorf("item %s decision conclusion and decided_at disagree: %w", i.ID, ErrInvalidConclusion)
		}
		return nil
	}
	if i.DecidedAt != nil {
		return fmt.Errorf("item %s %s conclusion with decided_at: %w", i.ID, c.Cause, ErrInvalidConclusion)
	}
	return nil
}

// WithConclusion returns a copy of the item carrying c. It is the one writer
// of Conclusion: the item must already carry its closing status (and, for a
// decision, its decided_at), and an item that already records a conclusion
// cannot take a second.
func (i AttentionItem) WithConclusion(c Conclusion) (AttentionItem, error) {
	if i.Conclusion != nil {
		return AttentionItem{}, fmt.Errorf("item %s conclusion already recorded: %w", i.ID, ErrImmutableTransition)
	}
	i.Conclusion = &c
	if err := i.validateConclusion(); err != nil {
		return AttentionItem{}, err
	}
	return i, nil
}

// ConcludeByCommand returns a copy of the open item closed by an accepted
// command: status is the action's outcome, and decided_at and the decision
// conclusion carry the same accepted instant. It leaves item_version to the
// caller, which owns the version bump of the write it is making.
func (i AttentionItem) ConcludeByCommand(status ItemStatus, at time.Time, decision ConclusionDecision) (AttentionItem, error) {
	if i.Status != StatusOpen {
		return AttentionItem{}, fmt.Errorf("item %s status %q is already closed: %w", i.ID, i.Status, ErrImmutableTransition)
	}
	conclusion, err := NewDecisionConclusion(at, decision)
	if err != nil {
		return AttentionItem{}, fmt.Errorf("item %s: %w", i.ID, err)
	}
	i.Status = status
	if i, err = i.WithDecidedAt(at); err != nil {
		return AttentionItem{}, err
	}
	return i.WithConclusion(conclusion)
}

// ConcludeByDaemon returns a copy of the open item closed by the daemon for
// cause: the status the cause names (expired, superseded, or resolved) and a
// conclusion carrying reason and, when the daemon minted a replacement, its
// successor. It leaves item_version to the caller, which owns the version
// bump of the write it is making.
func (i AttentionItem) ConcludeByDaemon(cause ConclusionCause, at time.Time, reason string, successor *ItemID) (AttentionItem, error) {
	if i.Status != StatusOpen {
		return AttentionItem{}, fmt.Errorf("item %s status %q is already closed: %w", i.ID, i.Status, ErrImmutableTransition)
	}
	var status ItemStatus
	switch cause {
	case ConcludedByExpiry:
		status = StatusExpired
	case ConcludedBySupersession:
		status = StatusSuperseded
	case ConcludedByDaemon:
		status = StatusResolved
	case ConcludedByDecision:
		return AttentionItem{}, fmt.Errorf("item %s: a decision concludes through its command: %w", i.ID, ErrInvalidConclusion)
	}
	if status == "" {
		return AttentionItem{}, fmt.Errorf("item %s conclusion cause %q: %w", i.ID, cause, ErrInvalidConclusion)
	}
	conclusion, err := NewDaemonConclusion(cause, at, reason, successor)
	if err != nil {
		return AttentionItem{}, fmt.Errorf("item %s: %w", i.ID, err)
	}
	i.Status = status
	return i.WithConclusion(conclusion)
}
