package signet_test

import (
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

// TestOpenAPIEnumsMatchDomain pins the cross-language registration seam:
// Swift's generated CaseIterable enums come from this OpenAPI set, so any Go
// vocabulary change must update the wire contract and regenerated client in
// the same unit.
func TestOpenAPIEnumsMatchDomain(t *testing.T) {
	body, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Enum []string `yaml:"enum"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"Action":                    enumStrings(domain.AllActions),
		"AttentionType":             enumStrings(domain.AllAttentionTypes),
		"RecommendationSource":      enumStrings(domain.AllRecommendationSources),
		"JudgmentSite":              enumStrings(domain.AllJudgmentSites),
		"DisplayNameSource":         enumStrings(domain.AllDisplayNameSources),
		"StageName":                 enumStrings(domain.AllStageNames),
		"BlockedWaitKind":           enumStrings(domain.AllBlockedWaitKinds),
		"BlockedKind":               enumStrings(domain.AllBlockedKinds),
		"AnswerRoute":               enumStrings(domain.AllAnswerRoutes),
		"ObservedInvocationStatus":  enumStrings(domain.AllObservedInvocationStatuses),
		"ExecutionOutcomeStatus":    enumStrings(domain.AllExecutionOutcomeStatuses),
		"ImpairedCapability":        enumStrings(domain.AllImpairedCapabilities),
		"TrustRule":                 enumStrings(domain.AllTrustRules),
		"RunMilestoneKind":          enumStrings(domain.AllRunMilestoneKinds),
		"RunHoldReason":             enumStrings(domain.AllRunHoldReasons),
		"RunOutcome":                enumStrings(domain.AllRunOutcomes),
		"AdjudicationProducer":      enumStrings(domain.AllAdjudicationProducers),
		"EvidenceMediaType":         enumStrings(domain.AllEvidenceMediaTypes),
		"EvidenceSource":            enumStrings(domain.AllEvidenceSources),
		"EvidenceAvailability":      enumStrings(domain.AllEvidenceAvailabilities),
		"ConnectionMode":            enumStrings(domain.AllConnectionModes),
		"DeviceScope":               enumStrings(domain.AllDeviceScopes),
		"ReadinessRequirementState": enumStrings(domain.AllReadinessRequirementStates),
		"VerificationCheckClass":    enumStrings(domain.AllVerificationCheckClasses),
		"RequirementKind":           enumStrings(domain.AllRequirementKinds),
		"WaiverGrantingAuthority":   enumStrings(domain.AllWaiverGrantingAuthorities),
	}
	for name, expected := range want {
		got := slices.Clone(document.Components.Schemas[name].Enum)
		slices.Sort(got)
		slices.Sort(expected)
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("OpenAPI %s = %v, domain registration = %v", name, got, expected)
		}
	}
}

func enumStrings[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = string(value)
	}
	return out
}
