package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// AlternateAgentQualification is the rule that let an agent stand in for a
// failed one (plan §4, alternate-agent retry). Each failure class has its
// own: an alternate qualifies only when it escapes what failed.
type AlternateAgentQualification string

const (
	// AlternateAgentDifferentPool answers a quota failure: the alternate
	// draws on another usage pool, so the exhausted quota is not its own.
	AlternateAgentDifferentPool AlternateAgentQualification = "different_pool"
	// AlternateAgentValidGeneration answers a credential expiry or
	// revocation: the alternate's enrollment holds a generation valid
	// through the attempt and its margin.
	AlternateAgentValidGeneration AlternateAgentQualification = "valid_generation"
	// AlternateAgentDifferentRoute answers a capacity failure: the alternate
	// reaches the model through another service route.
	AlternateAgentDifferentRoute AlternateAgentQualification = "different_route"
)

// AllAlternateAgentQualifications lists every valid
// AlternateAgentQualification; it is the single registration point.
var AllAlternateAgentQualifications = []AlternateAgentQualification{
	AlternateAgentDifferentPool, AlternateAgentValidGeneration, AlternateAgentDifferentRoute,
}

func (q AlternateAgentQualification) valid() bool {
	switch q {
	case AlternateAgentDifferentPool, AlternateAgentValidGeneration, AlternateAgentDifferentRoute:
		return true
	default:
		return false
	}
}

// QualifyAlternate reports the rule under which alt escapes a failure with
// this cause, or false when it escapes none. It is the per-agent half of
// DecideFailureDisposition: a failure is alternate-eligible exactly when some
// alternate qualifies here. Only quota, credential, and capacity causes have
// a rule; every other cause qualifies no alternate.
func QualifyAlternate(cause FailureCause, facts FailureFacts, alt FailureAlternate) (AlternateAgentQualification, bool) {
	switch cause {
	case FailureCauseQuota:
		return AlternateAgentDifferentPool,
			facts.UsagePool != "" && alt.UsagePool != "" && alt.UsagePool != facts.UsagePool
	case FailureCauseCredentialExpired, FailureCauseCredentialRevoked:
		return AlternateAgentValidGeneration, alt.GenerationValid
	case FailureCauseServiceCapacity:
		return AlternateAgentDifferentRoute, facts.Route != "" && alt.Route != "" && alt.Route != facts.Route
	case FailureCauseProviderUnavailable, FailureCauseInvalidConfiguration, FailureCauseContractContradiction,
		FailureCauseMemoryLimit, FailureCauseUnknown:
	}
	return "", false
}

// AlternateAgentOffer is one agent a card or a parked task offers in place of
// the one that failed: the agent's tree name, the pool it draws on, its
// harness and model, and the rule that qualified it. It is daemon-derived at
// the instant the card or hold was written; acceptance and admission re-derive
// the rule rather than trust it.
type AlternateAgentOffer struct {
	Agent         string                      `json:"agent"`
	Pool          UsagePoolID                 `json:"pool"`
	Harness       HarnessClientKind           `json:"harness"`
	Model         string                      `json:"model"`
	Qualification AlternateAgentQualification `json:"qualification"`
}

// Validate reports whether the offer names a tree agent, a pool, a harness, a
// model, and a registered rule.
func (o AlternateAgentOffer) Validate() error {
	switch {
	case !validAgentName(o.Agent):
		return fmt.Errorf("alternate agent offer agent: %w", ErrInvalidAgentName)
	case o.Pool == "":
		return fmt.Errorf("alternate agent offer %s pool: %w", o.Agent, ErrEmptyID)
	case !o.Harness.valid():
		return fmt.Errorf("alternate agent offer %s harness %q: %w", o.Agent, o.Harness, ErrInvalidHarnessClientKind)
	case strings.TrimSpace(o.Model) == "":
		return fmt.Errorf("alternate agent offer %s model: %w", o.Agent, ErrEmptyField)
	case !o.Qualification.valid():
		return fmt.Errorf("alternate agent offer %s qualification %q: %w",
			o.Agent, o.Qualification, ErrInvalidAlternateAgentQualification)
	}
	return nil
}

// ValidateAlternateAgentOffers checks a set of offers: none is empty, each is
// valid, and they are sorted by agent name with no name twice, so one set has
// one byte form and a choice names exactly one offer.
func ValidateAlternateAgentOffers(offers []AlternateAgentOffer) error {
	if len(offers) == 0 {
		return fmt.Errorf("alternate agent offers: %w", ErrEmptyField)
	}
	for i, offer := range offers {
		if err := offer.Validate(); err != nil {
			return err
		}
		if i > 0 && offers[i-1].Agent >= offer.Agent {
			return fmt.Errorf("alternate agent offers are not sorted by unique agent: %w", ErrCardFactInconsistent)
		}
	}
	return nil
}

// SortAlternateAgentOffers puts offers in the order
// ValidateAlternateAgentOffers requires.
func SortAlternateAgentOffers(offers []AlternateAgentOffer) {
	slices.SortFunc(offers, func(a, b AlternateAgentOffer) int { return strings.Compare(a.Agent, b.Agent) })
}

// alternateAgentRetryEncodingVersion tags the canonical serialization
// AlternateAgentRetry.ComputeID digests. A change to the encoding changes
// every record's identity, and an attempt or a line cites a record by it.
const alternateAgentRetryEncodingVersion = "freeside.alternate_agent_retry/v1"

// AlternateAgentRetry is the card record of plan §5.4 admission step 5: an
// operator's choice to run a role under another agent after a failure the
// agent's quota, credential, or capacity caused. A retry from a card cites
// the card's item; a move from a parked task cites none. The attempt or line
// the choice produces cites this record, and admission re-derives the rule
// against the failed admission before it runs the agent.
type AlternateAgentRetry struct {
	// ID is the content address of every other field.
	ID     Digest   `json:"id"`
	TaskID TaskID   `json:"task_id"`
	Role   RoleName `json:"role"`
	// FailedInvocationID is the invocation whose failure the choice answers.
	FailedInvocationID InvocationID `json:"failed_invocation_id"`
	// ItemID is the card the choice was made on, empty for a parked move.
	ItemID ItemID `json:"item_id"`
	// CommandID is the decision or operator command that made the choice.
	CommandID     string                      `json:"command_id"`
	Agent         string                      `json:"agent"`
	Pool          UsagePoolID                 `json:"pool"`
	Qualification AlternateAgentQualification `json:"qualification"`
	RecordedAt    time.Time                   `json:"recorded_at"`
}

// NewAlternateAgentRetry builds a validated record in canonical byte-form:
// the instant is normalized to UTC and the ID is computed last.
func NewAlternateAgentRetry(record AlternateAgentRetry) (AlternateAgentRetry, error) {
	record.RecordedAt = record.RecordedAt.UTC()
	id, err := record.ComputeID()
	if err != nil {
		return AlternateAgentRetry{}, err
	}
	record.ID = id
	if err := record.Validate(); err != nil {
		return AlternateAgentRetry{}, err
	}
	return record, nil
}

// canonicalAlternateAgentRetry is the versioned serialization ComputeID
// digests: every bound fact and nothing derived (ID is this value).
type canonicalAlternateAgentRetry struct {
	Version            string                      `json:"version"`
	TaskID             TaskID                      `json:"task_id"`
	Role               RoleName                    `json:"role"`
	FailedInvocationID InvocationID                `json:"failed_invocation_id"`
	ItemID             ItemID                      `json:"item_id"`
	CommandID          string                      `json:"command_id"`
	Agent              string                      `json:"agent"`
	Pool               UsagePoolID                 `json:"pool"`
	Qualification      AlternateAgentQualification `json:"qualification"`
	RecordedAt         time.Time                   `json:"recorded_at"`
}

// ComputeID returns the record's content address: a sha256 over its
// versioned canonical serialization.
func (r AlternateAgentRetry) ComputeID() (Digest, error) {
	body, err := json.Marshal(canonicalAlternateAgentRetry{
		Version: alternateAgentRetryEncodingVersion, TaskID: r.TaskID, Role: r.Role,
		FailedInvocationID: r.FailedInvocationID, ItemID: r.ItemID, CommandID: r.CommandID,
		Agent: r.Agent, Pool: r.Pool, Qualification: r.Qualification, RecordedAt: r.RecordedAt,
	})
	if err != nil {
		return "", fmt.Errorf("alternate agent retry id: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the record is well-formed and its identity
// authentic. The store re-runs it on every decode.
func (r AlternateAgentRetry) Validate() error {
	switch {
	case r.TaskID == "":
		return fmt.Errorf("alternate agent retry task_id: %w", ErrEmptyID)
	case !r.Role.TaskLineEligible():
		return fmt.Errorf("alternate agent retry role %q: %w", r.Role, ErrTaskLineRoleIneligible)
	case r.FailedInvocationID == "":
		return fmt.Errorf("alternate agent retry failed_invocation_id: %w", ErrEmptyID)
	case r.CommandID == "":
		return fmt.Errorf("alternate agent retry command_id: %w", ErrEmptyID)
	case !validAgentName(r.Agent):
		return fmt.Errorf("alternate agent retry agent: %w", ErrInvalidAgentName)
	case r.Pool == "":
		return fmt.Errorf("alternate agent retry pool: %w", ErrEmptyID)
	case !r.Qualification.valid():
		return fmt.Errorf("alternate agent retry qualification %q: %w",
			r.Qualification, ErrInvalidAlternateAgentQualification)
	case r.RecordedAt.IsZero():
		return fmt.Errorf("alternate agent retry recorded_at: %w", ErrMissingTimestamp)
	case r.RecordedAt.Location() != time.UTC:
		// recorded_at is part of the canonical encoding the id addresses, so
		// one instant must have one byte form.
		return fmt.Errorf("alternate agent retry recorded_at: %w", ErrTimestampNotUTC)
	}
	computed, err := r.ComputeID()
	if err != nil {
		return err
	}
	if r.ID != computed {
		return fmt.Errorf("alternate agent retry %s, content resolves to %s: %w",
			r.ID, computed, ErrAlternateAgentRetryInconsistent)
	}
	return nil
}

// ParkedMove reports whether the record moved a parked task rather than
// answering a card.
func (r AlternateAgentRetry) ParkedMove() bool { return r.ItemID == "" }
