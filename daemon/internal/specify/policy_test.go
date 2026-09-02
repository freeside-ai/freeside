package specify_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
)

func TestParsePolicy(t *testing.T) {
	keys := []domain.PolicyKey{
		policyKey(specify.PolicySpecApproval, "true"),
		policyKey(specify.PolicyMaxIterations, "3"),
		policyKey(specify.PolicyStageActiveTime, "45m"),
		policyKey(specify.PolicyApprovalWait, "4h"),
		policyKey(specify.PolicyResearchAllowlist, "https://docs.example, https://api.example:8443"),
		policyKey(specify.PolicyResearchMaxBytes, "1048576"),
	}
	resolved, err := domain.NewResolvedPolicy("run-spec", keys)
	if err != nil {
		t.Fatal(err)
	}
	got, err := specify.ParsePolicy(resolved)
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
		policyKey(specify.PolicySpecApproval, "true"),
		policyKey(specify.PolicyMaxIterations, "3"),
		policyKey(specify.PolicyStageActiveTime, "45m"),
		policyKey(specify.PolicyApprovalWait, "4h"),
		policyKey(specify.PolicyResearchAllowlist, "https://docs.example"),
		policyKey(specify.PolicyResearchMaxBytes, "1048576"),
	}
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{"iterations", specify.PolicyMaxIterations, strconv.Itoa(specify.MaxSpecificationIterations + 1)},
		{"response bytes", specify.PolicyResearchMaxBytes, strconv.Itoa(specify.MaxResearchResponseBytes + 1)},
		{
			"allowlist entries", specify.PolicyResearchAllowlist,
			strings.Repeat("https://docs.example,", specify.MaxResearchAllowlistEntries) + "https://docs.example",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys := append([]domain.PolicyKey(nil), base...)
			for i := range keys {
				if keys[i].Key == tc.key {
					keys[i].Value = tc.value
				}
			}
			resolved, err := domain.NewResolvedPolicy("run-spec", keys)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := specify.ParsePolicy(resolved); err == nil {
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
