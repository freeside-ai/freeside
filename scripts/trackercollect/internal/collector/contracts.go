package collector

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ContractsSchemaVersion = 1
	// DefaultContractCap is the standing limit on concurrent contract
	// implementations (AGENTS.md, Contract Changes).
	DefaultContractCap = 2

	contractLabel       = "kind:contract"
	contractActiveLabel = "coord:contract-active"
	deferralLabel       = "deferral"
	trackerLabel        = "tracker"

	// leaseDuration bounds both a claim comment's lease and a planning
	// reservation (docs/coordination.md, Claiming and Stages).
	leaseDuration = 48 * time.Hour
)

// Contract unit states, in the order deriveContractState prefers them.
const (
	ContractActivePR       = "active-pr"
	ContractActiveLease    = "active-lease"
	ContractActivePRLegacy = "active-pr-legacy"
	ContractReserved       = "reserved"
	ContractDormant        = "dormant"
	ContractExpired        = "expired"
	ContractIdle           = "idle"
)

// Claim comment statuses.
const (
	ClaimPRBacked = "pr-backed"
	ClaimLive     = "live"
	ClaimExpired  = "expired"
	ClaimReleased = "released"
)

const (
	FindingMissingLabel = "missing-label"
	FindingStaleLabel   = "stale-label"
)

type ContractsConfig struct {
	Repository Repository
	OutputDir  string
	Cap        int
	PageSize   int
	PageCap    int
}

type ContractsSnapshot struct {
	SchemaVersion int       `json:"schema_version"`
	Repository    string    `json:"repository"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at"`
	Cap           int       `json:"cap"`
	// Complete is false when any ambiguity exists. The units and findings
	// then cover retained evidence only and support no verdict.
	Complete bool `json:"complete"`
	// ActiveCount and ReservedCount are null when the evidence is incomplete.
	ActiveCount   *int              `json:"active_count"`
	ReservedCount *int              `json:"reserved_count"`
	Contracts     []ContractUnit    `json:"contracts"`
	Findings      []ContractFinding `json:"findings"`
	Ambiguities   []Ambiguity       `json:"ambiguities"`
}

type ContractUnit struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Labeled   bool   `json:"contract_active_label"`
	Deferral  bool   `json:"deferral"`
	Milestone string `json:"milestone"`
	// Trackers are the open trackers whose Units section lists the issue.
	Trackers []int `json:"trackers"`
	// OrderingClaim is the claim comment whose created_at and ID order an
	// active-lease or active-pr unit in arbitration.
	OrderingClaim *ContractClaim  `json:"ordering_claim"`
	Claims        []ContractClaim `json:"claims"`
	// LegacyPullRequests are the open PRs whose closing issues include the
	// unit while no claim comment keeps it active.
	LegacyPullRequests []int                 `json:"legacy_pull_requests"`
	Reservations       []ContractReservation `json:"reservations"`
	Stamp              ForgeStamp            `json:"stamp"`
}

type ContractClaim struct {
	CommentID int64  `json:"comment_id"`
	CreatedAt string `json:"created_at"`
	Branch    string `json:"branch"`
	Status    string `json:"status"`
	// ClosingPullRequests are the open canonical-repository PRs from the
	// claim's branch whose closing issues include the unit.
	ClosingPullRequests []int `json:"closing_pull_requests"`
	// OtherPullRequests are open canonical-repository PRs from the branch
	// that lack the close keyword. They are named and keep no lease alive.
	OtherPullRequests []int `json:"other_pull_requests"`
}

type ContractReservation struct {
	CommentID int64  `json:"comment_id"`
	CreatedAt string `json:"created_at"`
	Active    bool   `json:"active"`
}

type ContractFinding struct {
	Code   string `json:"code"`
	Number int    `json:"number"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

func contractStateIsActive(state string) bool {
	return state == ContractActivePR || state == ContractActiveLease || state == ContractActivePRLegacy
}

// RunContracts collects the contract occupancy report and writes its
// artifacts. It returns 0 for complete evidence with no findings, 3 for
// complete evidence with findings, 2 when any ambiguity exists (artifacts are
// still written), and 1 with an error for a hard failure.
func RunContracts(ctx context.Context, config ContractsConfig, runner QueryRunner, clock func() time.Time) (int, error) {
	snapshot, err := CollectContracts(ctx, config, runner, clock)
	if err != nil {
		return 1, err
	}
	if err := WriteContractsArtifacts(config.OutputDir, snapshot); err != nil {
		return 1, err
	}
	if len(snapshot.Ambiguities) > 0 {
		return 2, nil
	}
	if len(snapshot.Findings) > 0 {
		return 3, nil
	}
	return 0, nil
}

// CollectContracts reads every open kind:contract issue and derives its claim
// state, the open trackers listing it, and whether the occupancy label
// matches. It never writes to the forge.
func CollectContracts(ctx context.Context, config ContractsConfig, runner QueryRunner, clock func() time.Time) (ContractsSnapshot, error) {
	if config.Cap <= 0 {
		config.Cap = DefaultContractCap
	}
	if config.PageSize <= 0 {
		config.PageSize = defaultPageSize
	}
	if config.PageCap <= 0 {
		config.PageCap = defaultPageCap
	}
	c := &collector{
		config: Config{Repository: config.Repository, PageSize: config.PageSize, PageCap: config.PageCap},
		runner: runner,
	}
	// Leases are judged against the clock at collection start, so one run
	// reads every unit at the same instant.
	startedAt := clock().UTC()

	openIssues, err := c.fetchOpenIssues(ctx)
	if err != nil {
		return ContractsSnapshot{}, err
	}
	openPullRequests, err := c.fetchOpenPullRequests(ctx)
	if err != nil {
		return ContractsSnapshot{}, err
	}
	for i := range openPullRequests {
		linked, err := c.fetchPullRequestClosingIssues(ctx, openPullRequests[i].Number)
		if err != nil {
			return ContractsSnapshot{}, err
		}
		openPullRequests[i].LinkedIssues = linked
	}
	// The shared fetchers flag duplicate Scope lines and sections for the
	// merged-PR report. This report reads no scope, so they are not its
	// ambiguities.
	c.dropAmbiguities("parse-collision")

	var contracts []graphIssue
	contractNumbers := make(map[int]bool)
	for _, issue := range openIssues {
		if *issue.Labels.PageInfo.HasNextPage {
			c.ambiguous("label-truncation", fmt.Sprintf("issue #%d labels", issue.Number), "more labels exist than one page holds; its classification is unknown")
		}
		if issue.hasLabel(contractLabel) {
			contracts = append(contracts, issue)
			contractNumbers[issue.Number] = true
		}
	}
	listedBy := c.contractTrackers(openIssues)

	markers, err := c.fetchMarkerComments(ctx, contractNumbers, openPullRequests)
	if err != nil {
		return ContractsSnapshot{}, err
	}
	markersByIssue := make(map[int][]MarkerComment)
	for _, marker := range markers {
		// Anyone may comment on a public repository's issues, and the claim
		// protocol trusts collaborator comments only (docs/coordination.md,
		// Claiming). The protocol gives an outsider's marker no stated
		// standing, so it is neither counted nor dismissed: counting it
		// could fill a slot or hide a live claim behind a forged release.
		if !trustedMarkerAuthor(marker.AuthorAssociation) {
			c.ambiguous("untrusted-marker-author", fmt.Sprintf("comment %d", marker.Stamp.DatabaseID),
				fmt.Sprintf("%s marker on issue #%d has author association %s, not OWNER, MEMBER, or COLLABORATOR; it is not counted", marker.Kind, marker.IssueNumber, marker.AuthorAssociation))
			continue
		}
		markersByIssue[marker.IssueNumber] = append(markersByIssue[marker.IssueNumber], marker)
	}

	snapshot := ContractsSnapshot{
		SchemaVersion: ContractsSchemaVersion,
		Repository:    config.Repository.String(),
		StartedAt:     startedAt,
		Cap:           config.Cap,
		Contracts:     []ContractUnit{},
		Findings:      []ContractFinding{},
	}
	active, reserved := 0, 0
	for _, issue := range contracts {
		unit, err := deriveContractState(issue, markersByIssue[issue.Number], openPullRequests, startedAt)
		if err != nil {
			return ContractsSnapshot{}, err
		}
		unit.Trackers = append([]int{}, listedBy[issue.Number]...)
		snapshot.Contracts = append(snapshot.Contracts, unit)
		switch {
		case contractStateIsActive(unit.State):
			active++
			if !unit.Labeled {
				snapshot.Findings = append(snapshot.Findings, ContractFinding{
					Code: FindingMissingLabel, Number: unit.Number, State: unit.State,
					Detail: fmt.Sprintf("holds an active claim without the %s label", contractActiveLabel),
				})
			}
		case unit.Labeled:
			snapshot.Findings = append(snapshot.Findings, ContractFinding{
				Code: FindingStaleLabel, Number: unit.Number, State: unit.State,
				Detail: fmt.Sprintf("carries the %s label without an active claim", contractActiveLabel),
			})
		}
		if unit.State == ContractReserved {
			reserved++
		}
	}
	sort.Slice(snapshot.Contracts, func(i, j int) bool { return snapshot.Contracts[i].Number < snapshot.Contracts[j].Number })
	sort.Slice(snapshot.Findings, func(i, j int) bool { return snapshot.Findings[i].Number < snapshot.Findings[j].Number })
	sort.Slice(c.ambiguities, func(i, j int) bool {
		if c.ambiguities[i].Code != c.ambiguities[j].Code {
			return c.ambiguities[i].Code < c.ambiguities[j].Code
		}
		if c.ambiguities[i].Subject != c.ambiguities[j].Subject {
			return c.ambiguities[i].Subject < c.ambiguities[j].Subject
		}
		return c.ambiguities[i].Detail < c.ambiguities[j].Detail
	})
	snapshot.Ambiguities = append([]Ambiguity{}, c.ambiguities...)
	snapshot.Complete = len(snapshot.Ambiguities) == 0
	if snapshot.Complete {
		snapshot.ActiveCount, snapshot.ReservedCount = &active, &reserved
	}
	snapshot.CompletedAt = clock().UTC()
	return snapshot, nil
}

// trustedMarkerAuthor reports whether the forge places a comment's author
// inside the repository's trust boundary. Every other association, including
// one this tool does not know, is outside it.
func trustedMarkerAuthor(association string) bool {
	switch association {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}

func (c *collector) dropAmbiguities(code string) {
	kept := c.ambiguities[:0]
	for _, ambiguity := range c.ambiguities {
		if ambiguity.Code != code {
			kept = append(kept, ambiguity)
		}
	}
	c.ambiguities = kept
}

// contractTrackers maps a unit number to the open trackers whose Units section
// lists it. Membership comes from Units alone, as in buildContainingTrackers:
// Exit and Notes may cite a unit without listing it. A tracker whose listing
// cannot be read is ambiguous, because any unit might be on it.
func (c *collector) contractTrackers(openIssues []graphIssue) map[int][]int {
	listedBy := make(map[int][]int)
	for _, issue := range openIssues {
		if !issue.hasLabel(trackerLabel) {
			continue
		}
		subject := fmt.Sprintf("issue #%d", issue.Number)
		units := extractSections(issue.Body, "Units")
		if len(units) != 1 {
			c.ambiguous("tracker-structure", subject, fmt.Sprintf("expected exactly one Units section, found %d", len(units)))
			continue
		}
		entries, invalidNumbers := parseCheckboxEntries(units[0])
		if len(invalidNumbers) > 0 {
			c.ambiguous("malformed-tracker-entry", subject, fmt.Sprintf("invalid issue numbers: %s", strings.Join(invalidNumbers, ", ")))
			continue
		}
		seen := make(map[int]bool)
		for _, entry := range entries {
			if !seen[entry.UnitNumber] {
				seen[entry.UnitNumber] = true
				listedBy[entry.UnitNumber] = append(listedBy[entry.UnitNumber], issue.Number)
			}
		}
	}
	for number := range listedBy {
		sort.Ints(listedBy[number])
	}
	return listedBy
}

// deriveContractState applies the claim rule in docs/coordination.md to one
// contract issue. markers are that issue's marker comments, so a release
// posted on another issue never reaches this unit. Malformed markers carry no
// branch, claim ID, or plan number and are skipped; the collector has already
// recorded each as an ambiguity.
//
// A claim is released only by a release comment naming its comment ID. An
// unreleased claim is PR-backed, at any age, when an open canonical-repository
// PR from its branch has the unit among its closing issues; otherwise it is
// live for 48 hours from its creation and expired from the 48th hour on.
func deriveContractState(issue graphIssue, markers []MarkerComment, openPullRequests []OpenPullRequest, now time.Time) (ContractUnit, error) {
	unit := ContractUnit{
		Number:             issue.Number,
		Title:              issue.Title,
		Labeled:            issue.hasLabel(contractActiveLabel),
		Deferral:           issue.hasLabel(deferralLabel),
		Trackers:           []int{},
		Claims:             []ContractClaim{},
		LegacyPullRequests: []int{},
		Reservations:       []ContractReservation{},
		Stamp:              issueStamp(issue),
	}
	if issue.Milestone != nil {
		unit.Milestone = issue.Milestone.Title
	}

	closesUnit := make(map[int]bool)
	for _, pr := range openPullRequests {
		if pullRequestClosesIssue(pr, issue) {
			closesUnit[pr.Number] = true
		}
	}
	released := make(map[int64]bool)
	for _, marker := range markers {
		if marker.Kind == "release" && marker.ReleasesClaimID > 0 {
			released[marker.ReleasesClaimID] = true
		}
	}

	backing := make(map[int]bool)
	reserved := false
	for _, marker := range markers {
		switch marker.Kind {
		case "claim":
			if marker.Branch == "" {
				continue
			}
			live, err := withinLease(marker, now)
			if err != nil {
				return ContractUnit{}, err
			}
			claim := ContractClaim{
				CommentID: marker.Stamp.DatabaseID, CreatedAt: marker.CreatedAt, Branch: marker.Branch,
				ClosingPullRequests: []int{}, OtherPullRequests: []int{},
			}
			// MatchedOpenPullRequests are the open canonical-repository
			// PRs from the claim's branch.
			for _, number := range marker.MatchedOpenPullRequests {
				if closesUnit[number] {
					claim.ClosingPullRequests = append(claim.ClosingPullRequests, number)
				} else {
					claim.OtherPullRequests = append(claim.OtherPullRequests, number)
				}
			}
			switch {
			case released[claim.CommentID]:
				claim.Status = ClaimReleased
			case len(claim.ClosingPullRequests) > 0:
				claim.Status = ClaimPRBacked
				for _, number := range claim.ClosingPullRequests {
					backing[number] = true
				}
			case live:
				claim.Status = ClaimLive
			default:
				claim.Status = ClaimExpired
			}
			unit.Claims = append(unit.Claims, claim)
		case "planning-reservation":
			if marker.PlanIssueNumber != issue.Number {
				continue
			}
			live, err := withinLease(marker, now)
			if err != nil {
				return ContractUnit{}, err
			}
			unit.Reservations = append(unit.Reservations, ContractReservation{
				CommentID: marker.Stamp.DatabaseID, CreatedAt: marker.CreatedAt, Active: live,
			})
			reserved = reserved || live
		}
	}
	sort.Slice(unit.Claims, func(i, j int) bool {
		if unit.Claims[i].CreatedAt != unit.Claims[j].CreatedAt {
			return unit.Claims[i].CreatedAt < unit.Claims[j].CreatedAt
		}
		return unit.Claims[i].CommentID < unit.Claims[j].CommentID
	})
	for number := range closesUnit {
		if !backing[number] {
			unit.LegacyPullRequests = append(unit.LegacyPullRequests, number)
		}
	}
	sort.Ints(unit.LegacyPullRequests)

	// Claims are sorted by created_at, then comment ID, the key arbitration
	// uses, so the first active claim is the one that orders the unit even
	// when a later claim is the PR-backed one.
	prBacked, expired := false, false
	for _, claim := range unit.Claims {
		active := claim.Status == ClaimPRBacked || claim.Status == ClaimLive
		if active && unit.OrderingClaim == nil {
			ordering := claim
			unit.OrderingClaim = &ordering
		}
		prBacked = prBacked || claim.Status == ClaimPRBacked
		expired = expired || claim.Status == ClaimExpired
	}
	switch {
	case prBacked:
		unit.State = ContractActivePR
	case unit.OrderingClaim != nil:
		unit.State = ContractActiveLease
	case len(unit.LegacyPullRequests) > 0:
		unit.State = ContractActivePRLegacy
	case reserved:
		unit.State = ContractReserved
	case unit.Deferral && unit.Milestone == "":
		unit.State = ContractDormant
	case expired:
		unit.State = ContractExpired
	default:
		unit.State = ContractIdle
	}
	return unit, nil
}

// closeKeywordPattern matches GitHub's closing keywords before a same-repository
// issue reference.
var closeKeywordPattern = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?[ \t]+#([0-9]+)\b`)

// pullRequestClosesIssue reports whether an open PR carries the issue's close
// keyword. The forge's closing-issue list is matched by node ID, since a PR may
// close a same-numbered issue in another repository. That list is empty for a
// PR based on a branch other than the default one, so a stacked PR is read by
// the keyword in its body instead. A keyword quoted in a code block or code
// span is text about the keyword, as it is to the forge, and closes nothing.
func pullRequestClosesIssue(pr OpenPullRequest, issue graphIssue) bool {
	for _, linked := range pr.LinkedIssues {
		if linked.Stamp.NodeID == issue.ID {
			return true
		}
	}
	for _, match := range closeKeywordPattern.FindAllStringSubmatch(maskInlineCode(markdownEvidence(pr.Body)), -1) {
		if match[1] == strconv.Itoa(issue.Number) {
			return true
		}
	}
	return false
}

// maskInlineCode blanks each code span that opens and closes on one line: a
// run of backticks up to the next run of the same length. A span that crosses
// a line break is left as it is, so a stray backtick on another line can
// never hide a close keyword; the cost is reading a keyword inside such a
// span.
func maskInlineCode(text string) string {
	var result strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		for index := 0; index < len(line); {
			if line[index] == '\\' && index+1 < len(line) {
				// A backslash escapes the next character outside a code
				// span, so an escaped backtick opens nothing.
				result.WriteString(line[index : index+2])
				index += 2
				continue
			}
			if line[index] != '`' {
				result.WriteByte(line[index])
				index++
				continue
			}
			length := backtickRun(line, index)
			end := closingBacktickRun(line, index+length, length)
			if end < 0 {
				result.WriteString(line[index : index+length])
				index += length
				continue
			}
			result.WriteString(strings.Repeat(" ", end-index))
			index = end
		}
	}
	return result.String()
}

func backtickRun(line string, start int) int {
	length := 0
	for start+length < len(line) && line[start+length] == '`' {
		length++
	}
	return length
}

// closingBacktickRun returns the index just past the next run of exactly
// length backticks at or after start, or -1 when the line holds none.
func closingBacktickRun(line string, start, length int) int {
	for index := start; index < len(line); {
		if line[index] != '`' {
			index++
			continue
		}
		run := backtickRun(line, index)
		if run == length {
			return index + run
		}
		index += run
	}
	return -1
}

func withinLease(marker MarkerComment, now time.Time) (bool, error) {
	created, err := time.Parse(time.RFC3339, marker.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("issue #%d comment %d has malformed createdAt %q", marker.IssueNumber, marker.Stamp.DatabaseID, marker.CreatedAt)
	}
	return now.Sub(created) < leaseDuration, nil
}
