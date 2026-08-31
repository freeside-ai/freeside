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
// the elaboration producer and the store trust boundary. Keeping one pure
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
		c.ArtifactID != ArtifactID("spec-feedback-"+c.CommentID) ||
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

func cloneSpecRevisionFacts(in *SpecRevisionFacts) *SpecRevisionFacts {
	if in == nil {
		return nil
	}
	out := *in
	out.PriorComments = append([]SpecRevisionComment(nil), in.PriorComments...)
	out.ClaimedAddressals = append([]SpecAddressalClaim(nil), in.ClaimedAddressals...)
	return &out
}
