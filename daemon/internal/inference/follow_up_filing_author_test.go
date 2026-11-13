package inference_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

func followUpFilingAuthorInput() inference.FollowUpFilingAuthorInput {
	return inference.FollowUpFilingAuthorInput{
		RunID: "run-1", Repository: "owner/repo",
		Finding:       `{"message":"the sync client retries without a budget","rationale":"adjacent work"}`,
		DeclaredPaths: []string{"daemon/"},
	}
}

func scriptedAuthor(body string) *fake.Driver {
	driver := fake.New()
	driver.Script(inference.FollowUpFilingAuthorSiteID,
		fake.Script{Response: inference.Response{Output: []byte(body), ComputeUnits: 1}})
	return driver
}

// authorAnswer is a full, valid answer with one field replaced by value, or
// removed when value is nil, or added when the key is not one of the seven.
func authorAnswer(t *testing.T, key string, value any) string {
	t.Helper()
	answer := map[string]any{}
	if err := json.Unmarshal([]byte(fake.FollowUpFilingAuthorDraft), &answer); err != nil {
		t.Fatal(err)
	}
	if value == nil {
		delete(answer, key)
	} else {
		answer[key] = value
	}
	body, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestFollowUpFilingAuthorSiteIsAProposeSiteWhoseFailSafeIsNoDraft(t *testing.T) {
	site := inference.FollowUpFilingAuthorSite(testBudget(10))
	if site.Authority != inference.AuthorityPropose || site.Instruction == "" || site.Annotation != nil ||
		site.Adjudication != nil || site.DriftAudit != nil {
		t.Fatalf("site authority = %#v", site)
	}
	// An unavailable site must never read as a draft.
	if err := site.ValidateOutput([]byte(site.FailSafe)); err == nil {
		t.Fatal("the fail-safe passes the output schema")
	}
	role, ok := domain.RoleForSite(inference.FollowUpFilingAuthorSiteID)
	if !ok || role != domain.RoleFindingAdjudicator {
		t.Fatalf("role for site = %q, %t, want the finding adjudicator", role, ok)
	}
}

func TestFollowUpFilingAuthorSendsExactlyTheDeclaredFields(t *testing.T) {
	driver := scriptedAuthor(fake.FollowUpFilingAuthorDraft)
	client, _, _ := testClient(t, driver, 10)
	if _, err := client.AuthorFollowUpFiling(context.Background(), "project-1", "run-1",
		followUpFilingAuthorInput()); err != nil {
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
	if want := []string{"declared_paths", "finding", "repository", "run_id"}; !slices.Equal(names, want) {
		t.Fatalf("sent fields = %v, want %v", names, want)
	}
	if fields["declared_paths"] != `["daemon/"]` || fields["repository"] != "owner/repo" {
		t.Fatalf("fields = %#v", fields)
	}
}

func TestFollowUpFilingAuthorReturnsTheDraft(t *testing.T) {
	client, _, _ := testClient(t, scriptedAuthor(fake.FollowUpFilingAuthorDraft), 10)
	draft, err := client.AuthorFollowUpFiling(context.Background(), "project-1", "run-1", followUpFilingAuthorInput())
	if err != nil {
		t.Fatal(err)
	}
	if draft.Kind != domain.FollowUpFilingKindFix || draft.Title != "Bound the sync client's retries" ||
		draft.AffectedContracts != "none" || draft.Scope != "daemon/a.go" ||
		!strings.Contains(draft.Acceptance, "\n- A push that succeeds") {
		t.Fatalf("draft = %#v", draft)
	}
}

// Tamper: a kind outside the enum, a missing or extra key, a field over its
// cap, and every malformed or unavailable answer yields no draft, so the
// caller composes the filing as before and no label comes from the answer.
func TestFollowUpFilingAuthorRefusesMalformedAnswers(t *testing.T) {
	cases := map[string]struct {
		driver inference.Driver
		calls  int
	}{
		"kind outside the enum": {scriptedAuthor(fake.FollowUpFilingAuthorBadKind), 1},
		"kind of another case":  {scriptedAuthor(authorAnswer(t, "kind", "Fix")), 1},
		"no kind":               {scriptedAuthor(authorAnswer(t, "kind", nil)), 1},
		"a null kind":           {scriptedAuthor(strings.Replace(fake.FollowUpFilingAuthorDraft, `"fix"`, `null`, 1)), 1},
		"an extra key":          {scriptedAuthor(fake.FollowUpFilingAuthorMalformed), 1},
		"a missing key":         {scriptedAuthor(authorAnswer(t, "scope", nil)), 1},
		"case-folded duplicate key": {scriptedAuthor(strings.Replace(fake.FollowUpFilingAuthorDraft,
			`"kind":"fix"`, `"kind":"fix","Kind":"chore"`, 1)), 1},
		"a blank field":        {scriptedAuthor(authorAnswer(t, "objective", " \n ")), 1},
		"a multi-line title":   {scriptedAuthor(authorAnswer(t, "title", "One\nTwo")), 1},
		"a tab":                {scriptedAuthor(authorAnswer(t, "acceptance", "-\tIt stops.")), 1},
		"a carriage return":    {scriptedAuthor(authorAnswer(t, "scope", "daemon/\r\napp/")), 1},
		"a non-string field":   {scriptedAuthor(authorAnswer(t, "non_goals", []string{"none"})), 1},
		"a secret":             {scriptedAuthor(authorAnswer(t, "objective", "Rotate ghp_"+strings.Repeat("a", 36))), 1},
		"a title over its cap": {scriptedAuthor(authorAnswer(t, "title", strings.Repeat("x", domain.MaxFollowUpFilingTitleBytes+1))), 1},
		"a scope over its cap": {scriptedAuthor(authorAnswer(t, "scope", strings.Repeat("x", domain.MaxFollowUpFilingScopeBytes+1))), 1},
		"an objective over its cap": {scriptedAuthor(authorAnswer(t, "objective",
			strings.Repeat("x", domain.MaxFollowUpFilingObjectiveBytes+1))), 1},
		"the fail-safe": {scriptedAuthor(`{"kind":null}`), 1},
		"driver error": {func() inference.Driver {
			driver := fake.New()
			driver.Script(inference.FollowUpFilingAuthorSiteID, fake.Script{Err: errors.New("provider refused")})
			return driver
		}(), 1},
		"inference down": {nil, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client, _, _ := testClient(t, tc.driver, 10)
			draft, err := client.AuthorFollowUpFiling(context.Background(), "project-1", "run-1",
				followUpFilingAuthorInput())
			if !errors.Is(err, inference.ErrFollowUpFilingAuthorNotAvailable) || draft != (inference.FollowUpFilingDraft{}) {
				t.Fatalf("AuthorFollowUpFiling = %#v, %v", draft, err)
			}
			if driver, ok := tc.driver.(*fake.Driver); ok && len(driver.Requests()) != tc.calls {
				t.Fatalf("provider calls = %d, want %d", len(driver.Requests()), tc.calls)
			}
		})
	}
}

// Each field at exactly its cap is accepted.
func TestFollowUpFilingAuthorAcceptsFieldsAtTheirCaps(t *testing.T) {
	for key, limit := range map[string]int{
		"title":              domain.MaxFollowUpFilingTitleBytes,
		"objective":          domain.MaxFollowUpFilingObjectiveBytes,
		"non_goals":          domain.MaxFollowUpFilingNonGoalsBytes,
		"affected_contracts": domain.MaxFollowUpFilingAffectedContractsBytes,
		"acceptance":         domain.MaxFollowUpFilingAcceptanceBytes,
		"scope":              domain.MaxFollowUpFilingScopeBytes,
	} {
		t.Run(key, func(t *testing.T) {
			client, _, _ := testClient(t, scriptedAuthor(authorAnswer(t, key, strings.Repeat("x", limit))), 10)
			if _, err := client.AuthorFollowUpFiling(context.Background(), "project-1", "run-1",
				followUpFilingAuthorInput()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
