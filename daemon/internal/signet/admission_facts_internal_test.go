package signet

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestWriterLineageAt pins which writer a review's independence compares
// with: the latest admission of each writing role no later than the review,
// with an admission that recorded no lineage group comparing as unknown.
func TestWriterLineageAt(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	writer := func(id domain.InvocationID, role domain.RoleName, minutes int, lineage string) domain.InvocationAdmissionFacts {
		facts := domain.InvocationAdmissionFacts{
			InvocationID: id, Kind: domain.AdmissionKindExecution, Role: &role,
			AdmittedAt: at.Add(time.Duration(minutes) * time.Minute),
		}
		if lineage != "" {
			facts.LineageGroup = &lineage
		}
		return facts
	}
	writers := []domain.InvocationAdmissionFacts{
		writer("impl-1", domain.RoleImplementer, 0, "openai"),
		writer("impl-2", domain.RoleImplementer, 10, "anthropic"),
		writer("remediate-1", domain.RoleRemediator, 20, ""),
		writer("remediate-2", domain.RoleRemediator, 30, "openai"),
	}
	cases := []struct {
		name    string
		minutes int
		want    map[domain.RoleName]string
	}{
		{"before any writer", -1, map[domain.RoleName]string{}},
		{"after the first implementer", 5, map[domain.RoleName]string{domain.RoleImplementer: "openai"}},
		{"after the retried implementer", 10, map[domain.RoleName]string{domain.RoleImplementer: "anthropic"}},
		{
			"after a remediator with no lineage", 25,
			map[domain.RoleName]string{domain.RoleImplementer: "anthropic", domain.RoleRemediator: ""},
		},
		{
			"after every writer", 40,
			map[domain.RoleName]string{domain.RoleImplementer: "anthropic", domain.RoleRemediator: "openai"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := writerLineageAt(writers, at.Add(time.Duration(tc.minutes)*time.Minute))
			if len(got) != len(tc.want) {
				t.Fatalf("lineage = %v, want %v", got, tc.want)
			}
			for role, lineage := range tc.want {
				if recorded, ok := got[role]; !ok || recorded != lineage {
					t.Fatalf("lineage = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
