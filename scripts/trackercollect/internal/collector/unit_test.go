package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// dependencies replaces a fixture issue's body with a Dependencies section
// holding the lines.
func (f *contractFixture) dependencies(number int, lines ...string) {
	for _, issue := range f.issues {
		if issue["number"] == number {
			issue["body"] = "### Dependencies\n\n" + strings.Join(lines, "\n")
		}
	}
}

func unitConfig(output string, issue int) UnitConfig {
	return UnitConfig{
		Repository: Repository{Host: "github.com", Owner: "freeside-ai", Name: "freeside"},
		Issue:      issue,
		OutputDir:  output,
		PageSize:   100,
		PageCap:    10,
	}
}

func collectUnit(t *testing.T, fixture *contractFixture, issue int) UnitSnapshot {
	t.Helper()
	snapshot, err := CollectUnit(context.Background(), unitConfig("", issue), fixture.runner(t), fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func unitMember(t *testing.T, snapshot UnitSnapshot, number int) UnitMember {
	t.Helper()
	for _, member := range snapshot.Members {
		if member.Number == number {
			return member
		}
	}
	t.Fatalf("member #%d not in snapshot: %#v", number, snapshot.Members)
	return UnitMember{}
}

func memberStates(snapshot UnitSnapshot) string {
	parts := make([]string, 0, len(snapshot.Members))
	for _, member := range snapshot.Members {
		parts = append(parts, fmt.Sprintf("#%d=%s(%s)", member.Number, member.State, strings.Join(member.Relations, "+")))
	}
	return strings.Join(parts, " ")
}

func unknownRelationshipLines(snapshot UnitSnapshot) []string {
	lines := make([]string, 0, len(snapshot.UnknownRelationships))
	for _, unknown := range snapshot.UnknownRelationships {
		lines = append(lines, fmt.Sprintf("#%d|%s|%s", unknown.Issue, unknown.Line, unknown.Reason))
	}
	return lines
}

func unknownCodes(snapshot UnitSnapshot) string {
	parts := make([]string, 0, len(snapshot.Unknowns))
	for _, unknown := range snapshot.Unknowns {
		parts = append(parts, fmt.Sprintf("%s(%s)", unknown.Code, unknown.Subject))
	}
	return strings.Join(parts, " ")
}

func TestUnitDependenciesAreTypedOrUnknown(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "", "kind:feature")
	fixture.dependencies(10,
		"- `starts-after` #5: the resolver lands there",
		"- merges-after: PR #6 / #7.",
		"- `exclusive-with: #20` while both stay open",
		"- `stacked-on`: none.",
		"- Builds on merged #302.",
		"Refs #12.",
		"- `exclusive-with`: every other open `kind:contract` unit",
	)
	fixture.issue(20, "", "kind:feature")
	snapshot := collectUnit(t, fixture, 10)

	want := []UnitDependency{
		{Kind: DependencyTyped, Relationship: "starts-after", Targets: []DependencyTarget{{TargetIssue, 5}}, Line: "- `starts-after` #5: the resolver lands there"},
		{Kind: DependencyTyped, Relationship: "merges-after", Targets: []DependencyTarget{{TargetPullRequest, 6}, {TargetIssue, 7}}, Line: "- merges-after: PR #6 / #7."},
		{Kind: DependencyTyped, Relationship: "exclusive-with", Targets: []DependencyTarget{{TargetIssue, 20}}, Line: "- `exclusive-with: #20` while both stay open"},
		{Kind: DependencyNone, Relationship: "stacked-on", Targets: []DependencyTarget{}, Line: "- `stacked-on`: none."},
	}
	if !reflect.DeepEqual(snapshot.Dependencies, want) {
		t.Errorf("dependencies = %#v", snapshot.Dependencies)
	}
	wantUnknown := []string{
		"#10|- Builds on merged #302.|" + reasonUntyped,
		"#10|Refs #12.|" + reasonUntyped,
		"#10|- `exclusive-with`: every other open `kind:contract` unit|" + reasonNoTarget,
	}
	if got := unknownRelationshipLines(snapshot); !reflect.DeepEqual(got, wantUnknown) {
		t.Errorf("unknown relationships = %q", got)
	}
	if !snapshot.Complete || memberStates(snapshot) != "#10=idle(assigned) #20=idle(forward)" {
		t.Errorf("complete=%v members=%s unknowns=%s", snapshot.Complete, memberStates(snapshot), unknownCodes(snapshot))
	}

	t.Run("a none line", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "", "kind:feature")
		snapshot := collectUnit(t, fixture, 10)
		want := []UnitDependency{{Kind: DependencyNone, Targets: []DependencyTarget{}, Line: "none"}}
		if !reflect.DeepEqual(snapshot.Dependencies, want) || len(snapshot.UnknownRelationships) != 0 {
			t.Errorf("dependencies=%#v unknown=%q", snapshot.Dependencies, unknownRelationshipLines(snapshot))
		}
	})
	for name, body := range map[string]string{
		"a missing section":  "### Objective\n\n- `exclusive-with` #20",
		"a repeated section": "### Dependencies\n\nnone\n\n### Dependencies\n\n- `exclusive-with` #20",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newContractFixture()
			fixture.issue(10, "", "kind:feature")
			fixture.issues[0]["body"] = body
			fixture.issue(20, "", "kind:feature")
			snapshot := collectUnit(t, fixture, 10)
			got := unknownRelationshipLines(snapshot)
			if len(got) != 1 || !strings.HasPrefix(got[0], "#10||expected exactly one Dependencies section, found ") {
				t.Errorf("unknown relationships = %q", got)
			}
			// A repeated section is still read, so a partner it declares is
			// never left out of the set.
			if wantMember := name == "a repeated section"; (len(snapshot.Members) == 2) != wantMember {
				t.Errorf("members = %s", memberStates(snapshot))
			}
		})
	}
}

func TestUnitExclusivitySetIsForwardAndReverse(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "", "kind:feature")
	fixture.dependencies(10, "- `exclusive-with` #20, #21 and #30", "- exclusive-with #10")
	fixture.issue(20, "", "kind:feature")
	fixture.claim(20, 2000, time.Hour, "feat/twenty")
	// #21 is not in the open inventory: a closed partner, read no further.
	fixture.issue(30, "", "kind:feature")
	fixture.dependencies(30, "- `exclusive-with`: #10. Same card.")
	fixture.issue(31, "", "kind:feature")
	fixture.dependencies(31, "  - exclusive-with #99 and #10")
	fixture.reservation(31, 3100, time.Hour)
	// A nested item, indented as the forge allows under a list item.
	fixture.issue(32, "", "kind:feature")
	fixture.dependencies(32, "- `starts-after` #99", "    - exclusive-with #10")
	// Declarations that do not name #10, and a #10 line of another type.
	fixture.issue(40, "", "kind:feature")
	fixture.dependencies(40, "- `exclusive-with` #99", "- `starts-after` #10")
	// Lines that may declare the pair without parsing as a declaration.
	fixture.issue(50, "", "kind:feature")
	fixture.dependencies(50, "- `exclusive-with`: every other open `kind:contract` unit")
	fixture.issue(51, "", "kind:feature")
	fixture.dependencies(51, "- `starts-after` #99. In practice exclusive-with #10 too.")
	fixture.issue(52, "", "kind:feature")
	fixture.dependencies(52, "- `exclusive-with` #99: both edit the card", "  that #10 introduced.")
	fixture.issue(53, "", "kind:feature")
	fixture.dependencies(53, "- exclusive-with PR #10")
	fixture.issue(54, "", "kind:feature")
	fixture.dependencies(54, "- exclusive-with #0")
	// No Dependencies section: the field may sit under another heading, so
	// a line naming the pair is reported and a line naming another is not.
	fixture.issue(55, "", "kind:feature")
	fixture.issues[len(fixture.issues)-1]["body"] = "### Dependencies and Disposition\n\n- exclusive-with #10\n- exclusive-with #99\n- exclusive-with every contract unit"
	snapshot := collectUnit(t, fixture, 10)

	if got := memberStates(snapshot); got != "#10=idle(assigned) #20=active-lease(forward) #21=not-open(forward) #30=idle(forward+reverse) #31=reserved(reverse) #32=idle(reverse)" {
		t.Errorf("members = %s", got)
	}
	if !snapshot.Complete {
		t.Errorf("unknowns = %s", unknownCodes(snapshot))
	}
	if closed := unitMember(t, snapshot, 21); closed.Stamp != nil || closed.Title != "" || len(closed.Claims) != 0 {
		t.Errorf("not-open member = %#v", closed)
	}
	want := []string{
		"#50|- `exclusive-with`: every other open `kind:contract` unit|" + reasonNoTarget,
		"#51|- `starts-after` #99. In practice exclusive-with #10 too.|carries exclusive-with and #10 without declaring the pair",
		"#52|- `exclusive-with` #99: both edit the card\nthat #10 introduced.|carries exclusive-with and #10 without declaring the pair",
		"#53|- exclusive-with PR #10|carries exclusive-with and #10 without declaring the pair",
		"#54|- exclusive-with #0|" + reasonTargetOutRange,
		"#55|- exclusive-with #10|carries exclusive-with and #10 outside a Dependencies section",
	}
	if got := unknownRelationshipLines(snapshot); !reflect.DeepEqual(got, want) {
		t.Errorf("unknown relationships = %q", got)
	}
}

func TestUnitAssignedLineCannotHideAnExclusivePartner(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "", "kind:feature")
	fixture.dependencies(10,
		"- `starts-after` #5; exclusive-with #20 while both are open",
		"- none otherwise, though exclusive-with: #21 in practice",
		"- exclusive-with PR #400",
		"- `exclusive-with` #20 or #22",
		"- `exclusive-with` #20,",
		"    #23",
	)
	fixture.issue(20, "", "kind:feature")
	snapshot := collectUnit(t, fixture, 10)
	want := []string{
		"#10|- `starts-after` #5; exclusive-with #20 while both are open|carries exclusive-with and #5, #20 outside its declared exclusive-with targets",
		"#10|- none otherwise, though exclusive-with: #21 in practice|carries exclusive-with and #21 outside its declared exclusive-with targets",
		"#10|- exclusive-with PR #400|exclusive-with names pull request #400, not a unit issue; its unit is not in the set",
		"#10|- `exclusive-with` #20 or #22|carries exclusive-with and #22 outside its declared exclusive-with targets",
		"#10|- `exclusive-with` #20,\n#23|carries exclusive-with and #23 outside its declared exclusive-with targets",
	}
	if got := unknownRelationshipLines(snapshot); !reflect.DeepEqual(got, want) {
		t.Errorf("unknown relationships = %q", got)
	}
	if got := memberStates(snapshot); got != "#10=idle(assigned) #20=idle(forward)" {
		t.Errorf("members = %s", got)
	}
}

func TestUnitClaimStatesMatchTheContractsResolver(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "", "kind:feature")
	fixture.dependencies(10, "- `exclusive-with` #11, #12, #13, #14, #15, #16, #17, #18, #19")
	fixture.claim(10, 1000, time.Hour, "feat/ten")
	// Lease at its 48th hour with no close-keyword PR.
	fixture.issue(11, "1B", contractLabel)
	fixture.claim(11, 1100, 48*time.Hour, "feat/eleven")
	// Old claim kept alive by an open PR that closes the issue.
	fixture.issue(12, "1B", contractLabel)
	fixture.claim(12, 1200, 72*time.Hour, "feat/twelve")
	fixture.pullRequest(412, "feat/twelve", canonicalRepository, 12)
	// Open PR closing the issue with no claim comment.
	fixture.issue(13, "1B", contractLabel)
	fixture.pullRequest(413, "feat/thirteen", canonicalRepository, 13)
	// Claim released by a release naming its comment ID.
	fixture.issue(14, "1B", contractLabel)
	fixture.claim(14, 1400, 2*time.Hour, "feat/fourteen")
	fixture.release(14, 1401, time.Hour, "feat/fourteen", 1400)
	// A release naming another ID releases nothing.
	fixture.issue(15, "1B", contractLabel)
	fixture.claim(15, 1500, 2*time.Hour, "feat/fifteen")
	fixture.release(15, 1501, time.Hour, "feat/fifteen", 9999)
	// Planning reservations, active and at their 48th hour.
	fixture.issue(16, "1B", contractLabel)
	fixture.reservation(16, 1600, time.Hour)
	fixture.issue(17, "1B", contractLabel)
	fixture.reservation(17, 1700, 48*time.Hour)
	fixture.issue(18, "", contractLabel, deferralLabel)
	fixture.issue(19, "1B", contractLabel)
	snapshot := collectUnit(t, fixture, 10)
	if !snapshot.Complete {
		t.Fatalf("unknowns = %s", unknownCodes(snapshot))
	}

	wantStates := map[int]string{
		10: ContractActiveLease, 11: ContractExpired, 12: ContractActivePR, 13: ContractActivePRLegacy, 14: ContractIdle,
		15: ContractActiveLease, 16: ContractReserved, 17: ContractIdle, 18: ContractDormant, 19: ContractIdle,
	}
	contracts := collectContracts(t, fixture)
	for number, state := range wantStates {
		member := unitMember(t, snapshot, number)
		if member.State != state {
			t.Errorf("#%d state = %s, want %s", number, member.State, state)
		}
		if number == 10 {
			continue
		}
		// Every member but the assigned issue is a contract issue here, so the
		// contracts mode derives the same unit from the same evidence.
		unit := contractUnit(t, contracts, number)
		if member.State != unit.State || !reflect.DeepEqual(member.OrderingClaim, unit.OrderingClaim) || !reflect.DeepEqual(member.Claims, unit.Claims) ||
			!reflect.DeepEqual(member.LegacyPullRequests, unit.LegacyPullRequests) || !reflect.DeepEqual(member.Reservations, unit.Reservations) ||
			member.Stamp == nil || *member.Stamp != unit.Stamp || member.Milestone != unit.Milestone || member.Deferral != unit.Deferral {
			t.Errorf("#%d member = %#v\ncontracts unit = %#v", number, member, unit)
		}
	}

	created := func(age time.Duration) string { return fixedClock().Add(-age).Format(time.RFC3339) }
	assigned := unitMember(t, snapshot, 10)
	if assigned.OrderingClaim == nil || assigned.OrderingClaim.CommentID != 1000 || assigned.OrderingClaim.CreatedAt != created(time.Hour) {
		t.Errorf("assigned ordering claim = %#v", assigned.OrderingClaim)
	}
	backed := unitMember(t, snapshot, 12)
	if len(backed.Claims) != 1 || backed.Claims[0].CommentID != 1200 || backed.Claims[0].CreatedAt != created(72*time.Hour) ||
		backed.Claims[0].Status != ClaimPRBacked || !reflect.DeepEqual(backed.Claims[0].ClosingPullRequests, []int{412}) {
		t.Errorf("PR-backed claims = %#v", backed.Claims)
	}
	if legacy := unitMember(t, snapshot, 13); !reflect.DeepEqual(legacy.LegacyPullRequests, []int{413}) {
		t.Errorf("legacy PRs = %#v", legacy.LegacyPullRequests)
	}
	if released := unitMember(t, snapshot, 14); released.Claims[0].Status != ClaimReleased || released.OrderingClaim != nil {
		t.Errorf("released = %#v", released)
	}
	if unbound := unitMember(t, snapshot, 15); unbound.Claims[0].Status != ClaimLive || unbound.OrderingClaim.CommentID != 1500 {
		t.Errorf("claim beside an unbound release = %#v", unbound)
	}
	wantReservation := []ContractReservation{{CommentID: 1600, CreatedAt: created(time.Hour), Active: true}}
	if reserved := unitMember(t, snapshot, 16); !reflect.DeepEqual(reserved.Reservations, wantReservation) {
		t.Errorf("reservation = %#v", reserved.Reservations)
	}
	if lapsed := unitMember(t, snapshot, 17); len(lapsed.Reservations) != 1 || lapsed.Reservations[0].Active || lapsed.Reservations[0].CommentID != 1700 {
		t.Errorf("expired reservation = %#v", lapsed.Reservations)
	}

	report := RenderUnitReport(snapshot)
	for _, want := range []string{
		"- #12 Unit 12: `active-pr` (#10 declares `exclusive-with` it), milestone: 1B",
		"  - Ordering key: claim comment 1200, created " + created(72*time.Hour) + ".",
		"  - Claim comment 1200 at " + created(72*time.Hour) + " on `feat/twelve`: pr-backed, open PR closing the issue: #412.",
		"  - Open PR closing the issue with no claim comment behind it: #413.",
		"  - Planning reservation comment 1600 at " + created(time.Hour) + ": active.",
		"  - Planning reservation comment 1700 at " + created(48*time.Hour) + ": expired.",
		"- #18 Unit 18: `dormant` (#10 declares `exclusive-with` it), milestone: none, deferral",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

// assertIncompleteUnitReport checks that a report over incomplete evidence
// says so and calls no list empty.
func assertIncompleteUnitReport(t *testing.T, directory string) UnitSnapshot {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(directory, "unit.md"))
	if err != nil {
		t.Fatal(err)
	}
	report := string(raw)
	if strings.Contains(report, "\nNone.\n") {
		t.Errorf("report over incomplete evidence calls a list empty:\n%s", report)
	}
	for _, want := range []string{"The evidence is incomplete.", "Members found in retained evidence; the set may be larger."} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	var snapshot UnitSnapshot
	rawJSON, err := os.ReadFile(filepath.Join(directory, "unit.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawJSON, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Complete || len(snapshot.Unknowns) == 0 {
		t.Errorf("snapshot complete=%v unknowns=%#v", snapshot.Complete, snapshot.Unknowns)
	}
	for _, member := range snapshot.Members {
		// A member the inventory does not hold has no title.
		heading := strings.TrimSuffix(fmt.Sprintf("- #%d %s", member.Number, member.Title), " ") + ": `UNKNOWN`"
		if member.State == MemberUnknown && (member.OrderingClaim != nil || !strings.Contains(report, heading)) {
			t.Errorf("UNKNOWN member #%d carries an ordering claim or is not marked in the report", member.Number)
		}
	}
	return snapshot
}

// incompleteUnitFixture has an assigned issue with a live claim, a reverse
// partner with a live claim, a forward partner that is not open, and one
// unrelated open PR. Complete evidence over it exits 0.
func incompleteUnitFixture() *contractFixture {
	fixture := newContractFixture()
	fixture.issue(10, "", "kind:feature")
	fixture.dependencies(10, "- `exclusive-with` #21")
	fixture.claim(10, 1000, time.Hour, "feat/ten")
	fixture.issue(11, "", "kind:feature")
	fixture.dependencies(11, "- `exclusive-with` #10")
	fixture.claim(11, 1100, 2*time.Hour, "feat/eleven")
	fixture.pullRequest(400, "feat/unrelated", canonicalRepository)
	return fixture
}

func runIncompleteUnit(t *testing.T, runner *fixtureRunner, wantUnknown, wantMembers string) {
	t.Helper()
	directory := t.TempDir()
	config := unitConfig(directory, 10)
	config.PageCap = 1
	exit, err := RunUnit(context.Background(), config, runner, fixedClock)
	if err != nil || exit != 2 {
		t.Fatalf("exit=%d error=%v", exit, err)
	}
	snapshot := assertIncompleteUnitReport(t, directory)
	if got := unknownCodes(snapshot); got != wantUnknown {
		t.Errorf("unknowns = %s, want %s", got, wantUnknown)
	}
	if got := memberStates(snapshot); got != wantMembers {
		t.Errorf("members = %s, want %s", got, wantMembers)
	}
}

func TestUnitCompleteEvidenceOverTheIncompleteFixtureExitsZero(t *testing.T) {
	directory := t.TempDir()
	exit, err := RunUnit(context.Background(), unitConfig(directory, 10), incompleteUnitFixture().runner(t), fixedClock)
	if err != nil || exit != 0 {
		t.Fatalf("exit=%d error=%v", exit, err)
	}
	snapshot := collectUnit(t, incompleteUnitFixture(), 10)
	if got := memberStates(snapshot); got != "#10=active-lease(assigned) #11=active-lease(reverse) #21=not-open(forward)" {
		t.Errorf("members = %s", got)
	}
}

func TestUnitPageCapsKeepPartialEvidenceAndExitTwo(t *testing.T) {
	t.Run("open-issue inventory", func(t *testing.T) {
		runner := incompleteUnitFixture().runner(t)
		setFixturePageInfo(t, runner, "OpenIssues::", []string{"data", "repository", "issues"}, morePages())
		// The members' own reads finished, so their states stand. A partner
		// missing from a truncated inventory is not known to be closed.
		runIncompleteUnit(t, runner, "page-cap-truncation(open issues)",
			"#10=active-lease(assigned) #11=active-lease(reverse) #21=UNKNOWN(forward)")
	})
	t.Run("open-PR list", func(t *testing.T) {
		runner := incompleteUnitFixture().runner(t)
		setFixturePageInfo(t, runner, "OpenPullRequests::", []string{"data", "repository", "pullRequests"}, morePages())
		runIncompleteUnit(t, runner, "page-cap-truncation(open pull requests)",
			"#10=UNKNOWN(assigned) #11=UNKNOWN(reverse) #21=not-open(forward)")
	})
	t.Run("a PR's closing issues", func(t *testing.T) {
		runner := incompleteUnitFixture().runner(t)
		setFixturePageInfo(t, runner, "PullRequestClosingIssues:400:", []string{"data", "repository", "pullRequest", "closingIssuesReferences"}, morePages())
		runIncompleteUnit(t, runner, "page-cap-truncation(pull request #400 linked issues)",
			"#10=UNKNOWN(assigned) #11=UNKNOWN(reverse) #21=not-open(forward)")
	})
	t.Run("a member's comments", func(t *testing.T) {
		runner := incompleteUnitFixture().runner(t)
		setFixturePageInfo(t, runner, "IssueComments:11:", []string{"data", "repository", "issue", "comments"}, morePages())
		runIncompleteUnit(t, runner, "page-cap-truncation(issue #11 comments)",
			"#10=active-lease(assigned) #11=UNKNOWN(reverse) #21=not-open(forward)")
	})
}

func TestUnitNeverShowsAnUnreadMemberAsUnclaimed(t *testing.T) {
	onlyPartner := "#10=active-lease(assigned) #11=UNKNOWN(reverse) #21=not-open(forward)"
	everyMember := "#10=UNKNOWN(assigned) #11=UNKNOWN(reverse) #21=not-open(forward)"
	t.Run("a member's comment read fails", func(t *testing.T) {
		runner := incompleteUnitFixture().runner(t)
		delete(runner.responses, "IssueComments:11:")
		runIncompleteUnit(t, runner, "read-failure(issue #11 comments)", onlyPartner)
	})
	t.Run("a malformed marker among a member's comments", func(t *testing.T) {
		fixture := incompleteUnitFixture()
		fixture.comment(11, 1101, time.Hour, releaseMarker+"\nRelease: feat/eleven")
		runIncompleteUnit(t, fixture.runner(t), "malformed-marker-comment(comment 1101)", onlyPartner)
	})
	t.Run("a marker from an untrusted author", func(t *testing.T) {
		// Counted, the release would read #11's live claim as released.
		fixture := incompleteUnitFixture()
		fixture.release(11, 1101, time.Hour, "feat/eleven", 1100)
		fixture.postedBy(11, "NONE")
		runIncompleteUnit(t, fixture.runner(t), "untrusted-marker-author(comment 1101)", onlyPartner)
	})
	t.Run("the open-PR read fails", func(t *testing.T) {
		runner := incompleteUnitFixture().runner(t)
		delete(runner.responses, "OpenPullRequests::")
		runIncompleteUnit(t, runner, "read-failure(open pull requests)", everyMember)
	})
	t.Run("a closing-issue read fails", func(t *testing.T) {
		runner := incompleteUnitFixture().runner(t)
		delete(runner.responses, "PullRequestClosingIssues:400:")
		runIncompleteUnit(t, runner, "read-failure(pull request #400 linked issues)", everyMember)
	})
	t.Run("retained claims stay beside an UNKNOWN state", func(t *testing.T) {
		runner := incompleteUnitFixture().runner(t)
		delete(runner.responses, "PullRequestClosingIssues:400:")
		snapshot, err := CollectUnit(context.Background(), unitConfig("", 10), runner, fixedClock)
		if err != nil {
			t.Fatal(err)
		}
		member := unitMember(t, snapshot, 11)
		if member.State != MemberUnknown || member.OrderingClaim != nil || len(member.Claims) != 1 || member.Claims[0].CommentID != 1100 {
			t.Errorf("member = %#v", member)
		}
		if report := RenderUnitReport(snapshot); !strings.Contains(report, "  - Claim state not established; see **UNKNOWN**. Any claim, PR, or reservation below is retained evidence only.\n  - Claim comment 1100") {
			t.Errorf("report = %s", report)
		}
	})
}

func TestUnitTrackersListingTheIssue(t *testing.T) {
	base := func() *contractFixture {
		fixture := newContractFixture()
		fixture.issue(10, "1B", "kind:feature")
		fixture.tracker(100, "## Units\n- [ ] #10\n- [x] #11\n\n## Notes\n- [ ] #12")
		fixture.issues[len(fixture.issues)-1]["milestone"] = map[string]any{"title": "1B"}
		fixture.tracker(101, "## Units\n- [x] #10")
		// Listed under Notes only, on another tracker, and on an issue
		// without the tracker label: none lists #10.
		fixture.tracker(102, "## Units\n- [ ] #11\n\n## Notes\n- [ ] #10")
		fixture.issue(103, "", "kind:feature")
		fixture.issues[len(fixture.issues)-1]["body"] = "## Units\n- [ ] #10"
		return fixture
	}
	snapshot := collectUnit(t, base(), 10)
	want := []UnitTracker{{Number: 100, Title: "Unit 100", Milestone: "1B"}, {Number: 101, Title: "Unit 101"}}
	if !snapshot.Complete || !reflect.DeepEqual(snapshot.Trackers, want) {
		t.Errorf("trackers = %#v unknowns = %s", snapshot.Trackers, unknownCodes(snapshot))
	}
	report := RenderUnitReport(snapshot)
	if !strings.Contains(report, "- #100 Unit 100, milestone: 1B\n- #101 Unit 101, milestone: none\n") {
		t.Errorf("report = %s", report)
	}

	for name, tc := range map[string]struct{ body, unknown string }{
		"no Units section":        {"## Status\nActive\n\n- [ ] #10", "tracker-structure(issue #104)"},
		"an invalid issue number": {"## Units\n- [ ] #10\n- [ ] #0", "malformed-tracker-entry(issue #104)"},
	} {
		t.Run("a tracker with "+name, func(t *testing.T) {
			fixture := base()
			fixture.tracker(104, tc.body)
			directory := t.TempDir()
			exit, err := RunUnit(context.Background(), unitConfig(directory, 10), fixture.runner(t), fixedClock)
			if err != nil || exit != 2 {
				t.Fatalf("exit=%d error=%v", exit, err)
			}
			snapshot := assertIncompleteUnitReport(t, directory)
			if got := unknownCodes(snapshot); got != tc.unknown || len(snapshot.Trackers) != 2 {
				t.Errorf("unknowns = %s trackers = %#v", got, snapshot.Trackers)
			}
		})
	}
	t.Run("no tracker over an unreadable one is not called none", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "", "kind:feature")
		fixture.tracker(104, "## Status\nActive")
		snapshot := collectUnit(t, fixture, 10)
		if report := RenderUnitReport(snapshot); !strings.Contains(report, "## Open Trackers Listing #10\n\nNone in the retained evidence.\n") {
			t.Errorf("report = %s", report)
		}
	})
}

func TestUnitContractIssueLeavesTheConflictSetUnknown(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "1B", contractLabel)
	fixture.dependencies(10, "- `exclusive-with` #11")
	fixture.issue(11, "1B", contractLabel)
	fixture.claim(11, 1100, time.Hour, "feat/eleven")
	// An unassessed contract unit the declared set does not hold.
	fixture.issue(12, "1B", contractLabel)
	directory := t.TempDir()
	exit, err := RunUnit(context.Background(), unitConfig(directory, 10), fixture.runner(t), fixedClock)
	if err != nil || exit != 2 {
		t.Fatalf("exit=%d error=%v", exit, err)
	}
	snapshot := assertIncompleteUnitReport(t, directory)
	if got := unknownCodes(snapshot); got != "contract-conflict-set(issue #10)" {
		t.Fatalf("unknowns = %s", got)
	}
	detail := snapshot.Unknowns[0].Detail
	for _, want := range []string{"contract conflict set", "the cap", "`trackercollect contracts`"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q: %s", want, detail)
		}
	}
	// The declared set is still reported, and its members were read.
	if got := memberStates(snapshot); got != "#10=idle(assigned) #11=active-lease(forward)" {
		t.Errorf("members = %s", got)
	}

	t.Run("truncated labels leave the classification unknown", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "", "kind:feature")
		fixture.issues[0]["labels"].(map[string]any)["pageInfo"] = morePages()
		snapshot := collectUnit(t, fixture, 10)
		if got := unknownCodes(snapshot); got != "label-truncation(issue #10 labels)" {
			t.Errorf("unknowns = %s", got)
		}
	})
}

func TestUnitExitCodes(t *testing.T) {
	artifacts := func(t *testing.T, directory string, want bool) {
		t.Helper()
		for _, name := range []string{"unit.json", "unit.md"} {
			_, err := os.Stat(filepath.Join(directory, name))
			if (err == nil) != want {
				t.Errorf("%s present=%v, want %v", name, err == nil, want)
			}
		}
	}
	run := func(t *testing.T, config UnitConfig, runner *fixtureRunner, wantExit int) {
		t.Helper()
		exit, err := RunUnit(context.Background(), config, runner, fixedClock)
		if exit != wantExit || (err != nil) != (wantExit == 1) {
			t.Fatalf("exit=%d error=%v, want exit %d", exit, err, wantExit)
		}
		artifacts(t, config.OutputDir, wantExit != 1)
	}
	typed := func() *contractFixture {
		fixture := newContractFixture()
		fixture.issue(10, "", "kind:feature")
		fixture.dependencies(10, "- `starts-after` #5")
		return fixture
	}
	t.Run("0 when every read finished and every line is typed", func(t *testing.T) {
		directory := t.TempDir()
		run(t, unitConfig(directory, 10), typed().runner(t), 0)
		raw, _ := os.ReadFile(filepath.Join(directory, "unit.md"))
		for _, want := range []string{"## UNKNOWN\n\nNone.\n", "## UNKNOWN Relationships\n\nNone.\n", "- `starts-after` #5\n  - `` - `starts-after` #5 ``\n", "- #10 Unit 10: `idle` (the assigned issue), milestone: none\n"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("report lacks %q:\n%s", want, raw)
			}
		}
	})
	t.Run("3 when a line of the issue is an UNKNOWN relationship", func(t *testing.T) {
		fixture := typed()
		fixture.dependencies(10, "- `starts-after` #5", "Refs #6.")
		run(t, unitConfig(t.TempDir(), 10), fixture.runner(t), 3)
	})
	t.Run("3 when a line elsewhere is an UNKNOWN relationship", func(t *testing.T) {
		fixture := typed()
		fixture.issue(11, "", contractLabel)
		fixture.dependencies(11, "- exclusive-with every other open `kind:contract` unit")
		run(t, unitConfig(t.TempDir(), 10), fixture.runner(t), 3)
	})
	t.Run("2 for any other UNKNOWN entry, even beside an UNKNOWN relationship", func(t *testing.T) {
		fixture := typed()
		fixture.dependencies(10, "Refs #6.")
		runner := fixture.runner(t)
		delete(runner.responses, "IssueComments:10:")
		directory := t.TempDir()
		run(t, unitConfig(directory, 10), runner, 2)
		assertIncompleteUnitReport(t, directory)
	})
	t.Run("1 when the open-issue inventory read fails", func(t *testing.T) {
		runner := typed().runner(t)
		delete(runner.responses, "OpenIssues::")
		run(t, unitConfig(t.TempDir(), 10), runner, 1)
	})
	t.Run("1 when the issue is closed or missing", func(t *testing.T) {
		directory := t.TempDir()
		exit, err := RunUnit(context.Background(), unitConfig(directory, 99), typed().runner(t), fixedClock)
		if exit != 1 || err == nil || !strings.Contains(err.Error(), "issue #99 is not an open issue") {
			t.Fatalf("exit=%d error=%v", exit, err)
		}
		artifacts(t, directory, false)
	})
	t.Run("1 when a truncated inventory does not hold the issue", func(t *testing.T) {
		runner := typed().runner(t)
		setFixturePageInfo(t, runner, "OpenIssues::", []string{"data", "repository", "issues"}, morePages())
		config := unitConfig(t.TempDir(), 99)
		config.PageCap = 1
		exit, err := RunUnit(context.Background(), config, runner, fixedClock)
		if exit != 1 || err == nil || !strings.Contains(err.Error(), "stopped at the page cap") {
			t.Fatalf("exit=%d error=%v", exit, err)
		}
		artifacts(t, config.OutputDir, false)
	})
	t.Run("1 when the issue number does not fit the forge's integer", func(t *testing.T) {
		for _, number := range []int{0, -1, MaxGraphQLInt + 1} {
			runner := typed().runner(t)
			run(t, unitConfig(t.TempDir(), number), runner, 1)
			if len(runner.queries) != 0 {
				t.Errorf("issue %d reached the forge", number)
			}
		}
	})
	t.Run("1 when the output directory is missing", func(t *testing.T) {
		exit, err := RunUnit(context.Background(), unitConfig("", 10), typed().runner(t), fixedClock)
		if err == nil || exit != 1 {
			t.Fatalf("exit=%d error=%v", exit, err)
		}
	})
}

func TestUnitModeReadsOnlyQueriesAndOnlyItsMembers(t *testing.T) {
	fixture := incompleteUnitFixture()
	fixture.issue(12, "", "kind:feature")
	fixture.claim(12, 1200, time.Hour, "feat/twelve")
	runner := fixture.runner(t)
	// A read of an issue outside the set would fail and surface as UNKNOWN.
	delete(runner.responses, "IssueComments:12:")
	snapshot, err := CollectUnit(context.Background(), unitConfig("", 10), runner, fixedClock)
	if err != nil || !snapshot.Complete {
		t.Fatalf("error=%v unknowns=%s", err, unknownCodes(snapshot))
	}
	// Open issues, open PRs, one closing-issue read, and comments for the
	// two open members. The not-open partner is never read.
	if len(runner.queries) != 5 {
		t.Fatalf("queries = %d", len(runner.queries))
	}
	for _, query := range runner.queries {
		if !strings.HasPrefix(strings.TrimSpace(query), "query ") {
			t.Fatalf("non-query document: %s", query)
		}
	}
	// A closed issue never declares or joins: the inventory the reverse scan
	// and the member lookup read asks the forge for open issues only.
	if !strings.Contains(runner.queries[0], "issues(first: $pageSize, after: $cursor, states: OPEN, orderBy") {
		t.Errorf("open-issue inventory query = %s", runner.queries[0])
	}
}

func TestUnitReportQuotesIssueTextAndStatesNoVerdict(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "", "kind:feature")
	fixture.dependencies(10,
		"- `starts-after` #5",
		"- see ``` and `` here, startable and clear to claim",
		"## not a heading in the report",
	)
	report := RenderUnitReport(collectUnit(t, fixture, 10))
	for _, want := range []string{
		"  - ```` - see ``` and `` here, startable and clear to claim ````\n",
		"- **UNKNOWN relationship** on #10: " + reasonUntyped + ".\n",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	// The report's own words never decide; an issue's quoted text may say
	// anything, so it is cut before the check.
	var own []string
	for _, line := range strings.Split(report, "\n") {
		if !strings.HasPrefix(line, "  - `") {
			own = append(own, strings.ToLower(line))
		}
	}
	for _, forbidden := range []string{"startable", "may claim", "may start", "clear", "safe to", "no active claim", "unclaimed"} {
		if strings.Contains(strings.Join(own, "\n"), forbidden) {
			t.Errorf("report states a verdict (%q):\n%s", forbidden, report)
		}
	}
}
