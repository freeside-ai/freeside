package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fixtureRunner struct {
	responses map[string]json.RawMessage
	queries   []string
}

func loadFixtureRunner(t *testing.T) *fixtureRunner {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "collection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var responses map[string]json.RawMessage
	if err := json.Unmarshal(raw, &responses); err != nil {
		t.Fatal(err)
	}
	return &fixtureRunner{responses: responses}
}

var operationPattern = regexp.MustCompile(`^query ([A-Za-z0-9_]+)`)

func (r *fixtureRunner) Query(_ context.Context, query string, variables map[string]any) ([]byte, error) {
	r.queries = append(r.queries, query)
	match := operationPattern.FindStringSubmatch(strings.TrimSpace(query))
	if match == nil {
		return nil, errors.New("query has no operation name")
	}
	number := ""
	if value, ok := variables["number"]; ok {
		number = toString(value)
	}
	cursor := ""
	if value, ok := variables["cursor"].(*string); ok && value != nil {
		cursor = *value
	}
	key := match[1] + ":" + number + ":" + cursor
	response, ok := r.responses[key]
	if !ok {
		return nil, errors.New("missing fixture response for " + key)
	}
	return response, nil
}

func toString(value any) string {
	switch typed := value.(type) {
	case int:
		return strconv.Itoa(typed)
	case json.Number:
		return typed.String()
	default:
		raw, _ := json.Marshal(value)
		return strings.Trim(string(raw), `"`)
	}
}

func fixedClock() time.Time {
	return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
}

func fixtureConfig(output string) Config {
	return Config{
		Repository:  Repository{Host: "github.com", Owner: "freeside-ai", Name: "freeside"},
		PullRequest: 300,
		OutputDir:   output,
		PageSize:    100,
		PageCap:     10,
	}
}

func TestPullRequestNumberMustFitGraphQLInt(t *testing.T) {
	t.Run("maximum reaches the query boundary", func(t *testing.T) {
		runner := loadFixtureRunner(t)
		config := fixtureConfig("")
		config.PullRequest = MaxGraphQLInt
		_, err := Collect(context.Background(), config, runner, fixedClock)
		if err == nil || !strings.Contains(err.Error(), "missing fixture response") || len(runner.queries) != 1 {
			t.Fatalf("error=%v queries=%d", err, len(runner.queries))
		}
	})
	t.Run("maximum plus one fails before querying", func(t *testing.T) {
		runner := loadFixtureRunner(t)
		config := fixtureConfig("")
		config.PullRequest = MaxGraphQLInt + 1
		_, err := Collect(context.Background(), config, runner, fixedClock)
		if err == nil || !strings.Contains(err.Error(), "between 1 and 2147483647") || len(runner.queries) != 0 {
			t.Fatalf("error=%v queries=%d", err, len(runner.queries))
		}
	})
}

func TestCheckboxSelectionIsLineLocalAndExact(t *testing.T) {
	body := "- [ ] #17 valid\n- [x] #173 prefix\n- [ ]\n#17 split\n9. [x] #17 ordered\n- [ ] #17suffix\n```md\n- [ ] #17 fenced\n```\n    - [ ] #17 indented\n<!--\n- [ ] #17 hidden\n-->"
	entries := ParseCheckboxEntries(body)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %#v", len(entries), entries)
	}
	if entries[0].UnitNumber != 17 || entries[1].UnitNumber != 173 || entries[2].UnitNumber != 17 {
		t.Fatalf("unexpected entries: %#v", entries)
	}
}

func TestCheckboxSelectionAcceptsMaximumGraphQLInt(t *testing.T) {
	entries, invalidNumbers := parseCheckboxEntries("- [ ] #2147483647")
	if len(entries) != 1 || entries[0].UnitNumber != MaxGraphQLInt || len(invalidNumbers) != 0 {
		t.Fatalf("entries=%#v invalid numbers=%#v", entries, invalidNumbers)
	}
}

func TestWaveTitlePatternIsExact(t *testing.T) {
	valid := []string{"Wave 6 (1B.0) tracking", "Wave 12 (anything) tracking"}
	invalid := []string{"Wave 6 tracking", "prefix Wave 6 (1B.0) tracking", "Wave 6 () tracking extra"}
	for _, title := range valid {
		if !waveTitlePattern.MatchString(title) {
			t.Errorf("valid title did not match: %q", title)
		}
	}
	for _, title := range invalid {
		if waveTitlePattern.MatchString(title) {
			t.Errorf("invalid title matched: %q", title)
		}
	}
}

func TestZeroAndMultipleWaveTitleMatchesAreAmbiguous(t *testing.T) {
	for name, titles := range map[string][]string{
		"zero":     {"Not a wave", "Reliability tracking"},
		"multiple": {"Wave 6 (1B.0) tracking", "Wave 7 (1B.1) tracking"},
	} {
		t.Run(name, func(t *testing.T) {
			runner := loadFixtureRunner(t)
			setPinnedTitle(t, runner, "PinnedIssues::", titles[0])
			setPinnedTitle(t, runner, "PinnedIssues::next", titles[1])
			snapshot, err := Collect(context.Background(), fixtureConfig(""), runner, fixedClock)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Ambiguities) != 1 || snapshot.Ambiguities[0].Code != "wave-title-count" {
				t.Fatalf("ambiguities = %#v", snapshot.Ambiguities)
			}
		})
	}
}

func TestSingleClosedWaveTitleMatchIsValidInterWaveEvidence(t *testing.T) {
	runner := loadFixtureRunner(t)
	var response map[string]any
	if err := json.Unmarshal(runner.responses["PinnedIssues::"], &response); err != nil {
		t.Fatal(err)
	}
	issue := response["data"].(map[string]any)["repository"].(map[string]any)["pinnedIssues"].(map[string]any)["nodes"].([]any)[0].(map[string]any)["issue"].(map[string]any)
	issue["state"] = "CLOSED"
	runner.responses["PinnedIssues::"], _ = json.Marshal(response)

	snapshot, err := Collect(context.Background(), fixtureConfig(""), runner, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.OpenWaveTitleMatchCount != 0 || len(snapshot.Ambiguities) != 0 {
		t.Fatalf("open matches=%d ambiguities=%#v", snapshot.OpenWaveTitleMatchCount, snapshot.Ambiguities)
	}
}

func TestClosingIssuesRequireActualCloserAttribution(t *testing.T) {
	tests := []struct {
		name           string
		state          string
		closer         any
		omitCloser     bool
		omitTimeline   bool
		emptyTimeline  bool
		multipleEvents bool
		wantClosing    int
		wantAmbiguity  string
		wantError      bool
	}{
		{name: "pull request closer", state: "CLOSED", closer: map[string]any{"__typename": "PullRequest", "number": 300, "merged": true, "mergeCommit": map[string]any{"oid": "merge-sha"}}, wantClosing: 1},
		{name: "merge commit closer", state: "CLOSED", closer: map[string]any{"__typename": "Commit", "oid": "merge-sha"}, wantClosing: 1},
		{name: "manual close", state: "CLOSED", closer: nil, wantAmbiguity: "unit-origin"},
		{name: "other pull request", state: "CLOSED", closer: map[string]any{"__typename": "PullRequest", "number": 299, "merged": true, "mergeCommit": map[string]any{"oid": "other-sha"}}, wantAmbiguity: "unit-origin"},
		{name: "other commit", state: "CLOSED", closer: map[string]any{"__typename": "Commit", "oid": "other-sha"}, wantAmbiguity: "unit-origin"},
		{name: "non-default base leaves open", state: "OPEN", emptyTimeline: true, wantAmbiguity: "unit-origin"},
		{name: "reopened after attributed close", state: "OPEN", closer: map[string]any{"__typename": "PullRequest", "number": 300, "merged": true, "mergeCommit": map[string]any{"oid": "merge-sha"}}, wantAmbiguity: "closing-issue-state"},
		{name: "missing closer", state: "CLOSED", omitCloser: true, wantError: true},
		{name: "missing closed event", state: "CLOSED", omitTimeline: true, wantError: true},
		{name: "open issue missing timeline", state: "OPEN", omitTimeline: true, wantError: true},
		{name: "no closed event", state: "CLOSED", emptyTimeline: true, wantError: true},
		{name: "multiple latest events", state: "CLOSED", multipleEvents: true, wantError: true},
		{name: "commit missing oid", state: "CLOSED", closer: map[string]any{"__typename": "Commit"}, wantError: true},
		{name: "pull request missing number", state: "CLOSED", closer: map[string]any{"__typename": "PullRequest", "merged": true, "mergeCommit": map[string]any{"oid": "merge-sha"}}, wantError: true},
		{name: "incomplete pull request", state: "CLOSED", closer: map[string]any{"__typename": "PullRequest", "number": 300, "merged": false, "mergeCommit": nil}, wantError: true},
		{name: "pull request missing merge oid", state: "CLOSED", closer: map[string]any{"__typename": "PullRequest", "number": 300, "merged": true, "mergeCommit": map[string]any{}}, wantError: true},
		{name: "unknown closer", state: "CLOSED", closer: map[string]any{"__typename": "User"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := loadFixtureRunner(t)
			var response map[string]any
			if err := json.Unmarshal(runner.responses["MergedPullRequest:300:"], &response); err != nil {
				t.Fatal(err)
			}
			issue := response["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)["closingIssuesReferences"].(map[string]any)["nodes"].([]any)[0].(map[string]any)
			issue["state"] = test.state
			if test.omitTimeline {
				delete(issue, "timelineItems")
			} else {
				timeline := issue["timelineItems"].(map[string]any)
				event := timeline["nodes"].([]any)[0].(map[string]any)
				if test.emptyTimeline {
					timeline["nodes"] = []any{}
				} else {
					if test.multipleEvents {
						timeline["nodes"] = []any{event, event}
					}
					if test.omitCloser {
						delete(event, "closer")
					} else {
						event["closer"] = test.closer
					}
				}
			}
			runner.responses["MergedPullRequest:300:"], _ = json.Marshal(response)
			snapshot, err := Collect(context.Background(), fixtureConfig(""), runner, fixedClock)
			if test.wantError {
				if err == nil {
					t.Fatal("expected hard failure")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.MergedPullRequest.ClosingIssues) != test.wantClosing {
				t.Fatalf("closing=%#v, want %d issue(s)", snapshot.MergedPullRequest.ClosingIssues, test.wantClosing)
			}
			if test.wantAmbiguity != "" && !hasAmbiguity(snapshot.Ambiguities, test.wantAmbiguity) {
				t.Fatalf("missing %s ambiguity: %#v", test.wantAmbiguity, snapshot.Ambiguities)
			}
		})
	}
}

func TestMergedPullRequestIdentityAndStampAreStableAcrossPages(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(map[string]any)
		wantError bool
	}{
		{name: "stable"},
		{name: "node id", mutate: func(pr map[string]any) { pr["id"] = "PR_other" }, wantError: true},
		{name: "database id", mutate: func(pr map[string]any) { pr["databaseId"] = float64(301) }, wantError: true},
		{name: "title", mutate: func(pr map[string]any) { pr["title"] = "Changed title" }, wantError: true},
		{name: "updatedAt", mutate: func(pr map[string]any) { pr["updatedAt"] = "2026-08-25T10:00:02Z" }, wantError: true},
		{name: "body", mutate: func(pr map[string]any) { pr["body"] = "Changed body" }, wantError: true},
		{name: "head ref", mutate: func(pr map[string]any) { pr["headRefName"] = "other-head" }, wantError: true},
		{name: "base ref", mutate: func(pr map[string]any) { pr["baseRefName"] = "other-base" }, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := loadFixtureRunner(t)
			first := mergedPullRequestFixture(t, runner.responses["MergedPullRequest:300:"])
			connection := first["closingIssuesReferences"].(map[string]any)
			connection["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "next"}
			runner.responses["MergedPullRequest:300:"], _ = json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{
				"defaultBranchRef": map[string]any{"name": "main", "target": map[string]any{"oid": "base-sha"}},
				"pullRequest":      first,
			}}})

			second := cloneJSONMap(t, first)
			second["closingIssuesReferences"] = map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
			if test.mutate != nil {
				test.mutate(second)
			}
			runner.responses["MergedPullRequest:300:next"], _ = json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{
				"defaultBranchRef": map[string]any{"name": "main", "target": map[string]any{"oid": "base-sha"}},
				"pullRequest":      second,
			}}})

			_, err := Collect(context.Background(), fixtureConfig(""), runner, fixedClock)
			if test.wantError && err == nil {
				t.Fatal("expected hard failure")
			}
			if !test.wantError && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func mergedPullRequestFixture(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	return response["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)
}

func cloneJSONMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func setPinnedTitle(t *testing.T, runner *fixtureRunner, key, title string) {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(runner.responses[key], &response); err != nil {
		t.Fatal(err)
	}
	nodes := response["data"].(map[string]any)["repository"].(map[string]any)["pinnedIssues"].(map[string]any)["nodes"].([]any)
	nodes[0].(map[string]any)["issue"].(map[string]any)["title"] = title
	runner.responses[key], _ = json.Marshal(response)
}

func TestRepositoryIdentityDistinguishesNullMissingMalformedAndFork(t *testing.T) {
	canonical, err := parseRepositoryIdentity(json.RawMessage(`{"nameWithOwner":"FREESIDE-AI/FREESIDE"}`))
	if err != nil || !identityMatches(canonical, "freeside-ai/freeside") {
		t.Fatalf("canonical identity: %#v, %v", canonical, err)
	}
	fork, err := parseRepositoryIdentity(json.RawMessage(`{"nameWithOwner":"someone/freeside"}`))
	if err != nil || identityMatches(fork, "freeside-ai/freeside") {
		t.Fatalf("fork identity: %#v, %v", fork, err)
	}
	nullIdentity, err := parseRepositoryIdentity(json.RawMessage(`null`))
	if err != nil || nullIdentity.State != "explicit-null" || identityMatches(nullIdentity, "freeside-ai/freeside") {
		t.Fatalf("null identity: %#v, %v", nullIdentity, err)
	}
	for name, raw := range map[string]json.RawMessage{
		"missing":   nil,
		"field":     json.RawMessage(`{}`),
		"malformed": json.RawMessage(`{"nameWithOwner":"three/part/name"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRepositoryIdentity(raw); err == nil {
				t.Fatal("expected hard failure")
			}
		})
	}
}

func TestScopeExtractionPreservesBoundariesAndDeduplicatesPaths(t *testing.T) {
	body := "```md\n### Scope / declared paths\n- `ignored/`\n```\nintro\n### Scope / declared paths\n- `AGENTS.md`\n- `scripts/`\n- `scripts/trackercollect/main.go`\n- `scripts/`\n\n### Dependencies\n- none"
	section := extractSection(body, "Scope / declared paths")
	if strings.Contains(section, "Dependencies") || !strings.Contains(section, "AGENTS.md") {
		t.Fatalf("section boundary not preserved: %q", section)
	}
	paths := parseDeclaredPaths(section)
	want := []string{"AGENTS.md", "scripts/", "scripts/trackercollect/main.go"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestScopeExtractionParsesCommaSeparatedProse(t *testing.T) {
	section := "### Scope / declared paths\n\ndaemon/internal/engine, daemon/internal/exec, daemon/cmd/freesided, AGENTS.md, scripts/."
	want := "AGENTS.md|daemon/cmd/freesided|daemon/internal/engine|daemon/internal/exec|scripts/"
	if got := strings.Join(parseDeclaredPaths(section), "|"); got != want {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestDuplicateTrackerEntriesAreAmbiguous(t *testing.T) {
	runner := loadFixtureRunner(t)
	c := &collector{config: fixtureConfig(""), runner: runner, issueCache: map[int]UnitEvidence{}, prCache: map[int]PullRequestRef{}}
	issue := graphIssue{ID: "tracker", DatabaseID: 100, Number: 100, State: "OPEN", Body: "- [ ] #935 first\n2. [x] #935 second\n\n## Implementation Order\nPending"}
	trackers, _, err := c.buildContainingTrackers(context.Background(), []graphIssue{issue}, []IssueSummary{{Number: 935}})
	if err != nil {
		t.Fatal(err)
	}
	if len(trackers) != 1 || len(c.ambiguities) != 1 || c.ambiguities[0].Code != "duplicate-tracker-entry" {
		t.Fatalf("trackers=%d ambiguities=%#v", len(trackers), c.ambiguities)
	}
}

func TestContainingTrackerRequiresTrackerStructureAndValidEntries(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "ordinary work item", body: "## Acceptance\n- [ ] #935", code: "tracker-structure"},
		{name: "duplicate order sections", body: "- [ ] #935\n\n## Implementation Order\nFirst\n\n## Implementation Order\nSecond", code: "tracker-structure"},
		{name: "checkbox number exceeds forge Int", body: "- [ ] #935\n- [ ] #2147483648\n\n## Implementation Order\nPending", code: "malformed-tracker-entry"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := loadFixtureRunner(t)
			c := &collector{config: fixtureConfig(""), runner: runner, issueCache: map[int]UnitEvidence{}, prCache: map[int]PullRequestRef{}}
			issue := graphIssue{ID: "candidate", DatabaseID: 100, Number: 100, State: "OPEN", Body: test.body}
			trackers, _, err := c.buildContainingTrackers(context.Background(), []graphIssue{issue}, []IssueSummary{{Number: 935}})
			if err != nil {
				t.Fatal(err)
			}
			if len(trackers) != 0 || len(c.ambiguities) != 1 || c.ambiguities[0].Code != test.code {
				t.Fatalf("trackers=%d ambiguities=%#v", len(trackers), c.ambiguities)
			}
		})
	}
}

func TestUnitRequiresExactlyOneDependenciesSection(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "missing", body: "No dependency record"},
		{name: "duplicate", body: "### Dependencies\nnone\n\n### Dependencies\nnone"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := loadFixtureRunner(t)
			issue := issueDetailsFixture(t, runner.responses["IssueDetails:935:"])
			issue["body"] = test.body
			runner.responses["IssueDetails:935:"], _ = json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"issue": issue}}})
			c := &collector{config: fixtureConfig(""), runner: runner, issueCache: map[int]UnitEvidence{}, prCache: map[int]PullRequestRef{}}
			unit, err := c.fetchUnit(context.Background(), 935)
			if err != nil {
				t.Fatal(err)
			}
			if unit.DependenciesSection != "" || len(c.ambiguities) != 1 || c.ambiguities[0].Code != "parse-collision" {
				t.Fatalf("dependencies=%q ambiguities=%#v", unit.DependenciesSection, c.ambiguities)
			}
		})
	}
}

func TestIssueIdentityAndStampAreStableAcrossClosingPullRequestPages(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(map[string]any)
		wantError bool
	}{
		{name: "stable"},
		{name: "node id", mutate: func(issue map[string]any) { issue["id"] = "I_other" }, wantError: true},
		{name: "database id", mutate: func(issue map[string]any) { issue["databaseId"] = float64(936) }, wantError: true},
		{name: "number", mutate: func(issue map[string]any) { issue["number"] = float64(936) }, wantError: true},
		{name: "title", mutate: func(issue map[string]any) { issue["title"] = "Changed title" }, wantError: true},
		{name: "state", mutate: func(issue map[string]any) { issue["state"] = "OPEN" }, wantError: true},
		{name: "updatedAt", mutate: func(issue map[string]any) { issue["updatedAt"] = "2026-08-25T10:00:02Z" }, wantError: true},
		{name: "body", mutate: func(issue map[string]any) { issue["body"] = "### Dependencies\nchanged" }, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := loadFixtureRunner(t)
			first := issueDetailsFixture(t, runner.responses["IssueDetails:935:"])
			first["closedByPullRequestsReferences"].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "next"}
			runner.responses["IssueDetails:935:"], _ = json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"issue": first}}})

			second := cloneJSONMap(t, first)
			second["closedByPullRequestsReferences"] = map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
			if test.mutate != nil {
				test.mutate(second)
			}
			runner.responses["IssueDetails:935:next"], _ = json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"issue": second}}})

			c := &collector{config: fixtureConfig(""), runner: runner, issueCache: map[int]UnitEvidence{}, prCache: map[int]PullRequestRef{}}
			issue, pulls, err := c.fetchIssueDetails(context.Background(), 935)
			if test.wantError && err == nil {
				t.Fatal("expected hard failure")
			}
			if !test.wantError && (err != nil || issue.Number != 935 || len(pulls) != 1) {
				t.Fatalf("issue=%#v pulls=%#v error=%v", issue, pulls, err)
			}
		})
	}
}

func issueDetailsFixture(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	return response["data"].(map[string]any)["repository"].(map[string]any)["issue"].(map[string]any)
}

func TestMarkerParsersBindReleaseAndRetainReservation(t *testing.T) {
	c := &collector{config: fixtureConfig("")}
	release, keep := c.parseMarkerComment(1, graphComment{
		DatabaseID: 2,
		Body:       "<!-- freeside-work-release:v1 -->\nRelease: feat/work\nReleases-claim: 123",
	}, nil)
	if !keep || release.Kind != "release" || release.Branch != "feat/work" || release.ReleasesClaimID != 123 {
		t.Fatalf("release = %#v, keep=%t", release, keep)
	}
	reservation, keep := c.parseMarkerComment(1, graphComment{DatabaseID: 3, Body: "<!-- freeside-planning-reservation:v1 -->\nPlan: #1"}, nil)
	if !keep || reservation.Kind != "planning-reservation" || reservation.PlanIssueNumber != 1 {
		t.Fatalf("reservation = %#v, keep=%t", reservation, keep)
	}
}

func TestPlanningReservationRequiresMatchingPlanLine(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing", body: reservationMarker},
		{name: "zero", body: reservationMarker + "\nPlan: #0"},
		{name: "forge Int overflow", body: reservationMarker + "\nPlan: #2147483648"},
		{name: "different issue", body: reservationMarker + "\nPlan: #2"},
		{name: "duplicate", body: reservationMarker + "\nPlan: #1\nPlan: #1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := &collector{config: fixtureConfig("")}
			marker, keep := c.parseMarkerComment(1, graphComment{DatabaseID: 3, Body: test.body}, nil)
			if !keep || marker.Kind != "planning-reservation" || marker.PlanIssueNumber != 0 || len(c.ambiguities) != 1 || c.ambiguities[0].Code != "malformed-marker-comment" {
				t.Fatalf("marker=%#v keep=%t ambiguities=%#v", marker, keep, c.ambiguities)
			}
		})
	}
}

func TestReleaseMarkerAcceptsMaximumInt64ClaimID(t *testing.T) {
	c := &collector{config: fixtureConfig("")}
	marker, keep := c.parseMarkerComment(1, graphComment{
		DatabaseID: 2,
		Body:       releaseMarker + "\nRelease: feat/work\nReleases-claim: 9223372036854775807",
	}, nil)
	if !keep || marker.ReleasesClaimID != math.MaxInt64 || len(c.ambiguities) != 0 {
		t.Fatalf("marker=%#v keep=%t ambiguities=%#v", marker, keep, c.ambiguities)
	}
}

func TestReleaseMarkerRequiresPositiveInt64ClaimID(t *testing.T) {
	for _, claimID := range []string{"0", "9223372036854775808"} {
		t.Run(claimID, func(t *testing.T) {
			c := &collector{config: fixtureConfig("")}
			marker, keep := c.parseMarkerComment(1, graphComment{
				DatabaseID: 2,
				Body:       releaseMarker + "\nRelease: feat/work\nReleases-claim: " + claimID,
			}, nil)
			if !keep || marker.Kind != "release" || marker.ReleasesClaimID != 0 || len(c.ambiguities) != 1 || c.ambiguities[0].Code != "malformed-marker-comment" {
				t.Fatalf("marker=%#v keep=%t ambiguities=%#v", marker, keep, c.ambiguities)
			}
		})
	}
}

func TestMarkerParsingRequiresExactMarkerAndRejectsCollisions(t *testing.T) {
	c := &collector{config: fixtureConfig("")}
	if _, keep := c.parseMarkerComment(1, graphComment{Body: "mentions freeside-work-claim:v1 as prose\nClaim: feat/work"}, nil); keep {
		t.Fatal("prose marker name was retained as a marker comment")
	}
	if _, keep := c.parseMarkerComment(1, graphComment{Body: "```md\n" + claimMarker + "\nClaim: feat/work\n```"}, nil); keep {
		t.Fatal("fenced marker was retained as a marker comment")
	}
	if _, keep := c.parseMarkerComment(1, graphComment{Body: "<!--\n" + claimMarker + "\nClaim: feat/work\n-->"}, nil); keep {
		t.Fatal("marker nested in an HTML comment was retained")
	}
	marker, keep := c.parseMarkerComment(1, graphComment{
		DatabaseID: 4,
		Body:       claimMarker + "\nClaim: feat/one\nClaim: feat/two",
	}, nil)
	if !keep || marker.Kind != "claim" || len(c.ambiguities) != 1 || c.ambiguities[0].Code != "malformed-marker-comment" {
		t.Fatalf("marker=%#v ambiguities=%#v", marker, c.ambiguities)
	}
}

func TestOmittedConnectionFailsLoud(t *testing.T) {
	runner := loadFixtureRunner(t)
	runner.responses["OpenIssues::"] = json.RawMessage(`{"data":{"repository":{}}}`)
	if _, err := Collect(context.Background(), fixtureConfig(""), runner, fixedClock); err == nil || !strings.Contains(err.Error(), "connection") {
		t.Fatalf("error = %v, want missing connection", err)
	}
}

func TestOmittedCommentBodyAndDraftFlagFailLoud(t *testing.T) {
	t.Run("omitted comment body", func(t *testing.T) {
		runner := loadFixtureRunner(t)
		deleteFixtureNodeField(t, runner, "IssueComments:935:", []string{"data", "repository", "issue", "comments", "nodes"}, "body")
		c := &collector{config: fixtureConfig(""), runner: runner}
		if _, err := c.fetchIssueComments(context.Background(), 935); err == nil || !strings.Contains(err.Error(), "missing body") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("null comment body", func(t *testing.T) {
		runner := loadFixtureRunner(t)
		setFixtureNodeField(t, runner, "IssueComments:935:", []string{"data", "repository", "issue", "comments", "nodes"}, "body", nil)
		c := &collector{config: fixtureConfig(""), runner: runner}
		if _, err := c.fetchIssueComments(context.Background(), 935); err == nil || !strings.Contains(err.Error(), "missing body") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("omitted open PR isDraft", func(t *testing.T) {
		runner := loadFixtureRunner(t)
		deleteFixtureNodeField(t, runner, "OpenPullRequests::", []string{"data", "repository", "pullRequests", "nodes"}, "isDraft")
		c := &collector{config: fixtureConfig(""), runner: runner}
		if _, err := c.fetchOpenPullRequests(context.Background()); err == nil || !strings.Contains(err.Error(), "isDraft") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("null open PR isDraft", func(t *testing.T) {
		runner := loadFixtureRunner(t)
		setFixtureNodeField(t, runner, "OpenPullRequests::", []string{"data", "repository", "pullRequests", "nodes"}, "isDraft", nil)
		c := &collector{config: fixtureConfig(""), runner: runner}
		if _, err := c.fetchOpenPullRequests(context.Background()); err == nil || !strings.Contains(err.Error(), "isDraft") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRequestedObjectLookupsBindParentIdentity(t *testing.T) {
	t.Run("pull request reference rejects mismatch without caching", func(t *testing.T) {
		runner := &fixtureRunner{responses: map[string]json.RawMessage{
			"PullRequestReference:92:": json.RawMessage(`{"data":{"repository":{"pullRequest":{"id":"PR_93","databaseId":93,"number":93,"title":"Wrong base","state":"OPEN","merged":false,"updatedAt":"2026-08-25T10:00:00Z","headRefName":"wrong","baseRefName":"main","body":"","headRepository":{"nameWithOwner":"freeside-ai/freeside"}}}}}`),
		}}
		c := &collector{config: fixtureConfig(""), runner: runner, prCache: map[int]PullRequestRef{}}
		if _, err := c.fetchPullRequestReference(context.Background(), 92); err == nil || !strings.Contains(err.Error(), "names #93, want #92") {
			t.Fatalf("error = %v", err)
		}
		if len(c.prCache) != 0 {
			t.Fatalf("mismatched response poisoned cache: %#v", c.prCache)
		}
	})

	t.Run("linked issues reject wrong parent", func(t *testing.T) {
		runner := loadFixtureRunner(t)
		parent := connectionParentFixture(t, runner.responses["PullRequestClosingIssues:400:"], "pullRequest")
		parent["number"] = float64(401)
		runner.responses["PullRequestClosingIssues:400:"] = wrapConnectionParent(t, "pullRequest", parent)
		c := &collector{config: fixtureConfig(""), runner: runner}
		if _, err := c.fetchPullRequestClosingIssues(context.Background(), 400); err == nil || !strings.Contains(err.Error(), "names #401, want #400") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("linked issues reject parent changes across pages", func(t *testing.T) {
		runner := loadFixtureRunner(t)
		first := connectionParentFixture(t, runner.responses["PullRequestClosingIssues:400:"], "pullRequest")
		first["closingIssuesReferences"].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "next"}
		runner.responses["PullRequestClosingIssues:400:"] = wrapConnectionParent(t, "pullRequest", first)
		second := cloneJSONMap(t, first)
		second["updatedAt"] = "2026-08-25T09:46:00Z"
		second["closingIssuesReferences"] = map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
		runner.responses["PullRequestClosingIssues:400:next"] = wrapConnectionParent(t, "pullRequest", second)
		c := &collector{config: fixtureConfig(""), runner: runner}
		if _, err := c.fetchPullRequestClosingIssues(context.Background(), 400); err == nil || !strings.Contains(err.Error(), "changed across linked-issue pages") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("comments reject wrong parent", func(t *testing.T) {
		runner := loadFixtureRunner(t)
		parent := connectionParentFixture(t, runner.responses["IssueComments:935:"], "issue")
		parent["number"] = float64(936)
		runner.responses["IssueComments:935:"] = wrapConnectionParent(t, "issue", parent)
		c := &collector{config: fixtureConfig(""), runner: runner}
		if _, err := c.fetchIssueComments(context.Background(), 935); err == nil || !strings.Contains(err.Error(), "names #936, want #935") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("comments reject parent changes across pages", func(t *testing.T) {
		runner := loadFixtureRunner(t)
		first := connectionParentFixture(t, runner.responses["IssueComments:935:"], "issue")
		first["comments"].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "next"}
		runner.responses["IssueComments:935:"] = wrapConnectionParent(t, "issue", first)
		second := cloneJSONMap(t, first)
		second["updatedAt"] = "2026-08-25T10:00:02Z"
		second["comments"] = map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
		runner.responses["IssueComments:935:next"] = wrapConnectionParent(t, "issue", second)
		c := &collector{config: fixtureConfig(""), runner: runner}
		if _, err := c.fetchIssueComments(context.Background(), 935); err == nil || !strings.Contains(err.Error(), "changed across comment pages") {
			t.Fatalf("error = %v", err)
		}
	})
}

func connectionParentFixture(t *testing.T, raw json.RawMessage, field string) map[string]any {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	return response["data"].(map[string]any)["repository"].(map[string]any)[field].(map[string]any)
}

func wrapConnectionParent(t *testing.T, field string, parent map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{field: parent}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func deleteFixtureNodeField(t *testing.T, runner *fixtureRunner, key string, path []string, field string) {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(runner.responses[key], &response); err != nil {
		t.Fatal(err)
	}
	var current any = response
	for _, part := range path {
		current = current.(map[string]any)[part]
	}
	delete(current.([]any)[0].(map[string]any), field)
	runner.responses[key], _ = json.Marshal(response)
}

func setFixtureNodeField(t *testing.T, runner *fixtureRunner, key string, path []string, field string, value any) {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(runner.responses[key], &response); err != nil {
		t.Fatal(err)
	}
	var current any = response
	for _, part := range path {
		current = current.(map[string]any)[part]
	}
	current.([]any)[0].(map[string]any)[field] = value
	runner.responses[key], _ = json.Marshal(response)
}

func TestTruncatedOpenIssueInventoryNeverReportsValidZeroWork(t *testing.T) {
	snapshot := Snapshot{Ambiguities: []Ambiguity{{Code: "page-cap-truncation", Subject: "open issues"}}}
	report := RenderReport(snapshot)
	if strings.Contains(report, "valid zero-work result") || !strings.Contains(report, "not a zero-work verdict") {
		t.Fatalf("unsafe truncation report: %s", report)
	}
}

func TestIncompleteTrackerSelectionNeverReportsValidZeroWork(t *testing.T) {
	for _, code := range []string{"tracker-structure", "malformed-tracker-entry"} {
		t.Run(code, func(t *testing.T) {
			snapshot := Snapshot{
				MergedPullRequest: MergedPullRequest{ClosingIssues: []IssueSummary{{Number: 935}}},
				Ambiguities:       []Ambiguity{{Code: code, Subject: "issue #100"}},
			}
			report := RenderReport(snapshot)
			if strings.Contains(report, "valid zero-work result") || !strings.Contains(report, "not a zero-work verdict") {
				t.Fatalf("unsafe %s report: %s", code, report)
			}
		})
	}
}

func TestNestedTruncationQualifiesEmptyEvidence(t *testing.T) {
	snapshot := Snapshot{
		ContainingTrackers: []ContainingTracker{{Units: []UnitEvidence{{Number: 17}}}},
		Ambiguities: []Ambiguity{
			{Code: "page-cap-truncation", Subject: "issue #17 closing pull requests"},
			{Code: "page-cap-truncation", Subject: "issue #17 comments"},
		},
	}
	report := RenderReport(snapshot)
	if strings.Contains(report, "Closing PRs: none retained") || strings.Contains(report, "None retained") || !strings.Contains(report, "not a completeness claim") {
		t.Fatalf("unsafe nested truncation report: %s", report)
	}
}

func TestReportShowsCanonicalAndForkRepositoryIdentity(t *testing.T) {
	canonical := "freeside-ai/freeside"
	fork := "someone/freeside"
	snapshot := Snapshot{
		Repository: "github.com/freeside-ai/freeside",
		OpenPullRequests: []OpenPullRequest{
			{Number: 1, HeadRepository: RepositoryIdentity{State: "present", NameWithOwner: &canonical}},
			{Number: 2, HeadRepository: RepositoryIdentity{State: "present", NameWithOwner: &fork}},
		},
	}
	report := RenderReport(snapshot)
	if !strings.Contains(report, "canonical repository: true") || !strings.Contains(report, "canonical repository: false") {
		t.Fatalf("repository identity missing from report: %s", report)
	}
}

func TestMissingForgeStampFailsLoud(t *testing.T) {
	if err := validateIssue(graphIssue{Number: 1, State: "OPEN", UpdatedAt: "2026-08-25T00:00:00Z"}); err == nil {
		t.Fatal("expected missing IDs to fail")
	}
}

func TestPullRequestScalarPresence(t *testing.T) {
	var pr graphPullRequest
	raw := []byte(`{"id":"PR","databaseId":1,"number":1,"title":"title","state":"OPEN","merged":false,"updatedAt":"2026-08-25T00:00:00Z","headRefName":"head","baseRefName":"main","isDraft":false,"body":""}`)
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatal(err)
	}
	if !pr.titlePresent || !pr.statePresent || !pr.mergedPresent || !pr.draftPresent || !pr.bodyPresent || !pr.headPresent || !pr.basePresent {
		t.Fatalf("presence: %#v", pr)
	}
	var nullPR graphPullRequest
	if err := json.Unmarshal([]byte(`{"id":"PR","databaseId":1,"number":1,"title":null,"state":"OPEN","merged":null,"updatedAt":"2026-08-25T00:00:00Z","headRefName":"head","baseRefName":"main","isDraft":null,"body":""}`), &nullPR); err != nil {
		t.Fatal(err)
	}
	if nullPR.titlePresent || nullPR.mergedPresent || nullPR.draftPresent {
		t.Fatalf("null scalars reported present: %#v", nullPR)
	}
}

func TestRawReportSectionCannotCloseItsOwnFence(t *testing.T) {
	var report strings.Builder
	buffer := bytes.NewBuffer(nil)
	writeRawSection(buffer, "Dependencies", "### Dependencies\n```sh\necho unsafe\n```")
	report.WriteString(buffer.String())
	if !strings.Contains(report.String(), "````markdown") || strings.Count(report.String(), "````") != 2 {
		t.Fatalf("unsafe fence rendering: %s", report.String())
	}
}

func TestStackedOnReferencesRetainIssueAndPullRequestKinds(t *testing.T) {
	refs, invalidNumbers := parseStackedReferences("### Dependencies\n- stacked-on: #91\n- stacked-on PR #92\n- stacked-on: #91")
	if len(refs) != 2 || refs[0] != (stackedReference{Number: 91, Kind: "issue"}) || refs[1] != (stackedReference{Number: 92, Kind: "pull_request"}) {
		t.Fatalf("refs = %#v", refs)
	}
	if len(invalidNumbers) != 0 {
		t.Fatalf("invalid numbers = %#v", invalidNumbers)
	}
}

func TestStackedOnReferencesRejectOversizedIssueNumbers(t *testing.T) {
	refs, invalidNumbers := parseStackedReferences("### Dependencies\n- stacked-on: #91\n- stacked-on PR #2147483648")
	if len(refs) != 1 || refs[0] != (stackedReference{Number: 91, Kind: "issue"}) || len(invalidNumbers) != 1 {
		t.Fatalf("refs=%#v invalid numbers=%#v", refs, invalidNumbers)
	}
}

func TestStackedOnReferencesAcceptMaximumGraphQLInt(t *testing.T) {
	refs, invalidNumbers := parseStackedReferences("### Dependencies\n- stacked-on: #2147483647")
	if len(refs) != 1 || refs[0].Number != MaxGraphQLInt || len(invalidNumbers) != 0 {
		t.Fatalf("refs=%#v invalid numbers=%#v", refs, invalidNumbers)
	}
}

func TestFixtureCollectionDrainsPaginationAndIsDeterministic(t *testing.T) {
	runner := loadFixtureRunner(t)
	first, err := Collect(context.Background(), fixtureConfig(""), runner, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Collect(context.Background(), fixtureConfig(""), runner, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.MarshalIndent(first, "", "  ")
	secondJSON, _ := json.MarshalIndent(second, "", "  ")
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("snapshot JSON is not deterministic")
	}
	if RenderReport(first) != RenderReport(second) {
		t.Fatal("report is not deterministic")
	}
	if len(first.PinnedIssues) != 2 {
		t.Fatalf("pagination retained %d pinned issues, want 2", len(first.PinnedIssues))
	}
	if len(first.OpenIssueInventory) != 1 || first.OpenIssueInventory[0].Number != 100 {
		t.Fatalf("open-issue inventory = %#v", first.OpenIssueInventory)
	}
	if len(first.ContainingTrackers) != 1 || len(first.OpenPullRequests) != 1 || len(first.MarkerComments) != 1 {
		t.Fatalf("fixture evidence missing: trackers=%d prs=%d comments=%d", len(first.ContainingTrackers), len(first.OpenPullRequests), len(first.MarkerComments))
	}
	if got := first.MarkerComments[0].MatchedOpenPullRequests; len(got) != 1 || got[0] != 400 {
		t.Fatalf("canonical claim match = %#v, want [400]", got)
	}
	if len(first.Ambiguities) != 0 {
		t.Fatalf("unexpected ambiguities: %#v", first.Ambiguities)
	}
	for _, query := range runner.queries {
		trimmed := strings.TrimSpace(query)
		if !strings.HasPrefix(trimmed, "query ") || strings.Contains(strings.ToLower(trimmed), "mutation") {
			t.Fatalf("fixture runner received non-query document: %s", query)
		}
	}
}

func TestPageCapWritesAmbiguousArtifactsAndReturnsTwo(t *testing.T) {
	runner := loadFixtureRunner(t)
	output := filepath.Join(t.TempDir(), "out")
	config := fixtureConfig(output)
	config.PageCap = 1
	code, err := Run(context.Background(), config, runner, fixedClock)
	if err != nil || code != 2 {
		t.Fatalf("Run() = %d, %v; want 2, nil", code, err)
	}
	report, err := os.ReadFile(filepath.Join(output, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "page-cap-truncation") {
		t.Fatalf("report does not expose truncation: %s", report)
	}
}

func TestNonMergedPullRequestFailsWithoutArtifacts(t *testing.T) {
	runner := loadFixtureRunner(t)
	var response map[string]any
	if err := json.Unmarshal(runner.responses["MergedPullRequest:300:"], &response); err != nil {
		t.Fatal(err)
	}
	pr := response["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)
	pr["merged"] = false
	pr["state"] = "OPEN"
	runner.responses["MergedPullRequest:300:"], _ = json.Marshal(response)
	output := filepath.Join(t.TempDir(), "not-created")
	code, err := Run(context.Background(), fixtureConfig(output), runner, fixedClock)
	if code != 1 || err == nil {
		t.Fatalf("Run() = %d, %v; want hard failure", code, err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("output directory was written on non-merged input: %v", statErr)
	}
}

func TestZeroClosingIssuesRequireExplicitDirectProvenance(t *testing.T) {
	runner := loadFixtureRunner(t)
	var response map[string]any
	if err := json.Unmarshal(runner.responses["MergedPullRequest:300:"], &response); err != nil {
		t.Fatal(err)
	}
	closing := response["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)["closingIssuesReferences"].(map[string]any)
	closing["nodes"] = []any{}
	runner.responses["MergedPullRequest:300:"], _ = json.Marshal(response)
	temporary := t.TempDir()
	config := fixtureConfig(filepath.Join(temporary, "ambiguous"))
	snapshot, err := Collect(context.Background(), config, runner, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MergedPullRequest.DirectUnit || !hasAmbiguity(snapshot.Ambiguities, "unit-origin") {
		t.Fatalf("direct=%t ambiguities=%#v", snapshot.MergedPullRequest.DirectUnit, snapshot.Ambiguities)
	}
	code, err := Run(context.Background(), config, runner, fixedClock)
	if code != 2 || err != nil {
		t.Fatalf("Run() without --direct = %d, %v; want 2, nil", code, err)
	}
	report, err := os.ReadFile(filepath.Join(config.OutputDir, "report.md"))
	if err != nil || !strings.Contains(string(report), "unit-origin") || strings.Contains(string(report), "valid zero-work result") {
		t.Fatalf("ambiguous report = %q, %v", report, err)
	}

	config.Direct = true
	config.OutputDir = filepath.Join(temporary, "direct")
	snapshot, err = Collect(context.Background(), config, runner, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.MergedPullRequest.DirectUnit || hasAmbiguity(snapshot.Ambiguities, "unit-origin") || len(snapshot.ContainingTrackers) != 0 {
		t.Fatalf("direct=%t trackers=%#v ambiguities=%#v", snapshot.MergedPullRequest.DirectUnit, snapshot.ContainingTrackers, snapshot.Ambiguities)
	}
	code, err = Run(context.Background(), config, runner, fixedClock)
	if code != 0 || err != nil {
		t.Fatalf("Run() with --direct = %d, %v; want 0, nil", code, err)
	}
}

func TestDirectProvenanceContradictingAttributedClosureFailsWithoutArtifacts(t *testing.T) {
	for _, state := range []string{"CLOSED", "OPEN"} {
		t.Run(strings.ToLower(state), func(t *testing.T) {
			runner := loadFixtureRunner(t)
			if state == "OPEN" {
				var response map[string]any
				if err := json.Unmarshal(runner.responses["MergedPullRequest:300:"], &response); err != nil {
					t.Fatal(err)
				}
				issue := response["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)["closingIssuesReferences"].(map[string]any)["nodes"].([]any)[0].(map[string]any)
				issue["state"] = state
				runner.responses["MergedPullRequest:300:"], _ = json.Marshal(response)
			}
			output := filepath.Join(t.TempDir(), "not-created")
			config := fixtureConfig(output)
			config.Direct = true
			code, err := Run(context.Background(), config, runner, fixedClock)
			if code != 1 || err == nil {
				t.Fatalf("Run() = %d, %v; want hard failure", code, err)
			}
			if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
				t.Fatalf("output directory was written on contradictory direct provenance: %v", statErr)
			}
		})
	}
}

func TestMixedClosedAndReopenedAttributionNeverReportsValidZeroWork(t *testing.T) {
	runner := loadFixtureRunner(t)
	var mergedResponse map[string]any
	if err := json.Unmarshal(runner.responses["MergedPullRequest:300:"], &mergedResponse); err != nil {
		t.Fatal(err)
	}
	closing := mergedResponse["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)["closingIssuesReferences"].(map[string]any)
	nodes := closing["nodes"].([]any)
	reopened := cloneJSONMap(t, nodes[0].(map[string]any))
	reopened["id"] = "I_936"
	reopened["databaseId"] = float64(936)
	reopened["number"] = float64(936)
	reopened["title"] = "Reopened unit"
	reopened["state"] = "OPEN"
	reopened["updatedAt"] = "2026-08-25T10:00:02Z"
	reopened["body"] = "### Dependencies\nnone"
	reopened["timelineItems"].(map[string]any)["nodes"].([]any)[0].(map[string]any)["id"] = "CE_936"
	closing["nodes"] = append(nodes, reopened)
	runner.responses["MergedPullRequest:300:"], _ = json.Marshal(mergedResponse)

	var openIssuesResponse map[string]any
	if err := json.Unmarshal(runner.responses["OpenIssues::"], &openIssuesResponse); err != nil {
		t.Fatal(err)
	}
	openIssue := openIssuesResponse["data"].(map[string]any)["repository"].(map[string]any)["issues"].(map[string]any)["nodes"].([]any)[0].(map[string]any)
	openIssue["body"] = "No tracker entries"
	runner.responses["OpenIssues::"], _ = json.Marshal(openIssuesResponse)

	snapshot, err := Collect(context.Background(), fixtureConfig(""), runner, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.MergedPullRequest.ClosingIssues) != 1 || !hasAmbiguity(snapshot.Ambiguities, "closing-issue-state") || len(snapshot.ContainingTrackers) != 0 {
		t.Fatalf("closing=%#v ambiguities=%#v trackers=%#v", snapshot.MergedPullRequest.ClosingIssues, snapshot.Ambiguities, snapshot.ContainingTrackers)
	}
	report := RenderReport(snapshot)
	if strings.Contains(report, "valid zero-work result") || !strings.Contains(report, "not a zero-work verdict") {
		t.Fatalf("unsafe mixed-attribution report: %s", report)
	}
}

func hasAmbiguity(ambiguities []Ambiguity, code string) bool {
	for _, ambiguity := range ambiguities {
		if ambiguity.Code == code {
			return true
		}
	}
	return false
}

func TestEveryGraphQLDocumentIsAQuery(t *testing.T) {
	for _, document := range allQueryDocuments {
		trimmed := strings.TrimSpace(document)
		if !strings.HasPrefix(trimmed, "query ") || strings.Contains(strings.ToLower(trimmed), "mutation") {
			t.Fatalf("non-query document: %s", document)
		}
	}
}
