package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// MaxSpecRevisionCommentBytes bounds one durable revision comment and one
// claimed response. The same limit applies at the request_changes boundary.
const MaxSpecRevisionCommentBytes = 8 << 10

// MaxSpecRevisionComments bounds both the durable comments and the agent's
// claimed addressals on one revised specification.
const MaxSpecRevisionComments = 64

var (
	currencyCodePattern   = regexp.MustCompile(`^[A-Z]{3}$`)
	decimalAmountPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)
	diagnosticCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)
)

// DisplayName is one daemon-authored human-readable label. Source makes an
// identifier fallback explicit instead of presenting it as a chosen name.
type DisplayName struct {
	Text   string            `json:"text"`
	Source DisplayNameSource `json:"source"`
}

func (n DisplayName) Validate() error {
	if n.Text == "" || !n.Source.valid() {
		return fmt.Errorf("display name %q source %q: %w", n.Text, n.Source, ErrCardFactInconsistent)
	}
	return nil
}

// DisplayNames carries the scanning labels for an item's project and work
// unit. Both are always present when the aggregate is populated.
type DisplayNames struct {
	Project DisplayName `json:"project"`
	Task    DisplayName `json:"task"`
}

func (n DisplayNames) Validate() error {
	if err := n.Project.Validate(); err != nil {
		return fmt.Errorf("project: %w", err)
	}
	if err := n.Task.Validate(); err != nil {
		return fmt.Errorf("work unit: %w", err)
	}
	return nil
}

// CostSoFar is the billable-cost aggregate for a diminishing-returns card.
// Complete is false when at least one invocation lacked an observation.
type CostSoFar struct {
	Currency    string `json:"currency"`
	Amount      string `json:"amount"`
	Invocations int    `json:"invocations"`
	Complete    bool   `json:"complete"`
}

func (c CostSoFar) Validate() error {
	if !currencyCodePattern.MatchString(c.Currency) ||
		!decimalAmountPattern.MatchString(c.Amount) || c.Invocations < 1 {
		return fmt.Errorf("cost %q %q across %d invocations: %w",
			c.Currency, c.Amount, c.Invocations, ErrCardFactInconsistent)
	}
	return nil
}

// ExecutionFailureFacts identifies the failed terminal outcome, its cause,
// and the stage and invocation that produced it.
type ExecutionFailureFacts struct {
	Outcome ExecutionOutcomeStatus `json:"outcome"`
	// Cause mirrors ExecutionOutcome.Cause: present exactly when Outcome is
	// failed, where unknown means nothing classified the failure. omitempty
	// keeps a canceled or lost item's stored body byte-identical to the rows
	// written before causes existed.
	Cause            FailureCause              `json:"cause,omitempty"`
	Stage            StageName                 `json:"stage"`
	InvocationID     InvocationID              `json:"invocation_id"`
	OfferedManifests []CapabilityManifestOffer `json:"offered_manifests,omitempty"`
	// Admitted is what ran the failed invocation, read from its execution
	// admission when the item was raised, so an alternate-agent retry (#869)
	// can name the profile that failed. omitempty keeps an item raised before
	// it existed byte-identical to its stored body.
	Admitted *InvocationAdmissionFacts `json:"admitted,omitempty"`
	// HandoffCheck is the outcome's handoff check, copied from it: the ward
	// check that refused the run and its screened evidence.
	CheckFailure *HandoffCheckFailure `json:"handoff_check,omitempty"`
	// ExportRejection is the first few gauntlet findings of an
	// export_rejected failure, projected from the invocation's export
	// rejection record by NewExportRejectionFacts. The record is written
	// best-effort, so a card whose record failed to write has none.
	ExportRejection *ExportRejectionFacts `json:"export_rejection,omitempty"`
	// RetryOutlook says whether a same-agent retry could escape the failure,
	// as DecideRetryOutlook derives it. Every item raised since it existed
	// carries it; an older item has none and is byte-identical to its stored
	// body.
	RetryOutlook RetryOutlook `json:"retry_outlook,omitempty"`
}

func (f ExecutionFailureFacts) Validate() error {
	// A blocked terminal is a question for the human, never a failure card.
	if !f.Outcome.valid() || f.Outcome == ExecutionOutcomeBlocked || !f.Stage.valid() || f.InvocationID == "" {
		return fmt.Errorf("execution failure outcome %q stage %q invocation %q: %w",
			f.Outcome, f.Stage, f.InvocationID, ErrCardFactInconsistent)
	}
	if f.Outcome == ExecutionOutcomeFailed {
		if !f.Cause.valid() {
			return fmt.Errorf("execution failure cause %q: %w", f.Cause, ErrCardFactInconsistent)
		}
	} else if f.Cause != "" {
		return fmt.Errorf("execution failure %s with cause %q: %w", f.Outcome, f.Cause, ErrCardFactInconsistent)
	}
	seen := make(map[Digest]struct{}, len(f.OfferedManifests))
	for index, offer := range f.OfferedManifests {
		if err := offer.Validate(); err != nil {
			return fmt.Errorf("execution failure offered manifest[%d]: %w", index, err)
		}
		if index > 0 && f.OfferedManifests[index-1].Name >= offer.Name {
			return fmt.Errorf("execution failure manifest %q after %q: %w",
				offer.Name, f.OfferedManifests[index-1].Name, ErrKeysNotCanonical)
		}
		if _, duplicate := seen[offer.Digest]; duplicate {
			return fmt.Errorf("execution failure manifest digest %q: %w", offer.Digest, ErrDuplicate)
		}
		seen[offer.Digest] = struct{}{}
	}
	if admitted := f.Admitted; admitted != nil {
		if err := admitted.Validate(); err != nil {
			return fmt.Errorf("execution failure admitted: %w", err)
		}
		if admitted.Kind != AdmissionKindExecution || admitted.InvocationID != f.InvocationID {
			return fmt.Errorf("execution failure %q admitted facts of %s %q: %w",
				f.InvocationID, admitted.Kind, admitted.InvocationID, ErrCardFactInconsistent)
		}
	}
	if err := validateOutcomeHandoffCheck(f.Outcome, f.Cause, f.CheckFailure); err != nil {
		return fmt.Errorf("execution failure %q: %w: %w", f.InvocationID, ErrCardFactInconsistent, err)
	}
	if rejection := f.ExportRejection; rejection != nil {
		if f.Outcome != ExecutionOutcomeFailed || f.Cause != FailureCauseExportRejected {
			return fmt.Errorf("execution failure %s with cause %q and export rejection facts: %w",
				f.Outcome, f.Cause, ErrCardFactInconsistent)
		}
		if err := rejection.Validate(); err != nil {
			return fmt.Errorf("execution failure %q: %w", f.InvocationID, err)
		}
	}
	if f.RetryOutlook != "" {
		if want := DecideRetryOutlook(f.Outcome, f.Cause, f.CheckFailure); f.RetryOutlook != want {
			return fmt.Errorf("execution failure retry outlook %q, derived %q: %w",
				f.RetryOutlook, want, ErrCardFactInconsistent)
		}
	}
	return nil
}

// RetryOutlook says whether retrying a failed attempt on the same agent could
// succeed. It is derived from the recorded failure by DecideRetryOutlook,
// never stored as a property of a cause.
type RetryOutlook string

const (
	// RetryOutlookWouldRepeat: the failure follows from what the run produced
	// or from configuration, so a retry of the same work would fail the same
	// way.
	RetryOutlookWouldRepeat RetryOutlook = "would_repeat"
	// RetryOutlookMaySucceed: nothing recorded shows the failure would recur.
	RetryOutlookMaySucceed RetryOutlook = "may_succeed"
)

// AllRetryOutlooks lists every valid RetryOutlook; it is the single
// registration point.
var AllRetryOutlooks = []RetryOutlook{RetryOutlookWouldRepeat, RetryOutlookMaySucceed}

func (o RetryOutlook) valid() bool {
	switch o {
	case RetryOutlookWouldRepeat, RetryOutlookMaySucceed:
		return true
	default:
		return false
	}
}

// DecideRetryOutlook derives whether a same-agent retry could escape a
// failure. A handoff check that refused the content itself (ward's
// content-evidence mark) would refuse a retry identically, whatever the
// outcome. A failure whose cause lies in what the run produced or in its
// configuration would repeat. A transient, quota, credential, or capacity
// cause, an unknown one, and a loss or cancellation with no content evidence
// may succeed: either the condition can clear, or nothing shows it won't.
func DecideRetryOutlook(outcome ExecutionOutcomeStatus, cause FailureCause, check *HandoffCheckFailure) RetryOutlook {
	if check != nil && check.ContentEvidence {
		return RetryOutlookWouldRepeat
	}
	if outcome != ExecutionOutcomeFailed {
		return RetryOutlookMaySucceed
	}
	switch cause {
	case FailureCauseExportRejected, FailureCauseInvalidEvidence, FailureCauseInvalidConfiguration,
		FailureCauseContractContradiction, FailureCauseMemoryLimit:
		return RetryOutlookWouldRepeat
	case FailureCauseQuota, FailureCauseCredentialExpired, FailureCauseCredentialRevoked,
		FailureCauseServiceCapacity, FailureCauseProviderUnavailable, FailureCauseHandoffCheck,
		FailureCauseUnknown:
		return RetryOutlookMaySucceed
	}
	return RetryOutlookMaySucceed
}

// MaxExportRejectionFacts is how many gauntlet findings an execution-failure
// card carries; ExportRejectionFacts.Total counts the rest.
const MaxExportRejectionFacts = 5

// ExportRejectionFacts is the client-visible projection of an
// ExportRejection: the first findings' kinds and screened paths, and the
// total the gauntlet reported.
type ExportRejectionFacts struct {
	Findings []ExportRejectionFindingFacts `json:"findings"`
	Total    int                           `json:"total"`
}

// ExportRejectionFindingFacts is one finding's kind and the path it names.
// Path is screened (ScreenedName) and bounded; it is absent for a finding
// that names no representable path, such as an invalid path entry, whose
// raw name bytes stay daemon-internal.
type ExportRejectionFindingFacts struct {
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
}

// NewExportRejectionFacts projects a rejection record for a card. The engine
// that raises the card and the store gate that authenticates it both call it,
// so the two can't disagree. The record keeps its findings in the order the
// gauntlet reported them, fatal ones first.
func NewExportRejectionFacts(r ExportRejection) ExportRejectionFacts {
	shown := r.Findings[:min(len(r.Findings), MaxExportRejectionFacts)]
	findings := make([]ExportRejectionFindingFacts, 0, len(shown))
	for _, f := range shown {
		finding := ExportRejectionFindingFacts{Kind: strings.ToValidUTF8(f.Kind, "\uFFFD")}
		if f.Path != "" {
			finding.Path = screenedFindingPath(f.Path)
		}
		findings = append(findings, finding)
	}
	return ExportRejectionFacts{Findings: findings, Total: r.TotalFindings}
}

// screenedFindingPath bounds an agent-chosen path to the evidence cap and
// screens it. Screening follows the cut so a cut can't expose part of a
// credential-shaped name.
func screenedFindingPath(path string) string {
	path = strings.ToValidUTF8(path, "\uFFFD")
	if matchesCredentialRule(path) {
		return RedactedName(path)
	}
	if len(path) > MaxHandoffEvidenceBytes {
		path = cutHeadAndTail(path, MaxHandoffEvidenceBytes)
	}
	return ScreenedName(path)
}

func (f ExportRejectionFacts) Validate() error {
	if len(f.Findings) == 0 || len(f.Findings) > MaxExportRejectionFacts || f.Total < len(f.Findings) {
		return fmt.Errorf("export rejection facts with %d findings of %d: %w",
			len(f.Findings), f.Total, ErrCardFactInconsistent)
	}
	for index, finding := range f.Findings {
		if finding.Kind == "" || !utf8.ValidString(finding.Kind) || !utf8.ValidString(finding.Path) ||
			len(finding.Path) > MaxHandoffEvidenceBytes || matchesCredentialRule(finding.Path) {
			return fmt.Errorf("export rejection finding[%d]: %w", index, ErrCardFactInconsistent)
		}
	}
	return nil
}

// InterpretationFailureFacts identifies an invocation that completed and
// exported, and why the engine could not carry its output forward. It stands
// in for ExecutionFailureFacts on such an item, since the export, not an
// outcome, is that invocation's terminal record.
type InterpretationFailureFacts struct {
	Cause        InterpretationFailureCause `json:"cause"`
	Stage        StageName                  `json:"stage"`
	InvocationID InvocationID               `json:"invocation_id"`
}

func (f InterpretationFailureFacts) Validate() error {
	if !f.Cause.valid() || !f.Stage.valid() || f.InvocationID == "" {
		return fmt.Errorf("interpretation failure cause %q stage %q invocation %q: %w",
			f.Cause, f.Stage, f.InvocationID, ErrCardFactInconsistent)
	}
	return nil
}

// AgentQuestionFacts identifies the stage and invocation that stopped to ask,
// the blocker kind on the implementation side, and the decisions the human
// is asked to make. Kind is set exactly when Stage is implementation: the
// specifier's needs_decision output has no blocker taxonomy.
type AgentQuestionFacts struct {
	ScopeConflict *ScopeConflictFacts `json:"scope_conflict"`
	Stage         StageName           `json:"stage"`
	InvocationID  InvocationID        `json:"invocation_id"`
	Kind          *BlockedKind        `json:"kind"`
	Decisions     []Decision          `json:"decisions"`
	// PreviousAnswers are the owner's earlier answers to decisions that
	// match these, from earlier specification questions on the same
	// project and source (#3443), at most one per decision. They are a
	// default the owner confirms on the card, never an answer: the answer
	// command and ComputeDigest ignore them. omitempty keeps a question
	// without them at its stored bytes, and an app that predates the field
	// decodes it.
	PreviousAnswers []PreviousAnswer `json:"previous_answers,omitempty"`
}

// PreviousAnswer is an earlier answer carried onto one decision of a
// re-asked specification question. Exactly one of OptionLabel and Note is
// set: the exact label of the option the owner chose, which the decision
// still offers, or the text the owner typed when they answered in the note.
// ItemID is the earlier question item the answer came from.
type PreviousAnswer struct {
	DecisionIndex int     `json:"decision_index"`
	OptionLabel   *string `json:"option_label"`
	Note          *string `json:"note"`
	ItemID        ItemID  `json:"item_id"`
}

func (f AgentQuestionFacts) Validate() error {
	if f.ScopeConflict != nil {
		if f.Stage != StageNameImplementation || f.Kind == nil || *f.Kind != BlockedKindScopeExpansion || len(f.Decisions) != 1 {
			return ErrCardFactInconsistent
		}
		if err := f.ScopeConflict.Validate(); err != nil {
			return err
		}
	}
	if f.InvocationID == "" {
		return fmt.Errorf("agent question invocation: %w", ErrCardFactInconsistent)
	}
	switch f.Stage {
	case StageNameSpecification:
		if f.Kind != nil {
			return fmt.Errorf("agent question specification stage carries a blocked kind: %w",
				ErrCardFactInconsistent)
		}
	case StageNameImplementation:
		if f.Kind == nil || !f.Kind.valid() {
			return fmt.Errorf("agent question implementation stage kind: %w", ErrCardFactInconsistent)
		}
	default:
		return fmt.Errorf("agent question stage %q: %w", f.Stage, ErrCardFactInconsistent)
	}
	if err := ValidateDecisions(f.Decisions); err != nil {
		return fmt.Errorf("agent question: %w", err)
	}
	return f.validatePreviousAnswers()
}

// validatePreviousAnswers allows previous answers on a specification
// question only, one per decision, each naming an option that decision
// offers or carrying a non-blank note within the answer message's bound.
func (f AgentQuestionFacts) validatePreviousAnswers() error {
	if len(f.PreviousAnswers) == 0 {
		return nil
	}
	if f.Stage != StageNameSpecification {
		return fmt.Errorf("agent question previous answers on stage %q: %w", f.Stage, ErrCardFactInconsistent)
	}
	seen := make(map[int]bool, len(f.PreviousAnswers))
	for position, previous := range f.PreviousAnswers {
		index := previous.DecisionIndex
		if index < 0 || index >= len(f.Decisions) || seen[index] || previous.ItemID == "" {
			return fmt.Errorf("agent question previous_answers[%d] decision %d item %q: %w",
				position, index, previous.ItemID, ErrCardFactInconsistent)
		}
		seen[index] = true
		switch {
		case (previous.OptionLabel == nil) == (previous.Note == nil):
			return fmt.Errorf("agent question previous_answers[%d] sets %s: %w",
				position, "both or neither of option_label and note", ErrCardFactInconsistent)
		case previous.OptionLabel != nil:
			label := *previous.OptionLabel
			if !slices.ContainsFunc(f.Decisions[index].Options, func(option DecisionOption) bool { return option.Label == label }) {
				return fmt.Errorf("agent question previous_answers[%d] option_label for decision %d: %w",
					position, index, ErrCardFactInconsistent)
			}
		case strings.TrimSpace(*previous.Note) == "" || len(*previous.Note) > MaxQuestionAnswerNoteBytes:
			// The card offers the note again as the answer's note, so it
			// keeps the bound an answer's note has.
			return fmt.Errorf("agent question previous_answers[%d] note is blank or over %d bytes: %w",
				position, MaxQuestionAnswerNoteBytes, ErrCardFactInconsistent)
		}
	}
	return nil
}

func cloneAgentQuestionFacts(in *AgentQuestionFacts) *AgentQuestionFacts {
	if in == nil {
		return nil
	}
	out := *in
	out.Kind = clonePtr(in.Kind)
	out.ScopeConflict = cloneScopeConflictFacts(in.ScopeConflict)
	out.Decisions = cloneDecisions(in.Decisions)
	if in.PreviousAnswers != nil {
		out.PreviousAnswers = make([]PreviousAnswer, len(in.PreviousAnswers))
		for i, previous := range in.PreviousAnswers {
			previous.OptionLabel = clonePtr(previous.OptionLabel)
			previous.Note = clonePtr(previous.Note)
			out.PreviousAnswers[i] = previous
		}
	}
	return &out
}

// PublishBlockFacts carries exactly one of the normal hold vocabulary or the
// definitive trust-rule vocabulary used by publication failures.
type PublishBlockFacts struct {
	HoldReason *RunHoldReason `json:"hold_reason"`
	TrustRule  *TrustRule     `json:"trust_rule"`
}

func (f PublishBlockFacts) Validate() error {
	if (f.HoldReason == nil) == (f.TrustRule == nil) {
		return fmt.Errorf("publish block must carry exactly one variant: %w", ErrCardFactInconsistent)
	}
	if f.HoldReason != nil && !f.HoldReason.valid() {
		return fmt.Errorf("publish block hold reason %q: %w", *f.HoldReason, ErrCardFactInconsistent)
	}
	if f.TrustRule != nil && !f.TrustRule.valid() {
		return fmt.Errorf("publish block trust rule %q: %w", *f.TrustRule, ErrCardFactInconsistent)
	}
	return nil
}

// DiffStats is the daemon-derived prospective change summary bound to a base
// and head.
type DiffStats struct {
	FilesChanged int    `json:"files_changed"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	BaseSHA      string `json:"base_sha"`
	HeadSHA      string `json:"head_sha"`
}

func (s DiffStats) Validate() error {
	if s.FilesChanged < 0 || s.Additions < 0 || s.Deletions < 0 ||
		s.BaseSHA == "" || s.HeadSHA == "" {
		return fmt.Errorf("diff stats %d/%d/%d %q..%q: %w",
			s.FilesChanged, s.Additions, s.Deletions, s.BaseSHA, s.HeadSHA, ErrCardFactInconsistent)
	}
	return nil
}

// BlockedWait identifies what a blocked item awaited and when the wait began.
// It is retired with AttentionBlocked and stays for closed items.
type BlockedWait struct {
	Kind        BlockedWaitKind `json:"kind"`
	Since       time.Time       `json:"since"`
	ItemID      *ItemID         `json:"item_id"`
	PRReference *PRReference    `json:"pr_reference"`
}

func (w BlockedWait) Validate() error {
	if !w.Kind.valid() || w.Since.IsZero() || w.Since.Location() != time.UTC {
		return fmt.Errorf("blocked wait kind %q since %s: %w", w.Kind, w.Since, ErrCardFactInconsistent)
	}
	switch w.Kind {
	case BlockedWaitSpecApproval:
		if w.ItemID == nil || *w.ItemID == "" || w.PRReference != nil {
			return fmt.Errorf("blocked wait spec approval reference: %w", ErrCardFactInconsistent)
		}
	case BlockedWaitPRChecks, BlockedWaitExternalReview:
		if w.ItemID != nil || w.PRReference == nil {
			return fmt.Errorf("blocked wait pull request reference: %w", ErrCardFactInconsistent)
		}
		if err := w.PRReference.Validate(); err != nil {
			return fmt.Errorf("blocked wait: %w", err)
		}
	default:
		return fmt.Errorf("blocked wait kind %q: %w", w.Kind, ErrCardFactInconsistent)
	}
	return nil
}

// HealthDiagnostic is a daemon finding code and its current operational
// impact.
type HealthDiagnostic struct {
	Code    string             `json:"code"`
	Impairs ImpairedCapability `json:"impairs"`
}

func (d HealthDiagnostic) Validate() error {
	if !diagnosticCodePattern.MatchString(d.Code) || !d.Impairs.valid() {
		return fmt.Errorf("health diagnostic %q impairs %q: %w", d.Code, d.Impairs, ErrCardFactInconsistent)
	}
	return nil
}

// ReviewDisputeBinding binds the whole round in FindingIDs and its immutable
// completion evidence. Findings lists the rows the card shows. A card the
// per-finding path raises lists every adjudicated finding of the round, and a
// row with SettledRoute may go unanswered; a card written before that path
// listed only the findings a person must answer. The store requires prior
// adjudication entries for every finding the rows omit.
type ReviewDisputeBinding struct {
	RunID              RunID             `json:"run_id"`
	Round              int               `json:"round"`
	FindingIDs         []FindingID       `json:"finding_ids"`
	CompletionEvidence Digest            `json:"completion_evidence"`
	Findings           []DisputedFinding `json:"findings,omitempty"`
}

// DisputedFinding carries the authenticated finding and the routes the person
// can execute. External identity is attribution, never routing authority.
// Adjudication is the adjudicator's view of the finding, also never routing
// authority; it is nil when the round has no model-authored adjudication for
// the finding, and on a row stored before it existed (#2059).
//
// SettledRoute is the route the adjudication already settled for a finding
// that needs no person (SettledDisputeRouteIn). A person may leave the row
// unanswered, which keeps that route, or answer it with an offered route,
// which overrides it. A nil value means the row must be answered: a finding
// that waits for a person, a fallback card's row, or a row stored before the
// field existed. It may name a route the row does not offer.
type DisputedFinding struct {
	FindingID     FindingID                    `json:"finding_id"`
	Message       string                       `json:"message"`
	Location      *FindingLocation             `json:"location"`
	Severity      FindingSeverity              `json:"severity"`
	External      *DisputedFindingExternal     `json:"external"`
	OfferedRoutes []AdjudicationRoute          `json:"offered_routes"`
	Adjudication  *DisputedFindingAdjudication `json:"adjudication"`
	SettledRoute  *AdjudicationRoute           `json:"settled_route"`
}

// DisputedFindingAdjudication is the adjudicator's proposal for one disputed
// finding, shown so the person does not decide blind. Lean is the route the
// model proposed, nil when it proposed none (the `unknown` park). It may name
// a route the person is not offered, and nothing executes it: the person's
// choice still comes only from OfferedRoutes.
type DisputedFindingAdjudication struct {
	Lean           *AdjudicationRoute     `json:"lean"`
	Confidence     AdjudicationConfidence `json:"confidence"`
	Rationale      string                 `json:"rationale"`
	BelowThreshold bool                   `json:"below_threshold"`
}

type DisputedFindingExternal struct {
	ReviewerLogin string `json:"reviewer_login"`
	ThreadID      string `json:"thread_id"`
}

// SpecDiff is the daemon-computed line diff from the last reviewed
// specification to the revised body.
type SpecDiff struct {
	LinesAdded   int    `json:"lines_added"`
	LinesRemoved int    `json:"lines_removed"`
	Unified      string `json:"unified"`
	Truncated    bool   `json:"truncated"`
}

func (d SpecDiff) Validate() error {
	if d.LinesAdded < 0 || d.LinesRemoved < 0 || strings.TrimSpace(d.Unified) == "" ||
		d.Unified != strings.Trim(d.Unified, "\r\n") || !utf8.ValidString(d.Unified) ||
		len(d.Unified) > MaxClaimTextBytes {
		return fmt.Errorf("spec diff +%d/-%d: %w", d.LinesAdded, d.LinesRemoved, ErrCardFactInconsistent)
	}
	return nil
}

// DeriveSpecDiff returns the bounded, deterministic line diff used by both
// the specification producer and the store trust boundary. Keeping one pure
// derivation prevents a caller-supplied rendering or count from being
// presented as daemon-authenticated revision history.
func DeriveSpecDiff(before, after string) SpecDiff {
	// CRLF is a line separator, not part of the line content rendered in the
	// unified diff. The specification bodies and their digests remain unchanged.
	before = strings.ReplaceAll(before, "\r\n", "\n")
	after = strings.ReplaceAll(after, "\r\n", "\n")
	if before == after {
		return SpecDiff{Unified: "(no textual change)"}
	}
	beforeLines, afterLines := strings.Split(before, "\n"), strings.Split(after, "\n")
	ops := specLineDiffOperations(beforeLines, afterLines)
	added, removed := 0, 0
	for _, op := range ops {
		switch op.prefix {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	unified := formatSpecLineDiff(ops)
	truncated := len(unified) > MaxClaimTextBytes
	if truncated {
		const marker = "\n... diff truncated ..."
		limit := MaxClaimTextBytes - len(marker)
		for limit > 0 && !utf8.RuneStart(unified[limit]) {
			limit--
		}
		if newline := strings.LastIndexByte(unified[:limit], '\n'); newline >= 0 {
			limit = newline
		}
		unified = strings.TrimSuffix(unified[:limit], "\n") + marker
	}
	return SpecDiff{
		LinesAdded: added, LinesRemoved: removed,
		Unified: unified, Truncated: truncated,
	}
}

type specLineDiffOp struct {
	prefix byte
	line   string
}

func specLineDiffOperations(before, after []string) []specLineDiffOp {
	const maxLCSCells = 1_000_000
	if len(before) == 0 {
		return prefixedSpecLines('+', after)
	}
	if len(after) == 0 {
		return prefixedSpecLines('-', before)
	}
	if len(before) > maxLCSCells/len(after) {
		prefix := 0
		for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
			prefix++
		}
		suffix := 0
		for suffix < len(before)-prefix && suffix < len(after)-prefix &&
			before[len(before)-1-suffix] == after[len(after)-1-suffix] {
			suffix++
		}
		ops := make([]specLineDiffOp, 0, len(before)+len(after))
		for _, line := range before[:prefix] {
			ops = append(ops, specLineDiffOp{' ', line})
		}
		ops = append(ops, anchoredSpecLineDiffOperations(
			before[prefix:len(before)-suffix], after[prefix:len(after)-suffix],
		)...)
		for _, line := range before[len(before)-suffix:] {
			ops = append(ops, specLineDiffOp{' ', line})
		}
		return ops
	}
	width := len(after) + 1
	lcs := make([]int, (len(before)+1)*width)
	for i := len(before) - 1; i >= 0; i-- {
		for j := len(after) - 1; j >= 0; j-- {
			if before[i] == after[j] {
				lcs[i*width+j] = lcs[(i+1)*width+j+1] + 1
			} else {
				lcs[i*width+j] = max(lcs[(i+1)*width+j], lcs[i*width+j+1])
			}
		}
	}
	ops := make([]specLineDiffOp, 0, len(before)+len(after))
	for i, j := 0, 0; i < len(before) || j < len(after); {
		switch {
		case i < len(before) && j < len(after) && before[i] == after[j]:
			ops = append(ops, specLineDiffOp{' ', before[i]})
			i++
			j++
		case j < len(after) && (i == len(before) || lcs[i*width+j+1] >= lcs[(i+1)*width+j]):
			ops = append(ops, specLineDiffOp{'+', after[j]})
			j++
		default:
			ops = append(ops, specLineDiffOp{'-', before[i]})
			i++
		}
	}
	return ops
}

func anchoredSpecLineDiffOperations(before, after []string) []specLineDiffOp {
	if len(before) == 0 {
		return prefixedSpecLines('+', after)
	}
	if len(after) == 0 {
		return prefixedSpecLines('-', before)
	}
	anchors := orderedSpecLineAnchors(before, after)
	if len(anchors) == 0 {
		return append(prefixedSpecLines('-', before), prefixedSpecLines('+', after)...)
	}
	beforeCursor, afterCursor := 0, 0
	ops := make([]specLineDiffOp, 0, len(before)+len(after))
	for _, anchor := range anchors {
		ops = append(ops, specLineDiffOperations(
			before[beforeCursor:anchor.before], after[afterCursor:anchor.after],
		)...)
		ops = append(ops, specLineDiffOp{' ', before[anchor.before]})
		beforeCursor, afterCursor = anchor.before+1, anchor.after+1
	}
	return append(ops, specLineDiffOperations(before[beforeCursor:], after[afterCursor:])...)
}

func prefixedSpecLines(prefix byte, lines []string) []specLineDiffOp {
	ops := make([]specLineDiffOp, 0, len(lines))
	for _, line := range lines {
		ops = append(ops, specLineDiffOp{prefix, line})
	}
	return ops
}

type specLineAnchor struct {
	before int
	after  int
}

// orderedSpecLineAnchors is the bounded large-input path. It pairs matching
// line occurrences in order, then keeps their longest increasing subsequence.
// This preserves repeated unchanged runs as well as unique lines without
// allocating the quadratic LCS matrix.
func orderedSpecLineAnchors(before, after []string) []specLineAnchor {
	afterPositions := make(map[string][]int, len(after))
	for index, line := range after {
		afterPositions[line] = append(afterPositions[line], index)
	}
	beforeOccurrences := make(map[string]int, len(before))
	candidates := make([]specLineAnchor, 0, min(len(before), len(after)))
	for beforeIndex, line := range before {
		occurrence := beforeOccurrences[line]
		beforeOccurrences[line] = occurrence + 1
		positions := afterPositions[line]
		if occurrence < len(positions) {
			candidates = append(candidates, specLineAnchor{
				before: beforeIndex,
				after:  positions[occurrence],
			})
		}
	}
	if len(candidates) < 2 {
		return candidates
	}
	tails := make([]int, 0, len(candidates))
	tailIndexes := make([]int, 0, len(candidates))
	previous := make([]int, len(candidates))
	for index := range previous {
		previous[index] = -1
	}
	for index, candidate := range candidates {
		position, _ := slices.BinarySearch(tails, candidate.after)
		if position == len(tails) {
			tails = append(tails, candidate.after)
			tailIndexes = append(tailIndexes, index)
		} else {
			tails[position] = candidate.after
			tailIndexes[position] = index
		}
		if position > 0 {
			previous[index] = tailIndexes[position-1]
		}
	}
	anchors := make([]specLineAnchor, len(tails))
	for index, candidateIndex := len(anchors)-1, tailIndexes[len(tails)-1]; index >= 0; index-- {
		anchors[index] = candidates[candidateIndex]
		candidateIndex = previous[candidateIndex]
	}
	return anchors
}

func formatSpecLineDiff(ops []specLineDiffOp) string {
	const contextLines = 3
	changes := make([]int, 0)
	for index, op := range ops {
		if op.prefix != ' ' {
			changes = append(changes, index)
		}
	}
	var out strings.Builder
	for cursor := 0; cursor < len(changes); {
		startChange, endChange := changes[cursor], changes[cursor]
		for cursor+1 < len(changes) && changes[cursor+1]-endChange <= contextLines*2+1 {
			cursor++
			endChange = changes[cursor]
		}
		start, end := max(0, startChange-contextLines), min(len(ops), endChange+contextLines+1)
		oldStart, newStart := 1, 1
		for _, op := range ops[:start] {
			if op.prefix != '+' {
				oldStart++
			}
			if op.prefix != '-' {
				newStart++
			}
		}
		oldCount, newCount := 0, 0
		for _, op := range ops[start:end] {
			if op.prefix != '+' {
				oldCount++
			}
			if op.prefix != '-' {
				newCount++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, op := range ops[start:end] {
			fmt.Fprintf(&out, "%c%s\n", op.prefix, op.line)
		}
		cursor++
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// SpecRevisionComment is one daemon-authenticated request_changes comment.
type SpecRevisionComment struct {
	CommentID      string     `json:"comment_id"`
	ArtifactID     ArtifactID `json:"artifact_id"`
	Digest         Digest     `json:"digest"`
	RaisedOnItemID ItemID     `json:"raised_on_item_id"`
	Iteration      int        `json:"iteration"`
	Body           string     `json:"body"`
}

func (c SpecRevisionComment) validate(revisionIteration int) error {
	if c.CommentID == "" || c.CommentID != strings.TrimSpace(c.CommentID) ||
		!utf8.ValidString(c.CommentID) || c.ArtifactID == "" || c.Digest == "" ||
		c.RaisedOnItemID == "" || c.Iteration < 1 || c.Iteration >= revisionIteration ||
		c.ArtifactID != SpecificationFeedbackArtifactID(c.CommentID) ||
		c.Body == "" || c.Body != strings.TrimSpace(c.Body) || !utf8.ValidString(c.Body) ||
		len(c.Body) > MaxSpecRevisionCommentBytes ||
		Digest(contentaddr.Sum([]byte(c.Body))) != c.Digest {
		return fmt.Errorf("spec revision comment %q: %w", c.CommentID, ErrCardFactInconsistent)
	}
	return nil
}

// SpecAddressalClaim maps one comment id to the specifier's claimed response.
// It is an agent claim, not proof that the revision addressed the comment.
type SpecAddressalClaim struct {
	CommentID string `json:"comment_id"`
	Response  string `json:"response"`
}

func (a SpecAddressalClaim) validate() error {
	if a.CommentID == "" || a.CommentID != strings.TrimSpace(a.CommentID) ||
		a.Response == "" || a.Response != strings.TrimSpace(a.Response) ||
		!utf8.ValidString(a.CommentID) || !utf8.ValidString(a.Response) ||
		len(a.Response) > MaxSpecRevisionCommentBytes {
		return fmt.Errorf("spec addressal %q: %w", a.CommentID, ErrCardFactInconsistent)
	}
	return nil
}

// SpecRevisionFacts carries the daemon-derived history needed to review a
// superseding specification. ClaimedAddressals is a typed projection of the
// digest-bound Addressals agent claim.
type SpecRevisionFacts struct {
	Iteration           int                   `json:"iteration"`
	PriorItemID         ItemID                `json:"prior_item_id"`
	PriorSpecArtifactID ArtifactID            `json:"prior_spec_artifact_id"`
	PriorSpecDigest     Digest                `json:"prior_spec_digest"`
	Diff                SpecDiff              `json:"diff"`
	PriorComments       []SpecRevisionComment `json:"prior_comments"`
	ClaimedAddressals   []SpecAddressalClaim  `json:"claimed_addressals"`
	AddressalsDigest    Digest                `json:"addressals_digest"`
}

func (f SpecRevisionFacts) Validate() error {
	if f.Iteration < 2 || f.PriorItemID == "" || f.PriorSpecArtifactID == "" ||
		f.PriorSpecDigest == "" || f.PriorComments == nil ||
		len(f.PriorComments) == 0 || len(f.PriorComments) > MaxSpecRevisionComments ||
		f.ClaimedAddressals == nil || len(f.ClaimedAddressals) > MaxSpecRevisionComments ||
		f.AddressalsDigest == "" {
		return fmt.Errorf("spec revision iteration %d: %w", f.Iteration, ErrCardFactInconsistent)
	}
	if err := f.Diff.Validate(); err != nil {
		return err
	}
	comments := make(map[string]struct{}, len(f.PriorComments))
	artifacts := make(map[ArtifactID]struct{}, len(f.PriorComments))
	previousIteration := 0
	for _, comment := range f.PriorComments {
		if err := comment.validate(f.Iteration); err != nil {
			return err
		}
		if _, duplicate := comments[comment.CommentID]; duplicate {
			return fmt.Errorf("duplicate spec revision comment %q: %w", comment.CommentID, ErrCardFactInconsistent)
		}
		if _, duplicate := artifacts[comment.ArtifactID]; duplicate {
			return fmt.Errorf("duplicate spec revision artifact %q: %w", comment.ArtifactID, ErrCardFactInconsistent)
		}
		if comment.Iteration <= previousIteration {
			return fmt.Errorf("spec revision comment %q is out of order: %w",
				comment.CommentID, ErrCardFactInconsistent)
		}
		comments[comment.CommentID] = struct{}{}
		artifacts[comment.ArtifactID] = struct{}{}
		previousIteration = comment.Iteration
	}
	if f.PriorComments[len(f.PriorComments)-1].RaisedOnItemID != f.PriorItemID {
		return fmt.Errorf("spec revision prior item does not match latest comment: %w", ErrCardFactInconsistent)
	}
	addressed := make(map[string]struct{}, len(f.ClaimedAddressals))
	for _, addressal := range f.ClaimedAddressals {
		if err := addressal.validate(); err != nil {
			return err
		}
		if _, ok := comments[addressal.CommentID]; !ok {
			return fmt.Errorf("spec addressal names unknown comment %q: %w", addressal.CommentID, ErrCardFactInconsistent)
		}
		if _, duplicate := addressed[addressal.CommentID]; duplicate {
			return fmt.Errorf("duplicate spec addressal %q: %w", addressal.CommentID, ErrCardFactInconsistent)
		}
		addressed[addressal.CommentID] = struct{}{}
	}
	body, err := json.Marshal(f.ClaimedAddressals)
	if err != nil {
		return fmt.Errorf("encode spec addressals: %w", err)
	}
	if Digest(contentaddr.Sum(body)) != f.AddressalsDigest {
		return fmt.Errorf("spec addressals digest mismatch: %w", ErrCardFactInconsistent)
	}
	return nil
}

func (b ReviewDisputeBinding) Validate() error {
	if b.RunID == "" || b.Round < 1 || len(b.FindingIDs) == 0 || b.CompletionEvidence == "" {
		return fmt.Errorf("review dispute binding %s round %d: %w", b.RunID, b.Round, ErrCardFactInconsistent)
	}
	seen := make(map[FindingID]struct{}, len(b.FindingIDs))
	for _, id := range b.FindingIDs {
		if id == "" {
			return fmt.Errorf("review dispute binding empty finding id: %w", ErrCardFactInconsistent)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("review dispute binding duplicate finding %q: %w", id, ErrCardFactInconsistent)
		}
		seen[id] = struct{}{}
	}
	rows := make(map[FindingID]bool, len(b.Findings))
	mustAnswer := false
	for _, finding := range b.Findings {
		if _, ok := seen[finding.FindingID]; !ok || rows[finding.FindingID] ||
			finding.Message == "" || !utf8.ValidString(finding.Message) || len(finding.OfferedRoutes) == 0 {
			return ErrCardFactInconsistent
		}
		rows[finding.FindingID] = true
		if finding.Location != nil {
			if err := finding.Location.Validate(); err != nil {
				return err
			}
		}
		if finding.Severity != "" && !finding.Severity.valid() {
			return ErrCardFactInconsistent
		}
		if finding.External != nil && (finding.External.ReviewerLogin == "" || finding.External.ThreadID == "") {
			return ErrCardFactInconsistent
		}
		routes := make(map[AdjudicationRoute]bool)
		for _, route := range finding.OfferedRoutes {
			if (route != RouteRemediate && route != RouteDecline && route != RouteDefer) || routes[route] {
				return ErrCardFactInconsistent
			}
			routes[route] = true
		}
		if block := finding.Adjudication; block != nil {
			if !block.Confidence.valid() || strings.TrimSpace(block.Rationale) == "" ||
				!utf8.ValidString(block.Rationale) || (block.Lean != nil && !block.Lean.valid()) {
				return ErrCardFactInconsistent
			}
		}
		if settled := finding.SettledRoute; settled == nil {
			mustAnswer = true
		} else if *settled != RouteRemediate && *settled != RouteDecline && *settled != RouteDefer {
			return ErrCardFactInconsistent
		}
	}
	// A card is raised because some finding waits for a person, so rows that
	// are all settled describe no decision.
	if len(b.Findings) > 0 && !mustAnswer {
		return ErrCardFactInconsistent
	}
	return nil
}

func clonePublishBlockFacts(in *PublishBlockFacts) *PublishBlockFacts {
	if in == nil {
		return nil
	}
	out := *in
	if in.HoldReason != nil {
		v := *in.HoldReason
		out.HoldReason = &v
	}
	if in.TrustRule != nil {
		v := *in.TrustRule
		out.TrustRule = &v
	}
	return &out
}

func cloneBlockedWait(in *BlockedWait) *BlockedWait {
	if in == nil {
		return nil
	}
	out := *in
	if in.ItemID != nil {
		v := *in.ItemID
		out.ItemID = &v
	}
	if in.PRReference != nil {
		v := *in.PRReference
		out.PRReference = &v
	}
	return &out
}

func cloneReviewDisputeBinding(in *ReviewDisputeBinding) *ReviewDisputeBinding {
	if in == nil {
		return nil
	}
	out := *in
	out.FindingIDs = append([]FindingID(nil), in.FindingIDs...)
	out.Findings = slices.Clone(in.Findings)
	for index := range out.Findings {
		row := &out.Findings[index]
		row.OfferedRoutes = slices.Clone(row.OfferedRoutes)
		if row.Location != nil {
			value := *row.Location
			row.Location = &value
		}
		if row.External != nil {
			value := *row.External
			row.External = &value
		}
		if row.Adjudication != nil {
			value := *row.Adjudication
			if value.Lean != nil {
				lean := *value.Lean
				value.Lean = &lean
			}
			row.Adjudication = &value
		}
		if row.SettledRoute != nil {
			value := *row.SettledRoute
			row.SettledRoute = &value
		}
	}
	return &out
}

func cloneSpecRevisionFacts(in *SpecRevisionFacts) *SpecRevisionFacts {
	if in == nil {
		return nil
	}
	out := *in
	out.PriorComments = append([]SpecRevisionComment(nil), in.PriorComments...)
	out.ClaimedAddressals = append([]SpecAddressalClaim(nil), in.ClaimedAddressals...)
	return &out
}
