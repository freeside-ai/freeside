package collector

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	UnitSchemaVersion = 1

	// MemberUnknown is the state of a set member whose claim state was not
	// established. The claim-state resolver never returns it.
	MemberUnknown = "UNKNOWN"
	// MemberNotOpen is the state of a declared partner absent from a complete
	// open-issue inventory: a closed issue, or a number that is not an issue.
	// Its comments are not read.
	MemberNotOpen = "not-open"
)

// How a member joins the direct exclusivity set.
const (
	RelationAssigned = "assigned"
	// RelationForward: the assigned issue declares exclusive-with the member.
	RelationForward = "forward"
	// RelationReverse: the member declares exclusive-with the assigned issue.
	RelationReverse = "reverse"
)

const readFailureCode = "read-failure"

type UnitConfig struct {
	Repository Repository
	Issue      int
	OutputDir  string
	PageSize   int
	PageCap    int
}

type UnitSnapshot struct {
	SchemaVersion int       `json:"schema_version"`
	Repository    string    `json:"repository"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at"`
	Issue         int       `json:"issue"`
	Title         string    `json:"title"`
	// Complete is false when any UNKNOWN entry exists. The members, trackers,
	// and reverse declarations then cover retained evidence only.
	Complete bool `json:"complete"`
	// Dependencies are the assigned issue's lines that lead with a typed
	// relationship or none. Every other line is an UnknownRelationship.
	Dependencies         []UnitDependency      `json:"dependencies"`
	UnknownRelationships []UnknownRelationship `json:"unknown_relationships"`
	// Members are the assigned issue and every unit directly related to it by
	// a forward or reverse exclusive-with declaration.
	Members  []UnitMember  `json:"members"`
	Trackers []UnitTracker `json:"trackers"`
	Unknowns []Ambiguity   `json:"unknowns"`
}

type UnitDependency struct {
	Kind         string             `json:"kind"`
	Relationship string             `json:"relationship"`
	Targets      []DependencyTarget `json:"targets"`
	Line         string             `json:"line"`
}

// UnknownRelationship is a Dependencies line the report could not read as a
// typed relationship, on the assigned issue or on another open issue whose
// line may concern it. Line is empty when the finding is about the section.
type UnknownRelationship struct {
	Issue  int    `json:"issue"`
	Line   string `json:"line"`
	Reason string `json:"reason"`
}

// UnitMember carries the claim-state resolver's result for one member. With
// State UNKNOWN its claims, PRs, and reservations are retained evidence only.
type UnitMember struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Relations []string `json:"relations"`
	State     string   `json:"state"`
	Milestone string   `json:"milestone"`
	Deferral  bool     `json:"deferral"`
	// OrderingClaim is null for an UNKNOWN member: incomplete evidence
	// establishes no arbitration key.
	OrderingClaim      *ContractClaim        `json:"ordering_claim"`
	Claims             []ContractClaim       `json:"claims"`
	LegacyPullRequests []int                 `json:"legacy_pull_requests"`
	Reservations       []ContractReservation `json:"reservations"`
	// Stamp is null for a member that is not in the open-issue inventory.
	Stamp *ForgeStamp `json:"stamp"`
}

type UnitTracker struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Milestone string `json:"milestone"`
}

// RunUnit collects the coordination report for one issue and writes its
// artifacts. It returns 0 when every read finished and every Dependencies
// line was typed, 3 when every read finished and at least one line is an
// UNKNOWN relationship, 2 when any other UNKNOWN entry exists (artifacts are
// still written), and 1 with an error for a hard failure.
func RunUnit(ctx context.Context, config UnitConfig, runner QueryRunner, clock func() time.Time) (int, error) {
	snapshot, err := CollectUnit(ctx, config, runner, clock)
	if err != nil {
		return 1, err
	}
	if err := WriteUnitArtifacts(config.OutputDir, snapshot); err != nil {
		return 1, err
	}
	if len(snapshot.Unknowns) > 0 {
		return 2, nil
	}
	if len(snapshot.UnknownRelationships) > 0 {
		return 3, nil
	}
	return 0, nil
}

// CollectUnit reads the evidence the Claiming protocol asks for on one open
// issue (docs/coordination.md, Claiming): its typed Dependencies, its direct
// forward and reverse exclusive-with set, each member's claim and reservation
// state, and the open trackers listing it. It never writes to the forge and
// decides nothing about whether work may start.
//
// Only a failed open-issue inventory, or an assigned issue that is not open,
// ends the run. Every other read that fails or stops at the page cap is
// recorded as an UNKNOWN entry, and no member whose state rests on it is
// reported with a claim state.
func CollectUnit(ctx context.Context, config UnitConfig, runner QueryRunner, clock func() time.Time) (UnitSnapshot, error) {
	if config.Issue <= 0 || config.Issue > MaxGraphQLInt {
		return UnitSnapshot{}, fmt.Errorf("issue number must be between 1 and %d", MaxGraphQLInt)
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
	// reads every member at the same instant.
	startedAt := clock().UTC()

	openIssues, err := c.fetchOpenIssues(ctx)
	if err != nil {
		return UnitSnapshot{}, err
	}
	inventoryComplete := !c.hasAmbiguity("page-cap-truncation", "open issues")
	open := make(map[int]graphIssue, len(openIssues))
	for _, issue := range openIssues {
		open[issue.Number] = issue
		if *issue.Labels.PageInfo.HasNextPage {
			c.ambiguous("label-truncation", fmt.Sprintf("issue #%d labels", issue.Number), "more labels exist than one page holds; its classification is unknown")
		}
	}
	assigned, found := open[config.Issue]
	if !found {
		if !inventoryComplete {
			return UnitSnapshot{}, fmt.Errorf("issue #%d is not in the open-issue inventory, which stopped at the page cap", config.Issue)
		}
		return UnitSnapshot{}, fmt.Errorf("issue #%d is not an open issue", config.Issue)
	}
	openPullRequests, pullRequestsComplete := c.fetchClaimingPullRequests(ctx)

	snapshot := UnitSnapshot{
		SchemaVersion: UnitSchemaVersion,
		Repository:    config.Repository.String(),
		StartedAt:     startedAt,
		Issue:         config.Issue,
		Title:         assigned.Title,
		Dependencies:  []UnitDependency{},
		Members:       []UnitMember{},
		Trackers:      []UnitTracker{},
	}
	dependencies, own, forward := assignedDependencies(assigned)
	snapshot.Dependencies = dependencies
	reverse, elsewhere := reverseExclusivity(openIssues, config.Issue)
	snapshot.UnknownRelationships = append(own, elsewhere...)

	numbers := []int{config.Issue}
	for number := range forward {
		numbers = append(numbers, number)
	}
	for number := range reverse {
		if !forward[number] {
			numbers = append(numbers, number)
		}
	}
	sort.Ints(numbers[1:])
	for _, number := range numbers {
		member := UnitMember{Number: number, Claims: []ContractClaim{}, LegacyPullRequests: []int{}, Reservations: []ContractReservation{}}
		issue, isOpen := open[number]
		switch {
		case isOpen:
			member, err = c.collectMember(ctx, issue, openPullRequests, pullRequestsComplete, startedAt)
			if err != nil {
				return UnitSnapshot{}, err
			}
		case inventoryComplete:
			member.State = MemberNotOpen
		default:
			member.State = MemberUnknown
		}
		if number == config.Issue {
			member.Relations = append(member.Relations, RelationAssigned)
		}
		if forward[number] {
			member.Relations = append(member.Relations, RelationForward)
		}
		if reverse[number] {
			member.Relations = append(member.Relations, RelationReverse)
		}
		snapshot.Members = append(snapshot.Members, member)
	}

	for _, number := range c.contractTrackers(openIssues)[config.Issue] {
		tracker := UnitTracker{Number: number, Title: open[number].Title}
		if open[number].Milestone != nil {
			tracker.Milestone = open[number].Milestone.Title
		}
		snapshot.Trackers = append(snapshot.Trackers, tracker)
	}
	if assigned.hasLabel(contractLabel) {
		// A contract unit's conflict set also holds every open contract unit
		// with no recorded independence assessment, and its claim arbitrates
		// the cap (docs/coordination.md, Claiming). Neither is computed here,
		// so the declared set must not pass for the full one.
		c.ambiguous("contract-conflict-set", fmt.Sprintf("issue #%d", config.Issue),
			"the issue is kind:contract: this report covers its declared exclusive-with set only and computes neither the contract conflict set nor the cap; `trackercollect contracts` reports every open contract unit's claim state and the active count")
	}

	snapshot.Unknowns = c.sortedAmbiguities()
	snapshot.Complete = len(snapshot.Unknowns) == 0
	snapshot.CompletedAt = clock().UTC()
	return snapshot, nil
}

// fetchClaimingPullRequests reads the open PRs and each one's closing issues.
// A claim on any member can rest on any of them, so a read that fails or
// stops at the page cap is recorded and reported as incomplete instead of
// ending the run.
func (c *collector) fetchClaimingPullRequests(ctx context.Context) ([]OpenPullRequest, bool) {
	before := len(c.ambiguities)
	openPullRequests, err := c.fetchOpenPullRequests(ctx)
	if err != nil {
		c.ambiguous(readFailureCode, "open pull requests", err.Error())
	}
	for i := range openPullRequests {
		linked, err := c.fetchPullRequestClosingIssues(ctx, openPullRequests[i].Number)
		if err != nil {
			c.ambiguous(readFailureCode, fmt.Sprintf("pull request #%d linked issues", openPullRequests[i].Number), err.Error())
			continue
		}
		openPullRequests[i].LinkedIssues = linked
	}
	// The shared fetchers flag duplicate Scope lines and sections for the
	// merged-PR report. This report reads no scope, so they are not its
	// ambiguities.
	c.dropAmbiguities("parse-collision")
	return openPullRequests, len(c.ambiguities) == before
}

// assignedDependencies reads the assigned issue's Dependencies field. It
// returns the typed and none lines, every line it could not type, and the
// issues the field declares exclusive-with.
func assignedDependencies(assigned graphIssue) ([]UnitDependency, []UnknownRelationship, map[int]bool) {
	dependencies := []UnitDependency{}
	unknown := []UnknownRelationship{}
	forward := make(map[int]bool)
	sections := extractSections(assigned.Body, "Dependencies")
	if len(sections) != 1 {
		unknown = append(unknown, UnknownRelationship{
			Issue: assigned.Number, Reason: fmt.Sprintf("expected exactly one Dependencies section, found %d", len(sections)),
		})
	}
	for _, section := range sections {
		for _, entry := range parseDependencies(section) {
			if entry.kind == dependencyUnknown {
				unknown = append(unknown, UnknownRelationship{Issue: assigned.Number, Line: entry.line, Reason: entry.reason})
				continue
			}
			dependencies = append(dependencies, UnitDependency{
				Kind: entry.kind, Relationship: entry.relationship,
				Targets: append([]DependencyTarget{}, entry.targets...), Line: entry.line,
			})
			// A typed or none line can still name a partner its lead does
			// not declare: the set would then miss it without a trace. This
			// is the reverse scan's rule, applied to the issue's own lines.
			if undeclared := entry.undeclaredReferences(); len(undeclared) > 0 {
				unknown = append(unknown, UnknownRelationship{
					Issue: assigned.Number, Line: entry.line,
					Reason: fmt.Sprintf("carries exclusive-with and %s outside its declared exclusive-with targets", strings.Join(undeclared, ", ")),
				})
			}
			if entry.relationship != RelationshipExclusiveWith {
				continue
			}
			for _, target := range entry.targets {
				switch {
				case target.Kind == TargetPullRequest:
					unknown = append(unknown, UnknownRelationship{
						Issue: assigned.Number, Line: entry.line,
						Reason: fmt.Sprintf("exclusive-with names pull request #%d, not a unit issue; its unit is not in the set", target.Number),
					})
				case target.Number != assigned.Number:
					forward[target.Number] = true
				}
			}
		}
	}
	return dependencies, unknown, forward
}

// reverseExclusivity scans every other open issue's Dependencies field for a
// declaration naming the assigned issue. It returns the declaring issues and
// the lines that may declare the pair without parsing as a declaration: an
// exclusive-with keyword with no usable target, which could mean any unit,
// and a line that carries both exclusive-with and the assigned issue's
// number. An issue with no Dependencies section has no field to declare in,
// so a line of its body carrying both is reported and never read as a
// declaration: the field may sit under a heading this tool does not know.
func reverseExclusivity(openIssues []graphIssue, assigned int) (map[int]bool, []UnknownRelationship) {
	reverse := make(map[int]bool)
	unknown := []UnknownRelationship{}
	for _, issue := range openIssues {
		if issue.Number == assigned {
			continue
		}
		sections := extractSections(issue.Body, "Dependencies")
		if len(sections) == 0 {
			// parseDependencies skips a heading line; the body has none.
			for _, entry := range parseDependencies("\n" + issue.Body) {
				if entry.mentionsExclusiveWith() && entry.mentionsIssue(assigned) {
					unknown = append(unknown, UnknownRelationship{
						Issue: issue.Number, Line: entry.line,
						Reason: fmt.Sprintf("carries exclusive-with and #%d outside a Dependencies section", assigned),
					})
				}
			}
		}
		for _, section := range sections {
			for _, entry := range parseDependencies(section) {
				switch {
				case entry.declaresExclusiveWith(assigned):
					reverse[issue.Number] = true
				case entry.kind == dependencyUnknown && entry.relationship == RelationshipExclusiveWith:
					unknown = append(unknown, UnknownRelationship{Issue: issue.Number, Line: entry.line, Reason: entry.reason})
				case entry.mentionsExclusiveWith() && entry.mentionsIssue(assigned):
					unknown = append(unknown, UnknownRelationship{
						Issue: issue.Number, Line: entry.line,
						Reason: fmt.Sprintf("carries exclusive-with and #%d without declaring the pair", assigned),
					})
				}
			}
		}
	}
	sort.SliceStable(unknown, func(i, j int) bool { return unknown[i].Issue < unknown[j].Issue })
	return reverse, unknown
}

// collectMember reads one open member's comments and applies the contracts
// mode's claim-state resolver to them. The member is UNKNOWN, with whatever
// evidence was retained, when its comments were not fully read, when one of
// them is a malformed marker or a marker from an untrusted author, or when
// the PR evidence a claim could rest on is incomplete.
func (c *collector) collectMember(ctx context.Context, issue graphIssue, openPullRequests []OpenPullRequest, pullRequestsComplete bool, now time.Time) (UnitMember, error) {
	before := len(c.ambiguities)
	comments, err := c.fetchIssueComments(ctx, issue.Number)
	if err != nil {
		c.ambiguous(readFailureCode, fmt.Sprintf("issue #%d comments", issue.Number), err.Error())
	}
	var markers []MarkerComment
	for _, comment := range comments {
		if marker, keep := c.parseMarkerComment(issue.Number, comment, openPullRequests); keep && c.countsMarker(marker) {
			markers = append(markers, marker)
		}
	}
	sort.SliceStable(markers, func(i, j int) bool {
		if markers[i].CreatedAt != markers[j].CreatedAt {
			return markers[i].CreatedAt < markers[j].CreatedAt
		}
		return markers[i].Stamp.DatabaseID < markers[j].Stamp.DatabaseID
	})
	unit, err := deriveContractState(issue, markers, openPullRequests, now)
	if err != nil {
		return UnitMember{}, err
	}
	member := UnitMember{
		Number: unit.Number, Title: unit.Title, State: unit.State, Milestone: unit.Milestone, Deferral: unit.Deferral,
		OrderingClaim: unit.OrderingClaim, Claims: unit.Claims, LegacyPullRequests: unit.LegacyPullRequests,
		Reservations: unit.Reservations, Stamp: &unit.Stamp,
	}
	if !pullRequestsComplete || len(c.ambiguities) > before {
		member.State = MemberUnknown
		member.OrderingClaim = nil
	}
	return member, nil
}
