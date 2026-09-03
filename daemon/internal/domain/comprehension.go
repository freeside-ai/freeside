package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// Comprehension telemetry records the six decision-path signals plan §8 and §9
// Measurement prescribe, so the wave-10 exit evaluation reads recorded facts
// instead of impressions. The records are small and typed: identifiers,
// digests, and instants, never prose. They are policy-input telemetry, kept
// structurally unreachable by policy evaluation (the §5.13 advisory-segregation
// discipline the store enforces). None of these records ever widens an item's
// actions or authorizes a command.

// ClientCapabilityContract is the set of decision actions one paired device's
// app build can present and submit (plan §5.14). The daemon intersects it with
// an item's requested decisions to derive a DecisionActionSurface. Digest
// content-addresses the action set alone, so every device running one app build
// shares a digest; DeviceID is carried for attribution, not for identity.
type ClientCapabilityContract struct {
	DeviceID DeviceID `json:"device_id"`
	Actions  []Action `json:"actions"`
	Digest   Digest   `json:"digest"`
}

// clientCapabilityContractPreimage is the digest preimage: the canonical action
// set alone, so one app build's contract shares a digest across devices.
type clientCapabilityContractPreimage struct {
	Actions []Action `json:"actions"`
}

// NewClientCapabilityContract canonicalizes the action set, content-addresses
// it, and validates the completed contract. It rejects an empty set or an
// action outside AllActions. No later path takes the action list from a caller:
// the daemon rebuilds the contract from the registered set.
func NewClientCapabilityContract(deviceID DeviceID, actions []Action) (ClientCapabilityContract, error) {
	c := ClientCapabilityContract{DeviceID: deviceID, Actions: canonicalActions(actions)}
	digest, err := c.ComputeDigest()
	if err != nil {
		return ClientCapabilityContract{}, err
	}
	c.Digest = digest
	if err := c.Validate(); err != nil {
		return ClientCapabilityContract{}, err
	}
	return c, nil
}

// ComputeDigest returns the content address of the canonical action set.
func (c ClientCapabilityContract) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(clientCapabilityContractPreimage{Actions: canonicalActions(c.Actions)})
	if err != nil {
		return "", fmt.Errorf("client capability contract canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the contract is well-formed: a device, a non-empty
// canonical set of valid actions, and a Digest equal to its recomputation.
func (c ClientCapabilityContract) Validate() error {
	if c.DeviceID == "" {
		return fmt.Errorf("client capability contract device_id: %w", ErrEmptyID)
	}
	if len(c.Actions) == 0 {
		return fmt.Errorf("client capability contract actions: %w", ErrEmptyField)
	}
	for _, a := range c.Actions {
		if !a.valid() {
			return fmt.Errorf("client capability contract action %q: %w", a, ErrInvalidAction)
		}
	}
	if !slices.Equal(c.Actions, canonicalActions(c.Actions)) {
		return fmt.Errorf("client capability contract actions %v: %w", c.Actions, ErrActionsNotCanonical)
	}
	want, err := c.ComputeDigest()
	if err != nil {
		return err
	}
	if c.Digest != want {
		return fmt.Errorf("client capability contract digest %q, recomputed %q: %w",
			c.Digest, want, ErrCapabilityContractDigestMismatch)
	}
	return nil
}

// DecisionActionSurface is the daemon-derived record of the actions actually
// offered to one device for one item's decision surface (plan §5.14): the
// intersection of the item's requested decisions and the device's capability
// contract. It is telemetry evidence only — it never widens the item's actions
// and never authorizes a command. Digest content-addresses the four binding
// fields (item id is already encoded by ItemDecisionSurfaceDigest), so an
// action-taken event and its command reference the exact surface the daemon
// derived.
type DecisionActionSurface struct {
	DeviceID                  DeviceID `json:"device_id"`
	ItemID                    ItemID   `json:"item_id"`
	ItemDecisionSurfaceDigest Digest   `json:"item_decision_surface_digest"`
	ClientCapabilityDigest    Digest   `json:"client_capability_digest"`
	Actions                   []Action `json:"actions"`
	Digest                    Digest   `json:"digest"`
}

// decisionActionSurfacePreimage is the digest preimage: the four binding fields
// the record content-addresses. ItemID is omitted because
// ItemDecisionSurfaceDigest already encodes the item and its epoch.
type decisionActionSurfacePreimage struct {
	DeviceID                  DeviceID `json:"device_id"`
	ItemDecisionSurfaceDigest Digest   `json:"item_decision_surface_digest"`
	ClientCapabilityDigest    Digest   `json:"client_capability_digest"`
	Actions                   []Action `json:"actions"`
}

// DeriveDecisionActionSurface computes the offered-action intersection for a
// device and an item's current decision surface, content-addresses it, and
// validates it. Actions is the sorted intersection of the surface's requested
// decisions and the contract's actions; an empty intersection is valid (the
// item offers the device nothing). No action list is taken from a caller.
func DeriveDecisionActionSurface(deviceID DeviceID, surface DecisionSurface, contract ClientCapabilityContract) (DecisionActionSurface, error) {
	if err := surface.Validate(); err != nil {
		return DecisionActionSurface{}, err
	}
	if err := contract.Validate(); err != nil {
		return DecisionActionSurface{}, err
	}
	if contract.DeviceID != deviceID {
		return DecisionActionSurface{}, fmt.Errorf(
			"decision action surface device %q, contract device %q: %w",
			deviceID, contract.DeviceID, ErrDecisionActionSurfaceInconsistent)
	}
	s := DecisionActionSurface{
		DeviceID:                  deviceID,
		ItemID:                    surface.ItemID,
		ItemDecisionSurfaceDigest: surface.Digest,
		ClientCapabilityDigest:    contract.Digest,
		Actions:                   intersectActions(surface.RequestedDecision, contract.Actions),
	}
	digest, err := s.ComputeDigest()
	if err != nil {
		return DecisionActionSurface{}, err
	}
	s.Digest = digest
	if err := s.Validate(); err != nil {
		return DecisionActionSurface{}, err
	}
	return s, nil
}

// ComputeDigest returns the content address of the four binding fields.
func (s DecisionActionSurface) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(decisionActionSurfacePreimage{
		DeviceID:                  s.DeviceID,
		ItemDecisionSurfaceDigest: s.ItemDecisionSurfaceDigest,
		ClientCapabilityDigest:    s.ClientCapabilityDigest,
		Actions:                   canonicalActions(s.Actions),
	})
	if err != nil {
		return "", fmt.Errorf("decision action surface canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the surface is well-formed and its Digest equals its
// recomputation, so a decoded or caller-supplied record with a substituted
// action set or digest is refused before any consumer compares against it.
func (s DecisionActionSurface) Validate() error {
	if s.DeviceID == "" {
		return fmt.Errorf("decision action surface device_id: %w", ErrEmptyID)
	}
	if s.ItemID == "" {
		return fmt.Errorf("decision action surface item_id: %w", ErrEmptyID)
	}
	if !isSHA256Digest(string(s.ItemDecisionSurfaceDigest)) {
		return fmt.Errorf("decision action surface item_decision_surface_digest %q: %w",
			s.ItemDecisionSurfaceDigest, ErrInvalidDigest)
	}
	if !isSHA256Digest(string(s.ClientCapabilityDigest)) {
		return fmt.Errorf("decision action surface client_capability_digest %q: %w",
			s.ClientCapabilityDigest, ErrInvalidDigest)
	}
	for _, a := range s.Actions {
		if !a.valid() {
			return fmt.Errorf("decision action surface action %q: %w", a, ErrInvalidAction)
		}
	}
	if !slices.Equal(s.Actions, canonicalActions(s.Actions)) {
		return fmt.Errorf("decision action surface actions %v: %w", s.Actions, ErrActionsNotCanonical)
	}
	want, err := s.ComputeDigest()
	if err != nil {
		return err
	}
	if s.Digest != want {
		return fmt.Errorf("decision action surface digest %q, recomputed %q: %w",
			s.Digest, want, ErrDecisionActionSurfaceDigestMismatch)
	}
	return nil
}

// Offers reports whether the derived surface contains an action, which the
// override classification uses to separate a voluntary override (the accepted
// action differed from a recommendation the surface still offered) from a
// forced one (the surface omitted the recommendation).
func (s DecisionActionSurface) Offers(action Action) bool {
	return slices.Contains(s.Actions, action)
}

// intersectActions returns the canonical set of actions present in both inputs.
func intersectActions(a, b []Action) []Action {
	present := make(map[Action]struct{}, len(b))
	for _, x := range b {
		present[x] = struct{}{}
	}
	out := make([]Action, 0, len(a))
	for _, x := range a {
		if _, ok := present[x]; ok {
			out = append(out, x)
		}
	}
	return canonicalActions(out)
}

// ComprehensionEvent is one typed decision-path event a client emits and the
// daemon records (plan §8, §9). Ingestion follows the delivery-receipt
// discipline: it records the fact, makes no judgment, and has no version
// precondition. EventID is the client-generated idempotency key; ReceivedAt is
// daemon-stamped and so is absent from ComprehensionEventInput. Every event
// carries the item decision surface digest it was rendered against; the
// action-bearing kinds additionally reference the exact action surface and the
// accepted command, and every other kind carries neither.
type ComprehensionEvent struct {
	DeviceID                    DeviceID               `json:"device_id"`
	EventID                     string                 `json:"event_id"`
	ItemID                      ItemID                 `json:"item_id"`
	Kind                        ComprehensionEventKind `json:"kind"`
	ItemDecisionSurfaceDigest   Digest                 `json:"item_decision_surface_digest"`
	DecisionActionSurfaceDigest *Digest                `json:"decision_action_surface_digest"`
	CommandID                   string                 `json:"command_id"`
	OccurredAt                  time.Time              `json:"occurred_at"`
	Sequence                    int                    `json:"sequence"`
	ReceivedAt                  time.Time              `json:"received_at"`
}

// ComprehensionEventInput carries the client-supplied fields of a
// ComprehensionEvent. It omits ReceivedAt, which the daemon stamps at
// ingestion.
type ComprehensionEventInput struct {
	DeviceID                    DeviceID
	EventID                     string
	ItemID                      ItemID
	Kind                        ComprehensionEventKind
	ItemDecisionSurfaceDigest   Digest
	DecisionActionSurfaceDigest *Digest
	CommandID                   string
	OccurredAt                  time.Time
	Sequence                    int
}

// NewComprehensionEvent stamps the daemon-assigned ReceivedAt and validates the
// completed event.
func NewComprehensionEvent(in ComprehensionEventInput, receivedAt time.Time) (ComprehensionEvent, error) {
	e := ComprehensionEvent{
		DeviceID:                    in.DeviceID,
		EventID:                     in.EventID,
		ItemID:                      in.ItemID,
		Kind:                        in.Kind,
		ItemDecisionSurfaceDigest:   in.ItemDecisionSurfaceDigest,
		DecisionActionSurfaceDigest: clonePtr(in.DecisionActionSurfaceDigest),
		CommandID:                   in.CommandID,
		OccurredAt:                  in.OccurredAt,
		Sequence:                    in.Sequence,
		ReceivedAt:                  receivedAt,
	}
	if err := e.Validate(); err != nil {
		return ComprehensionEvent{}, err
	}
	return e, nil
}

// Validate reports whether the event is well-formed and matches its kind's
// field contract.
func (e ComprehensionEvent) Validate() error {
	if e.DeviceID == "" {
		return fmt.Errorf("comprehension event device_id: %w", ErrEmptyID)
	}
	if e.EventID == "" {
		return fmt.Errorf("comprehension event event_id: %w", ErrEmptyID)
	}
	if e.ItemID == "" {
		return fmt.Errorf("comprehension event %s item_id: %w", e.EventID, ErrEmptyID)
	}
	if !e.Kind.valid() {
		return fmt.Errorf("comprehension event %s kind %q: %w", e.EventID, e.Kind, ErrInvalidComprehensionEventKind)
	}
	if !isSHA256Digest(string(e.ItemDecisionSurfaceDigest)) {
		return fmt.Errorf("comprehension event %s item_decision_surface_digest %q: %w",
			e.EventID, e.ItemDecisionSurfaceDigest, ErrInvalidDigest)
	}
	if e.Sequence < 1 {
		return fmt.Errorf("comprehension event %s sequence %d: %w", e.EventID, e.Sequence, ErrNonPositive)
	}
	if err := requireUTC(e.OccurredAt, fmt.Sprintf("comprehension event %s occurred_at", e.EventID)); err != nil {
		return err
	}
	if err := requireUTC(e.ReceivedAt, fmt.Sprintf("comprehension event %s received_at", e.EventID)); err != nil {
		return err
	}
	// The action-bearing kinds reference the exact action surface and its
	// accepted command; every other kind carries neither, so a stray reference
	// is a contract violation rather than silently accepted telemetry.
	if e.Kind.requiresDecisionCommand() {
		if e.DecisionActionSurfaceDigest == nil || !isSHA256Digest(string(*e.DecisionActionSurfaceDigest)) {
			return fmt.Errorf("comprehension event %s %s decision_action_surface_digest: %w",
				e.EventID, e.Kind, ErrComprehensionEventInconsistent)
		}
		if e.CommandID == "" {
			return fmt.Errorf("comprehension event %s %s command_id: %w",
				e.EventID, e.Kind, ErrComprehensionEventInconsistent)
		}
		return nil
	}
	if e.DecisionActionSurfaceDigest != nil {
		return fmt.Errorf("comprehension event %s %s carries decision_action_surface_digest: %w",
			e.EventID, e.Kind, ErrComprehensionEventInconsistent)
	}
	if e.CommandID != "" {
		return fmt.Errorf("comprehension event %s %s carries command_id: %w",
			e.EventID, e.Kind, ErrComprehensionEventInconsistent)
	}
	return nil
}

// requiresDecisionCommand reports whether the kind references an accepted
// command and its derived action surface: action_taken and
// recommendation_override do, and every other kind does not. The switch omits
// default so a new kind must be classified here.
func (k ComprehensionEventKind) requiresDecisionCommand() bool {
	switch k {
	case ComprehensionActionTaken, ComprehensionRecommendationOverride:
		return true
	case ComprehensionCardOpened, ComprehensionDrillDownOpened,
		ComprehensionDetailsOpenedBeforeActing, ComprehensionNotDecidableHereShown:
		return false
	}
	return false
}

// ComprehensionDefect records one comprehension defect the operator found for
// an item (plan §9: the comprehension-defect count). Finding defects stays
// manual; this type only records one the operator identified. Reason is capped
// so a row stays small.
type ComprehensionDefect struct {
	ItemID      ItemID    `json:"item_id"`
	ClaimDigest Digest    `json:"claim_digest"`
	RecordedAt  time.Time `json:"recorded_at"`
	Reason      string    `json:"reason"`
}

// maxComprehensionDefectReasonBytes caps the operator-supplied defect reason so
// a telemetry row stays small.
const maxComprehensionDefectReasonBytes = 1 << 10 // 1 KiB

// Validate reports whether the defect is well-formed.
func (d ComprehensionDefect) Validate() error {
	if d.ItemID == "" {
		return fmt.Errorf("comprehension defect item_id: %w", ErrEmptyID)
	}
	if !isSHA256Digest(string(d.ClaimDigest)) {
		return fmt.Errorf("comprehension defect claim_digest %q: %w", d.ClaimDigest, ErrInvalidDigest)
	}
	if err := requireUTC(d.RecordedAt, "comprehension defect recorded_at"); err != nil {
		return err
	}
	if d.Reason == "" {
		return fmt.Errorf("comprehension defect reason: %w", ErrEmptyField)
	}
	if len(d.Reason) > maxComprehensionDefectReasonBytes {
		return fmt.Errorf("comprehension defect reason %d bytes: %w", len(d.Reason), ErrComprehensionDefectTooLarge)
	}
	return nil
}

// requireUTC reports whether t is a non-zero UTC instant, using label for
// context. It centralizes the identity-timestamp check the telemetry records
// share.
func requireUTC(t time.Time, label string) error {
	if t.IsZero() {
		return fmt.Errorf("%s: %w", label, ErrMissingTimestamp)
	}
	if t.Location() != time.UTC {
		return fmt.Errorf("%s: %w", label, ErrTimestampNotUTC)
	}
	return nil
}
