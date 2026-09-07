package domain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func validScopeConflict() domain.ScopeConflict {
	return domain.ScopeConflict{Version: domain.ScopeConflictEncodingVersion, Paths: []string{"AGENTS.md"}, Decision: domain.Decision{
		Question: "Keep scope?", WhyBlocking: "Required helper documentation is outside scope.",
		Options: []domain.DecisionOption{{Label: "Keep", Tradeoffs: "Documentation remains stale."}, {Label: "Stop", Tradeoffs: "Start a new run."}}, Recommendation: "Stop",
	}}
}

func TestScopeConflictContract(t *testing.T) {
	t.Parallel()
	conflict := validScopeConflict()
	body, err := domain.EncodeScopeConflict(conflict)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"unknown":                      []byte(strings.Replace(string(body), `"version":`, `"extra":1,"version":`, 1)),
		"duplicate invalid final path": []byte(strings.Replace(string(body), `"paths":["AGENTS.md"]`, `"paths":["AGENTS.md"],"paths":["../outside"]`, 1)),
		"trailing":                     append(append([]byte{}, body...), []byte(` {}`)...),
		"oversized":                    []byte(strings.Repeat(" ", int(domain.MaxScopeConflictBytes)+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := domain.DecodeScopeConflict(data); err == nil {
				t.Fatal("invalid document accepted")
			}
		})
	}
	for _, p := range []string{".", "../AGENTS.md", "/AGENTS.md", "docs//AGENTS.md", "docs/./AGENTS.md", "x\x00", strings.Repeat("a", 1025)} {
		bad := conflict
		bad.Paths = []string{p}
		if err := bad.Validate(); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	facts := domain.ScopeConflictFacts{Paths: conflict.Paths, DeclaredPaths: []string{"devlog/*.md", "src/*.ts"}, HeadSHA: "cafebabe"}
	if err := facts.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, declared := range [][]string{{"**"}, {"AGENTS.md"}, {"**/*.md"}} {
		bad := facts
		bad.DeclaredPaths = declared
		if err := bad.Validate(); err == nil {
			t.Errorf("in-scope path accepted under %v", declared)
		}
	}
	decision := domain.ScopeDecisionFacts{Paths: facts.Paths, DeclaredPaths: facts.DeclaredPaths, HeadSHA: facts.HeadSHA, CommandID: "scope-1", Answer: "Keep scope; document the remaining obligation.", DecidedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	for name, v := range map[string]any{"scope_conflict": conflict, "scope_conflict_facts": facts, "scope_decision_facts": decision} {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		golden.Assert(t, name, data)
	}
}
