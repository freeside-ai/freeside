package inference_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

func TestTaskNamerSiteAndClaim(t *testing.T) {
	site := inference.TaskNamerSite(testBudget(1))
	if site.ID != string(domain.JudgmentSiteTaskNamer) || site.Authority != inference.AuthorityExplain ||
		site.FailSafe != `{"name":""}` || site.MaxInputBytes != 64<<10 || site.MaxOutputBytes != 1<<10 ||
		site.Retention != 14*24*time.Hour || site.Timeout != 30*time.Second || site.AuditEvery != 10 {
		t.Fatalf("namer contract = %+v", site)
	}
	driver := fake.New()
	driver.Script(site.ID, fake.Script{Response: inference.Response{Output: []byte(`{"name":"Bound request bodies"}`), ComputeUnits: 1}})
	client, claims, _ := testClient(t, driver, 1)
	input := inference.TaskNamerInput{
		Project: "project-1", RootLineage: "run-1", SourceKind: "work_item_artifact", SourceText: "Bound request bodies; token-value",
	}
	name, fallback, err := client.NameTask(t.Context(), input)
	if err != nil || fallback || name != "Bound request bodies" {
		t.Fatalf("NameTask = %q, %t, %v", name, fallback, err)
	}
	requests := driver.Requests()
	if len(requests) != 1 || len(requests[0].Fields) != 6 || requests[0].Fields["source_text"] != "Bound request bodies; [REDACTED]" ||
		requests[0].Fields["source_kind"] != input.SourceKind || requests[0].Fields["repository"] != "" ||
		requests[0].Fields["issue_number"] != "" || requests[0].Fields["issue_title"] != "" || requests[0].Fields["issue_body"] != "" {
		t.Fatalf("outbound fields = %+v", requests)
	}
	entries, err := claims.List(t.Context())
	if err != nil || len(entries) != 2 || entries[1].Kind != "task_name_claim" || entries[1].Producer != "fake/test" ||
		entries[1].Body != name || entries[1].InputDigest != requests[0].InputDigest || entries[1].RootLineage != input.RootLineage {
		t.Fatalf("advisory entries = %+v, %v", entries, err)
	}
	if name, fallback, err := client.NameTask(t.Context(), input); err != nil || !fallback || name != "" || len(driver.Requests()) != 1 {
		t.Fatalf("budget fallback = %q, %t, %v", name, fallback, err)
	}
}

func TestTaskNamerValidationAndUnavailableFallback(t *testing.T) {
	for _, tc := range []struct {
		label, output string
		err           error
		unbound       bool
		valid         bool
	}{
		{label: "valid unicode boundary", output: `{"name":"` + strings.Repeat("界", 60) + `"}`, valid: true},
		{label: "too long", output: `{"name":"` + strings.Repeat("界", 61) + `"}`},
		{label: "empty", output: `{"name":""}`},
		{label: "whitespace", output: `{"name":"  "}`},
		{label: "untrimmed", output: `{"name":" Name "}`},
		{label: "newline", output: `{"name":"Name\nAgain"}`},
		{label: "carriage return", output: `{"name":"Name\rAgain"}`},
		{label: "secret", output: `{"name":"ghp_` + strings.Repeat("x", 36) + `"}`},
		{label: "extra field", output: `{"name":"Name task","approve":true}`},
		{label: "duplicate field", output: `{"name":"Name task","name":"Replace name"}`},
		{label: "trailing object", output: `{"name":"Name task"}{}`},
		{label: "invalid utf8", output: "{\"name\":\"\xff\"}"},
		{label: "driver error", err: errors.New("unavailable")},
		{label: "unbound", unbound: true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			driver := fake.New()
			driver.Script(inference.TaskNamerSiteID, fake.Script{Response: inference.Response{Output: []byte(tc.output)}, Err: tc.err})
			var bound inference.Driver = driver
			if tc.unbound {
				bound = nil
			}
			client, claims, _ := testClient(t, bound, 10)
			name, fallback, err := client.NameTask(t.Context(), inference.TaskNamerInput{Project: "p", RootLineage: "r", SourceKind: "work_item_artifact", SourceText: "Name this task"})
			if err != nil || fallback == tc.valid || (name != "") != tc.valid {
				t.Fatalf("NameTask = %q, %t, %v", name, fallback, err)
			}
			entries, err := claims.List(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if !tc.valid && entry.Kind == "task_name_claim" {
					t.Fatal("fallback persisted a name claim")
				}
			}
			if tc.valid {
				var expected struct{ Name string }
				if err := json.Unmarshal([]byte(tc.output), &expected); err != nil || name != expected.Name {
					t.Fatalf("name = %q, expected %q", name, expected.Name)
				}
			}
		})
	}
}
