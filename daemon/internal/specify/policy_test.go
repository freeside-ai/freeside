package specify_test

import (
	"reflect"
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
		got.ResearchMaxBytes != 1<<20 || len(got.ResearchAllowlist) != 2 {
		t.Fatalf("policy = %+v", got)
	}

	// Policies resolved before the blocked attention type was retired still
	// carry its waiting threshold, and a stored one must keep parsing.
	withRetired, err := domain.NewResolvedPolicy("run-spec", append(keys,
		policyKey("waiting.spec_approval_attention_after", "4h")))
	if err != nil {
		t.Fatal(err)
	}
	if kept, err := specify.ParsePolicy(withRetired); err != nil || !reflect.DeepEqual(kept, got) {
		t.Fatalf("policy with the retired waiting key = %+v, %v; want %+v", kept, err, got)
	}
}

func TestParsePolicyRejectsResearchResourceOverflow(t *testing.T) {
	base := []domain.PolicyKey{
		policyKey(specify.PolicySpecApproval, "true"),
		policyKey(specify.PolicyMaxIterations, "3"),
		policyKey(specify.PolicyStageActiveTime, "45m"),
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
