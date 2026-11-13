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
		FailureCauseMemoryLimit, FailureCauseInvalidEvidence, FailureCauseUnknown:
	}
	return "", false
}

// AlternateAgentOffer is one alternate a card or a parked task offers in
// place of what failed: the enrollment it would run, the pool it draws on,
// its harness, the model the role's lineup line runs on it, and the rule that
// qualified it. It is daemon-derived at the instant the card or hold was
// written; acceptance and admission re-derive the rule rather than trust it.
//
// Agent is the tree agent name an offer written before #2735 holds in place
// of Enrollment. Such an offer still decodes for display, but no choice
// matches it: the next failure or park evaluation offers enrollments.
type AlternateAgentOffer struct {
	Agent         string                      `json:"agent,omitempty"`
	Enrollment    ClientEnrollmentID          `json:"enrollment,omitempty"`
	Pool          UsagePoolID                 `json:"pool"`
	Harness       HarnessClientKind           `json:"harness"`
	Model         string                      `json:"model"`
	Qualification AlternateAgentQualification `json:"qualification"`
}

// key is what an offer is sorted and chosen by: its enrollment, or a
// pre-#2735 offer's agent name.
func (o AlternateAgentOffer) key() string {
	if o.Agent != "" {
		return o.Agent
	}
	return string(o.Enrollment)
}

// Validate reports whether the offer names exactly one of an enrollment or a
// pre-#2735 tree agent, and a pool, a harness, a model, and a registered rule.
func (o AlternateAgentOffer) Validate() error {
	switch {
	case o.Agent != "" && o.Enrollment != "":
		return fmt.Errorf("alternate agent offer names an agent and an enrollment: %w", ErrCardFactInconsistent)
	case o.Agent == "" && (o.Enrollment == "" || !ValidLaunchOptionValue(string(o.Enrollment))):
		return fmt.Errorf("alternate agent offer enrollment: %w", ErrEmptyID)
	case o.Agent != "" && !validAgentName(o.Agent):
		return fmt.Errorf("alternate agent offer agent: %w", ErrInvalidAgentName)
	case o.Pool == "":
		return fmt.Errorf("alternate agent offer %s pool: %w", o.key(), ErrEmptyID)
	case !o.Harness.valid():
		return fmt.Errorf("alternate agent offer %s harness %q: %w", o.key(), o.Harness, ErrInvalidHarnessClientKind)
	case strings.TrimSpace(o.Model) == "":
		return fmt.Errorf("alternate agent offer %s model: %w", o.key(), ErrEmptyField)
	case !o.Qualification.valid():
		return fmt.Errorf("alternate agent offer %s qualification %q: %w",
			o.key(), o.Qualification, ErrInvalidAlternateAgentQualification)
	}
	return nil
}

// ValidateAlternateAgentOffers checks a set of offers: none is empty, each is
// valid, all are of one shape (enrollments, or a pre-#2735 set's agents), and
// they are sorted by that key with none twice, so one set has one byte form
// and a choice names exactly one offer.
func ValidateAlternateAgentOffers(offers []AlternateAgentOffer) error {
	if len(offers) == 0 {
		return fmt.Errorf("alternate agent offers: %w", ErrEmptyField)
	}
	for i, offer := range offers {
		if err := offer.Validate(); err != nil {
			return err
		}
		if i == 0 {
			continue
		}
		if (offers[i-1].Agent == "") != (offer.Agent == "") {
			return fmt.Errorf("alternate agent offers mix agents and enrollments: %w", ErrCardFactInconsistent)
		}
		if offers[i-1].key() >= offer.key() {
			return fmt.Errorf("alternate agent offers are not sorted by unique enrollment: %w", ErrCardFactInconsistent)
		}
	}
	return nil
}

// SortAlternateAgentOffers puts offers in the order
// ValidateAlternateAgentOffers requires.
func SortAlternateAgentOffers(offers []AlternateAgentOffer) {
	slices.SortFunc(offers, func(a, b AlternateAgentOffer) int { return strings.Compare(a.key(), b.key()) })
}

// OfferedEnrollment returns the offer for enrollment, or false when the set
// offers none: a pre-#2735 set offers no enrollment.
func OfferedEnrollment(offers []AlternateAgentOffer, enrollment ClientEnrollmentID) (AlternateAgentOffer, bool) {
	i := slices.IndexFunc(offers, func(offer AlternateAgentOffer) bool {
		return offer.Agent == "" && offer.Enrollment == enrollment
	})
	if i < 0 {
		return AlternateAgentOffer{}, false
	}
	return offers[i], true
}

// AlternateAgentChoice is what an operator picks from the offers: an offered
// enrollment and, where the operator picks them, a model and an effort. An
// empty model or effort takes the role's lineup line's.
type AlternateAgentChoice struct {
	EnrollmentID ClientEnrollmentID `json:"enrollment_id"`
	ModelID      string             `json:"model_id"`
	Effort       string             `json:"effort"`
}

// Validate reports whether the choice names an enrollment and any model or
// effort it names is one launch option value. Like
// TaskLineSelection.Validate, the error names the field and never the text.
func (c AlternateAgentChoice) Validate() error {
	for _, field := range []struct {
		name, value string
		required    bool
	}{
		{"enrollment_id", string(c.EnrollmentID), true}, {"model_id", c.ModelID, false}, {"effort", c.Effort, false},
	} {
		if (field.required || field.value != "") && (field.value == "" || !ValidLaunchOptionValue(field.value)) {
			return fmt.Errorf("alternate agent choice %s: %w", field.name, ErrInvalidTaskLineSelection)
		}
	}
	return nil
}

// Message is the choice in the one byte form a retry_with_alternate_agent
// command stores as its message, so a replay compares the same choice. It is
// JSON rather than TaskLineSelection.Text because an enrollment id holds a
// slash, so the text form cannot be split back.
func (c AlternateAgentChoice) Message() string {
	body, err := json.Marshal(c)
	if err != nil {
		// Three strings always marshal.
		panic(fmt.Sprintf("alternate agent choice: %v", err))
	}
	return string(body)
}

// ParseAlternateAgentChoice reads a command message Message wrote. A message
// in any other byte form, or naming an invalid choice, is refused, so one
// choice has one stored message.
func ParseAlternateAgentChoice(message string) (AlternateAgentChoice, error) {
	var choice AlternateAgentChoice
	if err := json.Unmarshal([]byte(message), &choice); err != nil {
		return AlternateAgentChoice{}, fmt.Errorf("alternate agent choice: %w", ErrInvalidTaskLineSelection)
	}
	if err := choice.Validate(); err != nil {
		return AlternateAgentChoice{}, err
	}
	if choice.Message() != message {
		return AlternateAgentChoice{}, fmt.Errorf("alternate agent choice is not canonical: %w",
			ErrInvalidTaskLineSelection)
	}
	return choice, nil
}

// Chose reports whether selection is what the choice picks: the choice's
// enrollment and, where it names them, its model and effort.
func (c AlternateAgentChoice) Chose(selection TaskLineSelection) bool {
	return selection.EnrollmentID == c.EnrollmentID &&
		(c.ModelID == "" || c.ModelID == selection.ModelID) && (c.Effort == "" || c.Effort == selection.Effort)
}

// The encodings AlternateAgentRetry.ComputeID digests. A change to an
// encoding changes every record's identity, and an attempt or a line cites a
// record by it, so a new shape is a new encoding beside the old one and a
// stored record keeps the encoding it was written under: v1 names a tree
// agent (records before #2735), v2 a selection.
const (
	alternateAgentRetryEncodingV1 = "freeside.alternate_agent_retry/v1"
	alternateAgentRetryEncodingV2 = "freeside.alternate_agent_retry/v2"
)

// AlternateAgentRetry is the card record of plan §5.4 admission step 5: an
// operator's choice to run a role under another enrollment, model, and
// effort after a failure the agent's quota, credential, or capacity caused. A retry from a card cites
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
	CommandID string `json:"command_id"`
	// Agent is a v1 record's tree agent name, empty on every v2 record.
	Agent string `json:"agent,omitempty"`
	// Selection is a v2 record's chosen enrollment, model, and effort, nil on
	// every v1 record. Which of the two is set is the record's encoding.
	Selection     *TaskLineSelection          `json:"selection,omitempty"`
	Pool          UsagePoolID                 `json:"pool"`
	Qualification AlternateAgentQualification `json:"qualification"`
	RecordedAt    time.Time                   `json:"recorded_at"`
}

// NewAlternateAgentRetry builds a validated v2 record in canonical
// byte-form: the selection is detached, the instant is normalized to UTC, and
// the ID is computed last. Nothing writes a v1 record any more.
func NewAlternateAgentRetry(record AlternateAgentRetry) (AlternateAgentRetry, error) {
	if record.Agent != "" || record.Selection == nil {
		return AlternateAgentRetry{}, fmt.Errorf("alternate agent retry selection: %w", ErrInvalidTaskLineSelection)
	}
	record.Selection = clonePtr(record.Selection)
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

// canonicalAlternateAgentRetry is the v1 serialization ComputeID digests:
// every bound fact and nothing derived (ID is this value).
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

// canonicalAlternateAgentRetryV2 is the v2 serialization: the selection in
// place of the agent.
type canonicalAlternateAgentRetryV2 struct {
	Version            string                      `json:"version"`
	TaskID             TaskID                      `json:"task_id"`
	Role               RoleName                    `json:"role"`
	FailedInvocationID InvocationID                `json:"failed_invocation_id"`
	ItemID             ItemID                      `json:"item_id"`
	CommandID          string                      `json:"command_id"`
	Selection          *TaskLineSelection          `json:"selection"`
	Pool               UsagePoolID                 `json:"pool"`
	Qualification      AlternateAgentQualification `json:"qualification"`
	RecordedAt         time.Time                   `json:"recorded_at"`
}

// ComputeID returns the record's content address: a sha256 over the
// canonical serialization of its encoding, v2 when it holds a selection.
func (r AlternateAgentRetry) ComputeID() (Digest, error) {
	var canonical any = canonicalAlternateAgentRetry{
		Version: alternateAgentRetryEncodingV1, TaskID: r.TaskID, Role: r.Role,
		FailedInvocationID: r.FailedInvocationID, ItemID: r.ItemID, CommandID: r.CommandID,
		Agent: r.Agent, Pool: r.Pool, Qualification: r.Qualification, RecordedAt: r.RecordedAt,
	}
	if r.Selection != nil {
		canonical = canonicalAlternateAgentRetryV2{
			Version: alternateAgentRetryEncodingV2, TaskID: r.TaskID, Role: r.Role,
			FailedInvocationID: r.FailedInvocationID, ItemID: r.ItemID, CommandID: r.CommandID,
			Selection: r.Selection, Pool: r.Pool, Qualification: r.Qualification, RecordedAt: r.RecordedAt,
		}
	}
	body, err := json.Marshal(canonical)
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
	case r.Agent != "" && r.Selection != nil:
		return fmt.Errorf("alternate agent retry names an agent and a selection: %w",
			ErrAlternateAgentRetryInconsistent)
	case r.Selection != nil && r.Selection.Validate() != nil:
		return fmt.Errorf("alternate agent retry: %w", r.Selection.Validate())
	case r.Selection == nil && !validAgentName(r.Agent):
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
