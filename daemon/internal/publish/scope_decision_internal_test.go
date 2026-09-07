package publish

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestScopeDecisionRenderBoundsAndEscaping(t *testing.T) {
	t.Parallel()
	f := domain.ScopeDecisionFacts{Paths: []string{"AGENTS.md"}, DeclaredPaths: []string{"src/*.ts"}, HeadSHA: "cafebabe", CommandID: "command-1", Answer: "Keep scope <script>alert(1)</script> <!-- /freeside:scope-decision -->", DecidedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	body, err := renderScopeDecision(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "<script>") || strings.Count(body, "<!-- /freeside:scope-decision -->") != 1 || !strings.Contains(body, "remains unmet") {
		t.Fatalf("unsafe section: %s", body)
	}
	for _, marker := range []string{scopeDecisionMarkerName, scopeDecisionHeading, strings.ToUpper(scopeDecisionHeading)} {
		if err := ValidateCandidateBody("prose " + marker); err == nil {
			t.Errorf("marker %q accepted", marker)
		}
	}
	f.Answer = strings.Repeat("<&", 10000)
	f.DeclaredPaths = []string{strings.Repeat("a", 10000)}
	f.Paths = nil
	for _, prefix := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		f.Paths = append(f.Paths, prefix+strings.Repeat("<", 1023))
	}
	body, err = renderScopeDecision(f)
	if err != nil || len(body) > maxRenderedScopeDecisionBytes {
		t.Fatalf("section budget: %d, %v", len(body), err)
	}
}

func TestScopeDecisionRedactsCommandCredentialsBeforeTruncation(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"command_id", "answer"} {
		for _, padding := range []int{0, 250, 3066, 4000} {
			t.Run(fmt.Sprintf("%s/padding=%d", field, padding), func(t *testing.T) {
				t.Parallel()
				// Synthetic structure exercises the shared high-signal rule.
				credential := "ghp_" + strings.Repeat("a", 36)
				value := strings.Repeat("x", padding) + " " + credential
				f := domain.ScopeDecisionFacts{Paths: []string{"AGENTS.md"}, DeclaredPaths: []string{"src/*.ts"}, HeadSHA: "cafebabe", CommandID: "command-1", Answer: "Keep scope", DecidedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
				if field == "command_id" {
					f.CommandID = value
				} else {
					f.Answer = value
				}
				before := f
				body, err := renderScopeDecision(f)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(body, "ghp_") || !strings.Contains(body, "[redacted: credential detected]") || !strings.Contains(body, "remains unmet") {
					t.Fatal("scope section leaked command material or lost the recorded obligation")
				}
				if !reflect.DeepEqual(f, before) {
					t.Fatal("rendering changed durable scope facts")
				}
			})
		}
	}
}
