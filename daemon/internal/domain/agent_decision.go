package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// Bounds on the decisions an agent may hand back instead of a result. They
// keep a card seconds-readable (plan §9) and bound the artifact both
// producers record. Only a decision that blocks an implementation-ready
// specification belongs here: product, policy, compatibility, security
// posture, data migration, or scope. A detail with one repository-practice
// default stays a bounded assumption.
const (
	MaxDecisionsPerResult = 8
	MinDecisionOptions    = 2
	MaxDecisionOptions    = 6
	MaxDecisionTextBytes  = 4 << 10

	BlockedOutcomeEncodingVersion = "freeside.blocked-outcome/v1"
	MaxBlockedOutcomeBytes        = strictjson.Limit(64 << 10)
)

// DecisionOption is one enumerated answer to a Decision.
type DecisionOption struct {
	Label     string `json:"label"`
	Tradeoffs string `json:"tradeoffs"`
}

// Decision is one question an agent cannot answer without the human. It is
// shared by the specifier's needs_decision output, the implementer's blocked
// outcome, and the agent_question card. Recommendation is an agent claim
// naming one option label exactly; the card labels it as such.
type Decision struct {
	Question       string           `json:"question"`
	WhyBlocking    string           `json:"why_blocking"`
	Options        []DecisionOption `json:"options"`
	Recommendation string           `json:"recommendation"`
}

func validDecisionText(text string) bool {
	return text != "" && text == strings.TrimSpace(text) &&
		utf8.ValidString(text) && len(text) <= MaxDecisionTextBytes
}

// Validate enforces the per-decision limits: trimmed non-empty UTF-8 text
// within MaxDecisionTextBytes, two to six uniquely labeled options, and a
// recommendation equal to one option's label.
func (d Decision) Validate() error {
	if !validDecisionText(d.Question) {
		return fmt.Errorf("decision question: %w", ErrDecisionInvalid)
	}
	if !validDecisionText(d.WhyBlocking) {
		return fmt.Errorf("decision %q why_blocking: %w", d.Question, ErrDecisionInvalid)
	}
	if len(d.Options) < MinDecisionOptions || len(d.Options) > MaxDecisionOptions {
		return fmt.Errorf("decision %q has %d options, want %d to %d: %w",
			d.Question, len(d.Options), MinDecisionOptions, MaxDecisionOptions, ErrDecisionInvalid)
	}
	labels := make(map[string]struct{}, len(d.Options))
	for index, option := range d.Options {
		if !validDecisionText(option.Label) || !validDecisionText(option.Tradeoffs) {
			return fmt.Errorf("decision %q options[%d]: %w", d.Question, index, ErrDecisionInvalid)
		}
		if _, duplicate := labels[option.Label]; duplicate {
			return fmt.Errorf("decision %q option label %q: %w", d.Question, option.Label, ErrDuplicate)
		}
		labels[option.Label] = struct{}{}
	}
	if _, ok := labels[d.Recommendation]; !ok {
		return fmt.Errorf("decision %q recommendation %q matches no option: %w",
			d.Question, d.Recommendation, ErrDecisionInvalid)
	}
	return nil
}

// ValidateDecisions enforces the per-result bound: one to
// MaxDecisionsPerResult valid decisions.
func ValidateDecisions(decisions []Decision) error {
	if len(decisions) == 0 || len(decisions) > MaxDecisionsPerResult {
		return fmt.Errorf("%d decisions, want 1 to %d: %w",
			len(decisions), MaxDecisionsPerResult, ErrDecisionInvalid)
	}
	for index, decision := range decisions {
		if err := decision.Validate(); err != nil {
			return fmt.Errorf("decisions[%d]: %w", index, err)
		}
	}
	return nil
}

func cloneDecisions(in []Decision) []Decision {
	if in == nil {
		return nil
	}
	out := make([]Decision, len(in))
	for index, decision := range in {
		out[index] = decision
		out[index].Options = append([]DecisionOption(nil), decision.Options...)
	}
	return out
}

// BlockedOutcome is the implementer's typed stop: the kind of blocker and the
// decisions that would unblock it. It travels as the launcher-declared
// .freeside-evidence/blocked.json evidence file and is re-decoded strictly at
// every boundary that reads it.
type BlockedOutcome struct {
	Version   string      `json:"version"`
	Kind      BlockedKind `json:"kind"`
	Decisions []Decision  `json:"decisions"`
}

func (o BlockedOutcome) Validate() error {
	if o.Version != BlockedOutcomeEncodingVersion {
		return fmt.Errorf("blocked outcome version %q: %w", o.Version, ErrBlockedOutcomeInvalid)
	}
	if !o.Kind.valid() {
		return fmt.Errorf("blocked outcome kind %q: %w", o.Kind, ErrInvalidBlockedKind)
	}
	if err := ValidateDecisions(o.Decisions); err != nil {
		return fmt.Errorf("blocked outcome: %w", err)
	}
	return nil
}

// DecodeBlockedOutcome strictly reconstructs and validates one blocked
// outcome from the bytes the implementer wrote.
func DecodeBlockedOutcome(data []byte) (BlockedOutcome, error) {
	var out BlockedOutcome
	if err := strictjson.Decode(data, &out, strictjson.RejectInvalidUTF8, MaxBlockedOutcomeBytes); err != nil {
		return BlockedOutcome{}, fmt.Errorf("decode blocked outcome: %w", err)
	}
	if err := out.Validate(); err != nil {
		return BlockedOutcome{}, err
	}
	return out, nil
}

// EncodeBlockedOutcome emits the canonical bytes a fake driver or fixture
// writes so the real and fake paths authenticate the same content.
func EncodeBlockedOutcome(out BlockedOutcome) ([]byte, error) {
	if err := out.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode blocked outcome: %w", err)
	}
	if len(body) > int(MaxBlockedOutcomeBytes) {
		return nil, fmt.Errorf("encode blocked outcome: %w: got %d bytes, limit %d",
			strictjson.ErrLimitExceeded, len(body), MaxBlockedOutcomeBytes)
	}
	return body, nil
}

// ComputeDigest returns the content address an agent_question's Question
// claim must carry. The two producer stages persist different canonical
// payloads: specification stores the decisions array, while implementation
// stores the versioned blocked outcome that also carries the blocker kind.
func (f AgentQuestionFacts) ComputeDigest() (Digest, error) {
	if err := f.Validate(); err != nil {
		return "", err
	}
	var body []byte
	var err error
	if f.Stage == StageNameSpecification {
		body, err = json.Marshal(f.Decisions)
	} else {
		body, err = EncodeBlockedOutcome(BlockedOutcome{
			Version: BlockedOutcomeEncodingVersion, Kind: *f.Kind, Decisions: f.Decisions,
		})
	}
	if err != nil {
		return "", fmt.Errorf("encode agent question facts: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}
