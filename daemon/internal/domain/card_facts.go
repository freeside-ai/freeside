package domain

import (
	"fmt"
	"regexp"
	"time"
)

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
	Project  DisplayName `json:"project"`
	WorkUnit DisplayName `json:"work_unit"`
}

func (n DisplayNames) Validate() error {
	if err := n.Project.Validate(); err != nil {
		return fmt.Errorf("project: %w", err)
	}
	if err := n.WorkUnit.Validate(); err != nil {
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

// ExecutionFailureFacts identifies the failed terminal outcome and the stage
// and invocation that produced it.
type ExecutionFailureFacts struct {
	Outcome      ExecutionOutcomeStatus `json:"outcome"`
	Stage        StageName              `json:"stage"`
	InvocationID InvocationID           `json:"invocation_id"`
}

func (f ExecutionFailureFacts) Validate() error {
	if !f.Outcome.valid() || !f.Stage.valid() || f.InvocationID == "" {
		return fmt.Errorf("execution failure outcome %q stage %q invocation %q: %w",
			f.Outcome, f.Stage, f.InvocationID, ErrCardFactInconsistent)
	}
	return nil
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

// BlockedWait identifies what a blocked item awaits and when the wait began.
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

// ReviewDisputeBinding identifies the disputed findings and the immutable
// completion evidence for the bound review round.
type ReviewDisputeBinding struct {
	RunID              RunID       `json:"run_id"`
	Round              int         `json:"round"`
	FindingIDs         []FindingID `json:"finding_ids"`
	CompletionEvidence Digest      `json:"completion_evidence"`
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
	return &out
}
