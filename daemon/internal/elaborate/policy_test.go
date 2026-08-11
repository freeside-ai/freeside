package elaborate_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
)

func TestParsePolicy(t *testing.T) {
	keys := []domain.PolicyKey{
		policyKey(elaborate.PolicySpecApproval, "true"),
		policyKey(elaborate.PolicyMaxIterations, "3"),
		policyKey(elaborate.PolicyStageActiveTime, "45m"),
		policyKey(elaborate.PolicyApprovalWait, "4h"),
		policyKey(elaborate.PolicyResearchAllowlist, "https://docs.example, https://api.example:8443"),
		policyKey(elaborate.PolicyResearchMaxBytes, "1048576"),
	}
	resolved, err := domain.NewResolvedPolicy("run-elab", keys)
	if err != nil {
		t.Fatal(err)
	}
	got, err := elaborate.ParsePolicy(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SpecApproval || got.MaxIterations != 3 || got.StageActiveTime != 45*time.Minute ||
		got.ApprovalWait != 4*time.Hour || got.ResearchMaxBytes != 1<<20 ||
		len(got.ResearchAllowlist) != 2 {
		t.Fatalf("policy = %+v", got)
	}
}

func TestParsePolicyRejectsResearchResourceOverflow(t *testing.T) {
	base := []domain.PolicyKey{
		policyKey(elaborate.PolicySpecApproval, "true"),
		policyKey(elaborate.PolicyMaxIterations, "3"),
		policyKey(elaborate.PolicyStageActiveTime, "45m"),
		policyKey(elaborate.PolicyApprovalWait, "4h"),
		policyKey(elaborate.PolicyResearchAllowlist, "https://docs.example"),
		policyKey(elaborate.PolicyResearchMaxBytes, "1048576"),
	}
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{"iterations", elaborate.PolicyMaxIterations, strconv.Itoa(elaborate.MaxElaborationIterations + 1)},
		{"response bytes", elaborate.PolicyResearchMaxBytes, strconv.Itoa(elaborate.MaxResearchResponseBytes + 1)},
		{
			"allowlist entries", elaborate.PolicyResearchAllowlist,
			strings.Repeat("https://docs.example,", elaborate.MaxResearchAllowlistEntries) + "https://docs.example",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys := append([]domain.PolicyKey(nil), base...)
			for i := range keys {
				if keys[i].Key == tc.key {
					keys[i].Value = tc.value
				}
			}
			resolved, err := domain.NewResolvedPolicy("run-elab", keys)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := elaborate.ParsePolicy(resolved); err == nil {
				t.Fatal("ParsePolicy accepted an unbounded research resource")
			}
		})
	}
}

func policyKey(key, value string) domain.PolicyKey {
	return domain.PolicyKey{Key: key, Value: value, Provenance: domain.KeyProvenance{
		Source: domain.ProvenanceOverride, Digest: "sha256:source",
	}}
}
