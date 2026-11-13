package inference_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

func duplicateSearchInput() inference.DuplicateSearchInput {
	return inference.DuplicateSearchInput{
		RunID:   "run-1",
		Finding: `{"message":"the sync client retries without a token-value budget"}`,
		Candidates: []inference.DuplicateSearchCandidate{
			{Handle: fake.DuplicateSearchMatchHandle, Title: "Bound sync retries", BodyExcerpt: "The client retries forever."},
			{Handle: "candidate-2", Title: "Unrelated cleanup", BodyExcerpt: ""},
		},
	}
}

func scriptDuplicateSearch(driver *fake.Driver, bodies ...string) {
	scripts := make([]fake.Script, 0, len(bodies))
	for _, body := range bodies {
		scripts = append(scripts, fake.Script{Response: inference.Response{Output: []byte(body), ComputeUnits: 1}})
	}
	driver.Script(inference.DuplicateSearchSiteID, scripts...)
}

func TestDuplicateSearchSiteIsAChooseSiteWhoseFailSafeIsNoAnswer(t *testing.T) {
	site := inference.DuplicateSearchSite(testBudget(10))
	if site.Authority != inference.AuthorityChoose || site.Annotation != nil ||
		site.Adjudication != nil || site.DriftAudit != nil {
		t.Fatalf("site authority = %#v", site)
	}
	// An unavailable site must never read as "new".
	if err := site.ValidateOutput([]byte(site.FailSafe)); err == nil {
		t.Fatal("the fail-safe passes the output schema")
	}
	role, ok := domain.RoleForSite(inference.DuplicateSearchSiteID)
	if !ok || role != domain.RoleFindingAdjudicator {
		t.Fatalf("role for site = %q, %t, want the finding adjudicator", role, ok)
	}
}

// The site sees handles, titles, and excerpts, never issue numbers.
func TestDuplicateSearchSendsExactlyTheDeclaredFields(t *testing.T) {
	driver := fake.New()
	scriptDuplicateSearch(driver, fake.DuplicateSearchNew)
	client, _, _ := testClient(t, driver, 10)
	if _, err := client.SearchDuplicates(context.Background(), "project-1", "run-1", duplicateSearchInput()); err != nil {
		t.Fatal(err)
	}
	requests := driver.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %#v", requests)
	}
	fields := requests[0].Fields
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := []string{"candidates", "finding", "run_id"}; !slices.Equal(names, want) {
		t.Fatalf("sent fields = %v, want %v", names, want)
	}
	want := `[{"handle":"candidate-1","title":"Bound sync retries","body_excerpt":"The client retries forever."},` +
		`{"handle":"candidate-2","title":"Unrelated cleanup","body_excerpt":""}]`
	if fields["candidates"] != want {
		t.Fatalf("candidates = %s", fields["candidates"])
	}
	if fields["finding"] != `{"message":"the sync client retries without a [REDACTED] budget"}` {
		t.Fatalf("finding = %s", fields["finding"])
	}
}

func TestDuplicateSearchAnswers(t *testing.T) {
	cases := map[string]struct {
		body        string
		duplicateOf string
	}{
		"new":   {fake.DuplicateSearchNew, ""},
		"match": {fake.DuplicateSearchMatch, fake.DuplicateSearchMatchHandle},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			driver := fake.New()
			scriptDuplicateSearch(driver, tc.body)
			client, _, _ := testClient(t, driver, 10)
			answer, err := client.SearchDuplicates(context.Background(), "project-1", "run-1", duplicateSearchInput())
			if err != nil {
				t.Fatal(err)
			}
			if answer.DuplicateOf != tc.duplicateOf || answer.Confidence != domain.ConfidenceHigh || answer.Reason == "" {
				t.Fatalf("answer = %#v", answer)
			}
		})
	}
}

// Tamper: an answer naming a handle the input did not show is refused, as is
// every malformed or unavailable answer, so nothing reads as "new".
func TestDuplicateSearchRefusesUnshownHandleAndMalformedAnswers(t *testing.T) {
	cases := map[string]struct {
		driver inference.Driver
		calls  int
	}{
		"unknown handle": {scripted(fake.DuplicateSearchUnknownHandle), 1},
		"malformed":      {scripted(fake.DuplicateSearchMalformed), 1},
		"the fail-safe":  {scripted(`{"duplicate_of":null}`), 1},
		"an issue number": {scripted(
			`{"duplicate_of":17,"confidence":"high","reason":"issue 17 tracks it"}`), 1},
		"case-folded duplicate key": {scripted(
			`{"duplicate_of":null,"Duplicate_Of":"candidate-1","confidence":"high","reason":"x"}`), 1},
		"multi-line reason": {scripted(
			`{"duplicate_of":null,"confidence":"high","reason":"one\ntwo"}`), 1},
		"blank reason": {scripted(`{"duplicate_of":null,"confidence":"high","reason":" "}`), 1},
		"empty handle": {scripted(`{"duplicate_of":"","confidence":"high","reason":"x"}`), 1},
		"driver error": {func() inference.Driver {
			driver := fake.New()
			driver.Script(inference.DuplicateSearchSiteID, fake.Script{Err: errors.New("provider refused")})
			return driver
		}(), 1},
		"inference down": {nil, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client, _, _ := testClient(t, tc.driver, 10)
			answer, err := client.SearchDuplicates(context.Background(), "project-1", "run-1", duplicateSearchInput())
			if !errors.Is(err, inference.ErrDuplicateSearchNotAvailable) || answer != (inference.DuplicateSearchAnswer{}) {
				t.Fatalf("SearchDuplicates = %#v, %v", answer, err)
			}
			if driver, ok := tc.driver.(*fake.Driver); ok && len(driver.Requests()) != tc.calls {
				t.Fatalf("provider calls = %d, want %d", len(driver.Requests()), tc.calls)
			}
		})
	}
}

func scripted(body string) *fake.Driver {
	driver := fake.New()
	scriptDuplicateSearch(driver, body)
	return driver
}
