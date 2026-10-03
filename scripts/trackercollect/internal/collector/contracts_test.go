package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// contractFixture builds the four reads the contracts mode makes: the open
// issue inventory, the open PR list, each PR's closing issues, and each
// contract issue's comments.
type contractFixture struct {
	issues   []map[string]any
	prs      []map[string]any
	closing  map[int][]map[string]any
	comments map[int][]map[string]any
}

func newContractFixture() *contractFixture {
	return &contractFixture{closing: map[int][]map[string]any{}, comments: map[int][]map[string]any{}}
}

func lastPage() map[string]any { return map[string]any{"hasNextPage": false, "endCursor": nil} }

func morePages() map[string]any { return map[string]any{"hasNextPage": true, "endCursor": "next"} }

func fixtureIssueNode(number int, body string) map[string]any {
	return map[string]any{
		"id": fmt.Sprintf("I_%d", number), "databaseId": number, "number": number,
		"title": fmt.Sprintf("Unit %d", number), "state": "OPEN",
		"updatedAt": "2026-08-25T09:00:00Z", "body": body,
	}
}

func (f *contractFixture) issue(number int, milestone string, labels ...string) {
	nodes := make([]any, 0, len(labels))
	for _, label := range labels {
		nodes = append(nodes, map[string]any{"name": label})
	}
	node := fixtureIssueNode(number, "### Dependencies\nnone")
	node["labels"] = map[string]any{"nodes": nodes, "pageInfo": lastPage()}
	node["milestone"] = nil
	if milestone != "" {
		node["milestone"] = map[string]any{"title": milestone}
	}
	f.issues = append(f.issues, node)
}

func (f *contractFixture) tracker(number int, body string) {
	f.issue(number, "", trackerLabel)
	f.issues[len(f.issues)-1]["body"] = body
}

func (f *contractFixture) pullRequest(number int, branch, repository string, closes ...int) {
	f.prs = append(f.prs, map[string]any{
		"id": fmt.Sprintf("PR_%d", number), "databaseId": number, "number": number,
		"title": fmt.Sprintf("PR %d", number), "state": "OPEN", "merged": false,
		"updatedAt": "2026-08-25T09:00:00Z", "headRefName": branch, "baseRefName": "main",
		"isDraft": false, "body": "", "headRepository": map[string]any{"nameWithOwner": repository},
	})
	f.closing[number] = []map[string]any{}
	for _, issue := range closes {
		f.closing[number] = append(f.closing[number], fixtureIssueNode(issue, ""))
	}
}

func (f *contractFixture) comment(issue int, id int64, age time.Duration, body string) {
	created := fixedClock().Add(-age).Format(time.RFC3339)
	f.comments[issue] = append(f.comments[issue], map[string]any{
		"id": fmt.Sprintf("IC_%d", id), "databaseId": id, "body": body,
		"createdAt": created, "updatedAt": created, "authorAssociation": "MEMBER",
	})
}

// postedBy sets the author association of the issue's latest comment.
func (f *contractFixture) postedBy(issue int, association string) {
	f.comments[issue][len(f.comments[issue])-1]["authorAssociation"] = association
}

func (f *contractFixture) claim(issue int, id int64, age time.Duration, branch string) {
	f.comment(issue, id, age, claimMarker+"\nClaim: "+branch)
}

func (f *contractFixture) release(issue int, id int64, age time.Duration, branch string, claimID int64) {
	f.comment(issue, id, age, fmt.Sprintf("%s\nRelease: %s\nReleases-claim: %d", releaseMarker, branch, claimID))
}

func (f *contractFixture) reservation(issue int, id int64, age time.Duration) {
	f.comment(issue, id, age, fmt.Sprintf("%s\nPlan: #%d", reservationMarker, issue))
}

func (f *contractFixture) runner(t *testing.T) *fixtureRunner {
	t.Helper()
	responses := map[string]json.RawMessage{}
	put := func(key string, value any) {
		raw, err := json.Marshal(map[string]any{"data": map[string]any{"repository": value}})
		if err != nil {
			t.Fatal(err)
		}
		responses[key] = raw
	}
	nodes := func(items []map[string]any) []any {
		result := make([]any, 0, len(items))
		for _, item := range items {
			result = append(result, item)
		}
		return result
	}
	put("OpenIssues::", map[string]any{"issues": map[string]any{"nodes": nodes(f.issues), "pageInfo": lastPage()}})
	put("OpenPullRequests::", map[string]any{"pullRequests": map[string]any{"nodes": nodes(f.prs), "pageInfo": lastPage()}})
	for _, pr := range f.prs {
		number := pr["number"].(int)
		put(fmt.Sprintf("PullRequestClosingIssues:%d:", number), map[string]any{"pullRequest": map[string]any{
			"id": pr["id"], "databaseId": number, "number": number, "updatedAt": pr["updatedAt"],
			"closingIssuesReferences": map[string]any{"nodes": nodes(f.closing[number]), "pageInfo": lastPage()},
		}})
	}
	for _, issue := range f.issues {
		number := issue["number"].(int)
		put(fmt.Sprintf("IssueComments:%d:", number), map[string]any{"issue": map[string]any{
			"id": issue["id"], "databaseId": number, "number": number, "updatedAt": issue["updatedAt"],
			"comments": map[string]any{"nodes": nodes(f.comments[number]), "pageInfo": lastPage()},
		}})
	}
	return &fixtureRunner{responses: responses}
}

const canonicalRepository = "freeside-ai/freeside"

func contractsConfig(output string) ContractsConfig {
	return ContractsConfig{
		Repository: Repository{Host: "github.com", Owner: "freeside-ai", Name: "freeside"},
		OutputDir:  output,
		PageSize:   100,
		PageCap:    10,
	}
}

func collectContracts(t *testing.T, fixture *contractFixture) ContractsSnapshot {
	t.Helper()
	snapshot, err := CollectContracts(context.Background(), contractsConfig(""), fixture.runner(t), fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func contractUnit(t *testing.T, snapshot ContractsSnapshot, number int) ContractUnit {
	t.Helper()
	for _, unit := range snapshot.Contracts {
		if unit.Number == number {
			return unit
		}
	}
	t.Fatalf("contract #%d not in snapshot: %#v", number, snapshot.Contracts)
	return ContractUnit{}
}

func activeCount(t *testing.T, snapshot ContractsSnapshot) int {
	t.Helper()
	if !snapshot.Complete || snapshot.ActiveCount == nil || snapshot.ReservedCount == nil {
		t.Fatalf("snapshot states no counts: complete=%v ambiguities=%#v", snapshot.Complete, snapshot.Ambiguities)
	}
	return *snapshot.ActiveCount
}

func findingCodes(snapshot ContractsSnapshot) string {
	parts := make([]string, 0, len(snapshot.Findings))
	for _, finding := range snapshot.Findings {
		parts = append(parts, fmt.Sprintf("%s#%d", finding.Code, finding.Number))
	}
	return strings.Join(parts, ",")
}

func TestContractActiveLease(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
	fixture.claim(10, 1000, 48*time.Hour-time.Second, "feat/ten")
	fixture.issue(11, "Wave 8", contractLabel)
	fixture.claim(11, 1100, time.Hour, "feat/eleven")
	snapshot := collectContracts(t, fixture)

	unit := contractUnit(t, snapshot, 10)
	if unit.State != ContractActiveLease || unit.OrderingClaim == nil || unit.OrderingClaim.CommentID != 1000 || unit.Claims[0].Status != ClaimLive {
		t.Fatalf("unit = %#v", unit)
	}
	// The label is expected on an active lease: #10 carries it, #11 lacks it.
	if activeCount(t, snapshot) != 2 || findingCodes(snapshot) != "missing-label#11" {
		t.Fatalf("active=%d findings=%s", *snapshot.ActiveCount, findingCodes(snapshot))
	}
}

func TestContractExpiredLease(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
	fixture.claim(10, 1000, 48*time.Hour, "feat/ten")
	// An open PR from the claimed branch that lacks the close keyword is
	// named and keeps no lease alive.
	fixture.pullRequest(400, "feat/ten", canonicalRepository)
	snapshot := collectContracts(t, fixture)

	unit := contractUnit(t, snapshot, 10)
	if unit.State != ContractExpired || unit.OrderingClaim != nil || unit.Claims[0].Status != ClaimExpired {
		t.Fatalf("unit = %#v", unit)
	}
	if len(unit.Claims[0].OtherPullRequests) != 1 || unit.Claims[0].OtherPullRequests[0] != 400 || len(unit.Claims[0].ClosingPullRequests) != 0 {
		t.Fatalf("claim = %#v", unit.Claims[0])
	}
	if activeCount(t, snapshot) != 0 || findingCodes(snapshot) != "stale-label#10" {
		t.Fatalf("active=%d findings=%s", *snapshot.ActiveCount, findingCodes(snapshot))
	}
	if report := RenderContractsReport(snapshot); !strings.Contains(report, "open PR from the branch without the close keyword: #400") {
		t.Fatalf("report does not name the unlinked PR: %s", report)
	}
}

func TestContractPullRequestBackedClaim(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
	// The later claim is PR-backed; the earlier one expired. The backed
	// claim's own created_at and comment ID stay the ordering key.
	fixture.claim(10, 900, 40*24*time.Hour, "feat/abandoned")
	fixture.claim(10, 1000, 30*24*time.Hour, "feat/ten")
	fixture.pullRequest(400, "feat/ten", "FREESIDE-AI/FREESIDE", 10)
	snapshot := collectContracts(t, fixture)

	unit := contractUnit(t, snapshot, 10)
	if unit.State != ContractActivePR || unit.OrderingClaim == nil {
		t.Fatalf("unit = %#v", unit)
	}
	wantCreated := fixedClock().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	if unit.OrderingClaim.CommentID != 1000 || unit.OrderingClaim.CreatedAt != wantCreated || unit.OrderingClaim.ClosingPullRequests[0] != 400 {
		t.Fatalf("ordering claim = %#v", unit.OrderingClaim)
	}
	if len(unit.LegacyPullRequests) != 0 || activeCount(t, snapshot) != 1 || len(snapshot.Findings) != 0 {
		t.Fatalf("unit=%#v findings=%s", unit, findingCodes(snapshot))
	}

	t.Run("a fork PR from the same branch name does not back the claim", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel)
		fixture.claim(10, 1000, 30*24*time.Hour, "feat/ten")
		fixture.pullRequest(400, "feat/ten", "someone/freeside", 10)
		unit := contractUnit(t, collectContracts(t, fixture), 10)
		// The fork PR still closes the issue, so the unit is active, but
		// through no claim comment: the claim itself has expired.
		if unit.State != ContractActivePRLegacy || unit.OrderingClaim != nil || unit.Claims[0].Status != ClaimExpired || len(unit.Claims[0].ClosingPullRequests) != 0 {
			t.Fatalf("unit = %#v", unit)
		}
	})
	t.Run("a stacked PR is read by the close keyword in its body", func(t *testing.T) {
		// The forge lists no closing issue for a PR based on another branch.
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
		fixture.claim(10, 1000, 30*24*time.Hour, "feat/ten")
		fixture.pullRequest(400, "feat/ten", canonicalRepository)
		fixture.prs[0]["baseRefName"] = "feat/base"
		fixture.prs[0]["body"] = "## Why\r\n\r\nCloses #10\r\n"
		fixture.issue(11, "Wave 8", contractLabel)
		fixture.claim(11, 1100, 30*24*time.Hour, "feat/eleven")
		fixture.pullRequest(401, "feat/eleven", canonicalRepository)
		fixture.prs[1]["body"] = "Refs #11. Closes other/repo#11.\n```\nCloses #11\n```"
		snapshot := collectContracts(t, fixture)
		if unit := contractUnit(t, snapshot, 10); unit.State != ContractActivePR || unit.Claims[0].ClosingPullRequests[0] != 400 {
			t.Fatalf("unit = %#v", unit)
		}
		if unit := contractUnit(t, snapshot, 11); unit.State != ContractExpired || unit.Claims[0].OtherPullRequests[0] != 401 {
			t.Fatalf("unit = %#v", unit)
		}
	})
	t.Run("a close keyword quoted in a code span closes nothing", func(t *testing.T) {
		state := func(body string) string {
			t.Helper()
			fixture := newContractFixture()
			fixture.issue(10, "Wave 8", contractLabel)
			fixture.claim(10, 1000, 30*24*time.Hour, "feat/ten")
			fixture.pullRequest(400, "feat/ten", canonicalRepository)
			fixture.prs[0]["baseRefName"] = "feat/base"
			fixture.prs[0]["body"] = body
			return contractUnit(t, collectContracts(t, fixture), 10).State
		}
		for _, body := range []string{
			"Refs #10. Write `Closes #10` to close it.",
			"Refs #10. Write ``Closes #10`` to close it.",
			"Use `x` or `Fixes #10`, never both.",
			"Closes `#10` is the form.",
		} {
			if got := state(body); got != ContractExpired {
				t.Errorf("body %q: state = %s, want %s", body, got, ContractExpired)
			}
		}
		for _, body := range []string{
			"Closes #10",
			"See `main.go`. Closes #10, as `Refs` would not.",
			// A stray backtick on another line opens no span.
			"A stray ` here.\nCloses #10\nAnother ` there.",
			// Runs of different lengths don't pair.
			"A `` run. Closes #10 ` here.",
			// An escaped backtick is a literal one.
			"\\`Closes #10\\`",
		} {
			if got := state(body); got != ContractActivePR {
				t.Errorf("body %q: state = %s, want %s", body, got, ContractActivePR)
			}
		}
	})
	t.Run("an earlier live claim keeps the ordering key", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
		fixture.claim(10, 900, 2*time.Hour, "feat/first")
		fixture.claim(10, 1000, time.Hour, "feat/ten")
		fixture.pullRequest(400, "feat/ten", canonicalRepository, 10)
		unit := contractUnit(t, collectContracts(t, fixture), 10)
		if unit.State != ContractActivePR || unit.OrderingClaim == nil || unit.OrderingClaim.CommentID != 900 {
			t.Fatalf("unit = %#v", unit)
		}
	})
	t.Run("a release beats the claim's open PR", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
		fixture.claim(10, 1000, 3*time.Hour, "feat/ten")
		fixture.release(10, 1001, time.Hour, "feat/ten", 1000)
		fixture.pullRequest(400, "feat/ten", canonicalRepository, 10)
		unit := contractUnit(t, collectContracts(t, fixture), 10)
		// The claim is released; the open PR would still close the issue.
		if unit.Claims[0].Status != ClaimReleased || unit.OrderingClaim != nil || unit.State != ContractActivePRLegacy || unit.LegacyPullRequests[0] != 400 {
			t.Fatalf("unit = %#v", unit)
		}
	})
}

func TestContractRelease(t *testing.T) {
	t.Run("a release naming the claim comment makes it inactive", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel)
		fixture.claim(10, 1000, 2*time.Hour, "feat/ten")
		fixture.release(10, 1001, time.Hour, "feat/ten", 1000)
		snapshot := collectContracts(t, fixture)
		unit := contractUnit(t, snapshot, 10)
		if unit.State != ContractIdle || unit.Claims[0].Status != ClaimReleased || activeCount(t, snapshot) != 0 || len(snapshot.Findings) != 0 {
			t.Fatalf("unit=%#v findings=%s", unit, findingCodes(snapshot))
		}
	})
	t.Run("a release naming a different ID releases nothing", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
		fixture.claim(10, 1000, 2*time.Hour, "feat/ten")
		fixture.release(10, 1001, time.Hour, "feat/ten", 999)
		unit := contractUnit(t, collectContracts(t, fixture), 10)
		if unit.State != ContractActiveLease || unit.Claims[0].Status != ClaimLive {
			t.Fatalf("unit = %#v", unit)
		}
	})
	t.Run("a release posted on another issue releases nothing", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
		fixture.claim(10, 1000, 2*time.Hour, "feat/ten")
		fixture.issue(11, "Wave 8", contractLabel)
		fixture.release(11, 1001, time.Hour, "feat/ten", 1000)
		snapshot := collectContracts(t, fixture)
		if unit := contractUnit(t, snapshot, 10); unit.State != ContractActiveLease {
			t.Fatalf("unit = %#v", unit)
		}
		if unit := contractUnit(t, snapshot, 11); unit.State != ContractIdle {
			t.Fatalf("unit = %#v", unit)
		}
	})
}

func TestContractPlanningReservation(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "Wave 8", contractLabel)
	fixture.reservation(10, 1000, 48*time.Hour-time.Second)
	fixture.issue(11, "Wave 8", contractLabel)
	fixture.reservation(11, 1100, 48*time.Hour)
	snapshot := collectContracts(t, fixture)

	if unit := contractUnit(t, snapshot, 10); unit.State != ContractReserved || !unit.Reservations[0].Active {
		t.Fatalf("unit = %#v", unit)
	}
	if unit := contractUnit(t, snapshot, 11); unit.State != ContractIdle || unit.Reservations[0].Active {
		t.Fatalf("unit = %#v", unit)
	}
	// A reservation expects no label and counts as reserved, never active.
	if activeCount(t, snapshot) != 0 || *snapshot.ReservedCount != 1 || len(snapshot.Findings) != 0 {
		t.Fatalf("active=%d reserved=%d findings=%s", *snapshot.ActiveCount, *snapshot.ReservedCount, findingCodes(snapshot))
	}
	report := RenderContractsReport(snapshot)
	if !strings.Contains(report, "Active contract implementations: **0** against a cap of **2**.") || !strings.Contains(report, "Reserved for planning: **1** (not counted as active).") {
		t.Fatalf("report = %s", report)
	}
}

func TestContractLegacyPullRequestClaim(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
	fixture.pullRequest(400, "feat/legacy", canonicalRepository, 10)
	fixture.issue(11, "Wave 8", contractLabel)
	fixture.pullRequest(401, "feat/other-legacy", canonicalRepository, 11)
	snapshot := collectContracts(t, fixture)

	unit := contractUnit(t, snapshot, 10)
	if unit.State != ContractActivePRLegacy || len(unit.Claims) != 0 || unit.OrderingClaim != nil || unit.LegacyPullRequests[0] != 400 {
		t.Fatalf("unit = %#v", unit)
	}
	if activeCount(t, snapshot) != 2 || findingCodes(snapshot) != "missing-label#11" {
		t.Fatalf("active=%d findings=%s", *snapshot.ActiveCount, findingCodes(snapshot))
	}
}

func TestContractLegacyPullRequestBesideAnExpiredClaim(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
	fixture.claim(10, 1000, 72*time.Hour, "feat/abandoned")
	fixture.pullRequest(400, "feat/successor", canonicalRepository, 10)
	unit := contractUnit(t, collectContracts(t, fixture), 10)
	if unit.State != ContractActivePRLegacy || unit.Claims[0].Status != ClaimExpired || unit.LegacyPullRequests[0] != 400 {
		t.Fatalf("unit = %#v", unit)
	}
}

func TestContractClosingIssueInAnotherRepositoryIsNotThisUnit(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "Wave 8", contractLabel)
	fixture.pullRequest(400, "feat/elsewhere", canonicalRepository)
	foreign := fixtureIssueNode(10, "")
	foreign["id"] = "I_other_repository_10"
	fixture.closing[400] = []map[string]any{foreign}
	snapshot := collectContracts(t, fixture)
	if unit := contractUnit(t, snapshot, 10); unit.State != ContractIdle || len(unit.LegacyPullRequests) != 0 || len(snapshot.Findings) != 0 {
		t.Fatalf("unit=%#v findings=%s", unit, findingCodes(snapshot))
	}
}

func TestContractDeferralHandling(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "", contractLabel, deferralLabel)
	fixture.claim(10, 1000, 72*time.Hour, "feat/old")
	fixture.issue(11, "Wave 8", contractLabel, deferralLabel)
	fixture.issue(12, "", contractLabel, deferralLabel)
	fixture.claim(12, 1200, time.Hour, "feat/twelve")
	fixture.issue(13, "", contractLabel, deferralLabel)
	fixture.reservation(13, 1300, time.Hour)
	fixture.issue(14, "", contractLabel)
	snapshot := collectContracts(t, fixture)

	want := map[int]string{
		10: ContractDormant,     // unmilestoned, only an expired claim
		11: ContractIdle,        // milestoned: treated like any contract
		12: ContractActiveLease, // actively claimed: treated like any contract
		13: ContractReserved,
		14: ContractIdle, // unmilestoned but not a deferral
	}
	for number, state := range want {
		if unit := contractUnit(t, snapshot, number); unit.State != state {
			t.Errorf("#%d state = %s, want %s", number, unit.State, state)
		}
	}
	if activeCount(t, snapshot) != 1 || *snapshot.ReservedCount != 1 || findingCodes(snapshot) != "missing-label#12" {
		t.Fatalf("active=%d reserved=%d findings=%s", *snapshot.ActiveCount, *snapshot.ReservedCount, findingCodes(snapshot))
	}
}

func TestContractLabelReconciliation(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
	fixture.claim(10, 1000, time.Hour, "feat/ten")
	fixture.issue(11, "Wave 8", contractLabel)
	fixture.claim(11, 1100, time.Hour, "feat/eleven")
	fixture.issue(12, "Wave 8", contractLabel, contractActiveLabel)
	fixture.issue(13, "", contractLabel, deferralLabel, contractActiveLabel)
	fixture.issue(14, "Wave 8", contractLabel, contractActiveLabel)
	fixture.reservation(14, 1400, time.Hour)
	fixture.issue(15, "Wave 8", "kind:feature", contractActiveLabel)
	fixture.tracker(100, "## Status\nActive\n\n## Units\n- [ ] #10\n- [ ] #11\n- [ ] #11\n\n## Notes\n- [ ] #12")
	fixture.tracker(101, "## Units\n- [x] #10")
	config := contractsConfig("")
	config.Cap = 3
	snapshot, err := CollectContracts(context.Background(), config, fixture.runner(t), fixedClock)
	if err != nil {
		t.Fatal(err)
	}

	if got := findingCodes(snapshot); got != "missing-label#11,stale-label#12,stale-label#13,stale-label#14" {
		t.Fatalf("findings = %s", got)
	}
	if len(snapshot.Contracts) != 5 || activeCount(t, snapshot) != 2 || snapshot.Cap != 3 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if trackers := contractUnit(t, snapshot, 10).Trackers; len(trackers) != 2 || trackers[0] != 100 || trackers[1] != 101 {
		t.Fatalf("#10 trackers = %v", trackers)
	}
	// Notes may cite a unit without listing it: #12 is on no tracker.
	if trackers := contractUnit(t, snapshot, 12).Trackers; len(trackers) != 0 {
		t.Fatalf("#12 trackers = %v", trackers)
	}
	report := RenderContractsReport(snapshot)
	for _, want := range []string{
		"Active contract implementations: **2** against a cap of **3**.",
		"- #10 Unit 10: `active-lease`, label present, trackers: #100, #101, milestone: Wave 8",
		"- #11 Unit 11: `active-lease`, label absent, trackers: #100, milestone: Wave 8",
		"Findings: **4**.",
		"- **missing-label** #11 (`active-lease`)",
		"- **stale-label** #14 (`reserved`)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "exceeds the cap") {
		t.Fatalf("report claims an over-cap count: %s", report)
	}

	t.Run("an active count past the cap is stated", func(t *testing.T) {
		config.Cap = 1
		snapshot, err := CollectContracts(context.Background(), config, fixture.runner(t), fixedClock)
		if err != nil {
			t.Fatal(err)
		}
		if report := RenderContractsReport(snapshot); !strings.Contains(report, "**2** against a cap of **1**.\n") || !strings.Contains(report, "The active count exceeds the cap.") {
			t.Fatalf("report = %s", report)
		}
	})
}

// assertNoVerdict fails when a report over incomplete evidence prints an
// active count, a finding count, or a clean verdict.
func assertNoVerdict(t *testing.T, directory string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(directory, "contracts.md"))
	if err != nil {
		t.Fatal(err)
	}
	report := string(raw)
	for _, forbidden := range []string{"Findings: **", "matches claim state", "Active contract implementations: **", "No contract unit holds"} {
		if strings.Contains(report, forbidden) {
			t.Errorf("report over incomplete evidence prints %q:\n%s", forbidden, report)
		}
	}
	if !strings.Contains(report, "see **AMBIGUOUS**") || !strings.Contains(report, "No finding count or verdict") {
		t.Errorf("report does not mark the evidence incomplete:\n%s", report)
	}
	var snapshot ContractsSnapshot
	rawJSON, err := os.ReadFile(filepath.Join(directory, "contracts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawJSON, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Complete || len(snapshot.Ambiguities) == 0 || snapshot.ActiveCount != nil || snapshot.ReservedCount != nil {
		t.Errorf("snapshot complete=%v active=%v reserved=%v ambiguities=%#v", snapshot.Complete, snapshot.ActiveCount, snapshot.ReservedCount, snapshot.Ambiguities)
	}
}

func setFixturePageInfo(t *testing.T, runner *fixtureRunner, key string, path []string, pageInfo map[string]any) {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(runner.responses[key], &response); err != nil {
		t.Fatal(err)
	}
	current := response
	for _, part := range path {
		current = current[part].(map[string]any)
	}
	current["pageInfo"] = pageInfo
	runner.responses[key], _ = json.Marshal(response)
}

func TestContractIncompleteReadsAreAmbiguous(t *testing.T) {
	// One active unit without its label: complete evidence would report one
	// finding and exit 3. Every incomplete read must exit 2 instead and
	// state no count or verdict.
	base := func() *contractFixture {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel)
		fixture.claim(10, 1000, time.Hour, "feat/ten")
		fixture.pullRequest(400, "feat/unrelated", canonicalRepository)
		return fixture
	}
	run := func(t *testing.T, runner *fixtureRunner, code, subject string) {
		t.Helper()
		directory := t.TempDir()
		config := contractsConfig(directory)
		config.PageCap = 1
		exit, err := RunContracts(context.Background(), config, runner, fixedClock)
		if err != nil || exit != 2 {
			t.Fatalf("exit=%d error=%v", exit, err)
		}
		assertNoVerdict(t, directory)
		raw, _ := os.ReadFile(filepath.Join(directory, "contracts.md"))
		if want := fmt.Sprintf("- **%s** (%s)", code, subject); !strings.Contains(string(raw), want) {
			t.Fatalf("report lacks %q:\n%s", want, raw)
		}
	}
	t.Run("complete evidence reports the finding", func(t *testing.T) {
		exit, err := RunContracts(context.Background(), contractsConfig(t.TempDir()), base().runner(t), fixedClock)
		if err != nil || exit != 3 {
			t.Fatalf("exit=%d error=%v", exit, err)
		}
	})
	t.Run("open-issue inventory page cap", func(t *testing.T) {
		runner := base().runner(t)
		setFixturePageInfo(t, runner, "OpenIssues::", []string{"data", "repository", "issues"}, morePages())
		run(t, runner, "page-cap-truncation", "open issues")
	})
	t.Run("open-PR list page cap", func(t *testing.T) {
		runner := base().runner(t)
		setFixturePageInfo(t, runner, "OpenPullRequests::", []string{"data", "repository", "pullRequests"}, morePages())
		run(t, runner, "page-cap-truncation", "open pull requests")
	})
	t.Run("PR closing-issue page cap", func(t *testing.T) {
		runner := base().runner(t)
		setFixturePageInfo(t, runner, "PullRequestClosingIssues:400:", []string{"data", "repository", "pullRequest", "closingIssuesReferences"}, morePages())
		run(t, runner, "page-cap-truncation", "pull request #400 linked issues")
	})
	t.Run("contract issue comments page cap", func(t *testing.T) {
		runner := base().runner(t)
		setFixturePageInfo(t, runner, "IssueComments:10:", []string{"data", "repository", "issue", "comments"}, morePages())
		run(t, runner, "page-cap-truncation", "issue #10 comments")
	})
	t.Run("malformed marker comment", func(t *testing.T) {
		fixture := base()
		fixture.comment(10, 1001, time.Hour, claimMarker+"\nClaim: feat/one\nClaim: feat/two")
		run(t, fixture.runner(t), "malformed-marker-comment", "comment 1001")
	})
	t.Run("issue label page overflow", func(t *testing.T) {
		fixture := base()
		fixture.issue(11, "", "kind:feature")
		fixture.issues[1]["labels"].(map[string]any)["pageInfo"] = morePages()
		run(t, fixture.runner(t), "label-truncation", "issue #11 labels")
	})
	t.Run("tracker entry with an invalid issue number", func(t *testing.T) {
		fixture := base()
		fixture.tracker(100, "## Units\n- [ ] #10\n- [ ] #0")
		run(t, fixture.runner(t), "malformed-tracker-entry", "issue #100")
	})
	t.Run("tracker without one Units section", func(t *testing.T) {
		fixture := base()
		fixture.tracker(100, "## Status\nActive\n\n- [ ] #10")
		run(t, fixture.runner(t), "tracker-structure", "issue #100")
	})
}

func TestContractMarkerFromOutsideTheTrustBoundaryIsAmbiguous(t *testing.T) {
	// Each fixture has complete evidence and a verdict when a collaborator
	// posts the marker. Posted by anyone else, the marker is not counted
	// and the report states no verdict.
	run := func(t *testing.T, fixture *contractFixture, wantExit int) ContractsSnapshot {
		t.Helper()
		directory := t.TempDir()
		exit, err := RunContracts(context.Background(), contractsConfig(directory), fixture.runner(t), fixedClock)
		if err != nil || exit != wantExit {
			t.Fatalf("exit=%d error=%v, want exit %d", exit, err, wantExit)
		}
		if wantExit == 2 {
			assertNoVerdict(t, directory)
		}
		var snapshot ContractsSnapshot
		raw, err := os.ReadFile(filepath.Join(directory, "contracts.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	assertAmbiguity := func(t *testing.T, snapshot ContractsSnapshot, comment int64, detail string) {
		t.Helper()
		if len(snapshot.Ambiguities) != 1 {
			t.Fatalf("ambiguities = %#v", snapshot.Ambiguities)
		}
		got := snapshot.Ambiguities[0]
		if got.Code != "untrusted-marker-author" || got.Subject != fmt.Sprintf("comment %d", comment) || !strings.HasPrefix(got.Detail, detail) {
			t.Fatalf("ambiguity = %#v, want comment %d with detail %q", got, comment, detail)
		}
	}
	claimed := func(association string) *contractFixture {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel)
		fixture.claim(10, 1000, time.Hour, "feat/ten")
		fixture.postedBy(10, association)
		return fixture
	}

	for _, association := range []string{"OWNER", "MEMBER", "COLLABORATOR"} {
		t.Run("a claim by "+association+" counts", func(t *testing.T) {
			snapshot := run(t, claimed(association), 3)
			if unit := contractUnit(t, snapshot, 10); unit.State != ContractActiveLease || findingCodes(snapshot) != "missing-label#10" {
				t.Fatalf("unit=%#v findings=%s", unit, findingCodes(snapshot))
			}
		})
	}
	// UNLISTED stands for an association this tool does not know.
	for _, association := range []string{"CONTRIBUTOR", "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER", "MANNEQUIN", "NONE", "UNLISTED"} {
		t.Run("a claim by "+association+" is not counted", func(t *testing.T) {
			snapshot := run(t, claimed(association), 2)
			assertAmbiguity(t, snapshot, 1000, "claim marker on issue #10 has author association "+association+",")
			if unit := contractUnit(t, snapshot, 10); unit.State != ContractIdle || len(unit.Claims) != 0 {
				t.Fatalf("unit = %#v", unit)
			}
		})
	}
	t.Run("an outsider's reservation is not counted", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel)
		fixture.reservation(10, 1000, time.Hour)
		if unit := contractUnit(t, run(t, fixture, 0), 10); unit.State != ContractReserved {
			t.Fatalf("collaborator reservation: unit = %#v", unit)
		}
		fixture.postedBy(10, "NONE")
		snapshot := run(t, fixture, 2)
		assertAmbiguity(t, snapshot, 1000, "planning-reservation marker on issue #10 has author association NONE,")
		if unit := contractUnit(t, snapshot, 10); unit.State != ContractIdle || len(unit.Reservations) != 0 {
			t.Fatalf("unit = %#v", unit)
		}
	})
	t.Run("an outsider's release of a real claim releases nothing", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
		fixture.claim(10, 1000, 2*time.Hour, "feat/ten")
		fixture.release(10, 1001, time.Hour, "feat/ten", 1000)
		// From a collaborator the release stands, and the label is stale.
		if snapshot := run(t, fixture, 3); contractUnit(t, snapshot, 10).State != ContractIdle || findingCodes(snapshot) != "stale-label#10" {
			t.Fatalf("collaborator release: unit=%#v findings=%s", contractUnit(t, snapshot, 10), findingCodes(snapshot))
		}
		fixture.postedBy(10, "NONE")
		snapshot := run(t, fixture, 2)
		assertAmbiguity(t, snapshot, 1001, "release marker on issue #10 has author association NONE,")
		if unit := contractUnit(t, snapshot, 10); unit.State != ContractActiveLease || unit.Claims[0].Status != ClaimLive {
			t.Fatalf("unit = %#v", unit)
		}
	})
	t.Run("a comment without a marker is not judged", func(t *testing.T) {
		fixture := claimed("MEMBER")
		fixture.comment(10, 1001, time.Hour, "Is this unit still in progress?")
		fixture.postedBy(10, "NONE")
		if snapshot := run(t, fixture, 3); len(snapshot.Ambiguities) != 0 {
			t.Fatalf("ambiguities = %#v", snapshot.Ambiguities)
		}
	})
}

func TestContractScopeCollisionsAreNotThisReportsAmbiguity(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "Wave 8", contractLabel)
	fixture.pullRequest(400, "feat/unrelated", canonicalRepository)
	fixture.prs[0]["body"] = "Scope: docs/\nScope: scripts/"
	snapshot := collectContracts(t, fixture)
	if !snapshot.Complete || len(snapshot.Ambiguities) != 0 {
		t.Fatalf("ambiguities = %#v", snapshot.Ambiguities)
	}
}

func TestContractExitCodes(t *testing.T) {
	artifacts := func(t *testing.T, directory string, want bool) {
		t.Helper()
		for _, name := range []string{"contracts.json", "contracts.md"} {
			_, err := os.Stat(filepath.Join(directory, name))
			if (err == nil) != want {
				t.Errorf("%s present=%v, want %v", name, err == nil, want)
			}
		}
	}
	t.Run("0 with no findings and no ambiguities", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
		fixture.claim(10, 1000, time.Hour, "feat/ten")
		fixture.issue(11, "", contractLabel, deferralLabel)
		directory := t.TempDir()
		exit, err := RunContracts(context.Background(), contractsConfig(directory), fixture.runner(t), fixedClock)
		if err != nil || exit != 0 {
			t.Fatalf("exit=%d error=%v", exit, err)
		}
		artifacts(t, directory, true)
		raw, _ := os.ReadFile(filepath.Join(directory, "contracts.md"))
		if !strings.Contains(string(raw), "Findings: **0**.") || !strings.Contains(string(raw), "- #11 Unit 11: `dormant`") {
			t.Fatalf("report = %s", raw)
		}
	})
	t.Run("3 with findings and complete evidence", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
		directory := t.TempDir()
		exit, err := RunContracts(context.Background(), contractsConfig(directory), fixture.runner(t), fixedClock)
		if err != nil || exit != 3 {
			t.Fatalf("exit=%d error=%v", exit, err)
		}
		artifacts(t, directory, true)
	})
	t.Run("2 when ambiguous, even with findings, artifacts written", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel, contractActiveLabel)
		fixture.comment(10, 1000, time.Hour, releaseMarker+"\nRelease: feat/ten")
		directory := t.TempDir()
		exit, err := RunContracts(context.Background(), contractsConfig(directory), fixture.runner(t), fixedClock)
		if err != nil || exit != 2 {
			t.Fatalf("exit=%d error=%v", exit, err)
		}
		artifacts(t, directory, true)
		assertNoVerdict(t, directory)
	})
	t.Run("1 on a hard failure, no artifacts", func(t *testing.T) {
		fixture := newContractFixture()
		fixture.issue(10, "Wave 8", contractLabel)
		runner := fixture.runner(t)
		delete(runner.responses, "IssueComments:10:")
		directory := t.TempDir()
		exit, err := RunContracts(context.Background(), contractsConfig(directory), runner, fixedClock)
		if err == nil || exit != 1 {
			t.Fatalf("exit=%d error=%v", exit, err)
		}
		artifacts(t, directory, false)
	})
	t.Run("1 when the output directory is missing", func(t *testing.T) {
		fixture := newContractFixture()
		exit, err := RunContracts(context.Background(), contractsConfig(""), fixture.runner(t), fixedClock)
		if err == nil || exit != 1 {
			t.Fatalf("exit=%d error=%v", exit, err)
		}
	})
}

func TestContractModeReadsOnlyQueries(t *testing.T) {
	fixture := newContractFixture()
	fixture.issue(10, "Wave 8", contractLabel)
	fixture.issue(11, "Wave 8", "kind:feature")
	fixture.pullRequest(400, "feat/x", canonicalRepository)
	runner := fixture.runner(t)
	if _, err := CollectContracts(context.Background(), contractsConfig(""), runner, fixedClock); err != nil {
		t.Fatal(err)
	}
	// Open issues, open PRs, one closing-issue read, and comments for the
	// one contract issue: a non-contract issue's comments are never read.
	if len(runner.queries) != 4 {
		t.Fatalf("queries = %d", len(runner.queries))
	}
	for _, query := range runner.queries {
		if !strings.HasPrefix(strings.TrimSpace(query), "query ") {
			t.Fatalf("non-query document: %s", query)
		}
	}
}
