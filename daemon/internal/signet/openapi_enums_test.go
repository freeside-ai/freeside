package signet_test

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

// openAPIEnumExemptions names the enum schemas that have no domain
// registration to compare against, each with the reason it has none.
var openAPIEnumExemptions = map[string]string{
	"NullRecipeDigest": "null-only marker for an absent recipe digest; no domain type",
}

// TestOpenAPIEnumsMatchDomain pins the cross-language registration seam:
// Swift's generated CaseIterable enums come from this OpenAPI set, so any Go
// vocabulary change must update the wire contract and regenerated client in
// the same unit. It covers every enum schema in the spec, so a new one fails
// until it is mapped to its domain registration or exempted with a reason.
func TestOpenAPIEnumsMatchDomain(t *testing.T) {
	t.Parallel()
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
		"Action":                     enumStrings(domain.AllActions),
		"AttentionType":              enumStrings(domain.AllAttentionTypes),
		"EffectKind":                 enumStrings(domain.AllEffectKinds),
		"ClosureProvenance":          enumStrings(domain.AllClosureProvenances),
		"ClosureFlagOrigin":          enumStrings(domain.AllClosureFlagOrigins),
		"IssueTextRuleset":           enumStrings(domain.AllIssueTextRulesets),
		"ScreeningVerdict":           enumStrings(domain.AllScreeningVerdicts),
		"FollowUpSourceKind":         enumStrings(domain.AllFollowUpSourceKinds),
		"ComprehensionEventKind":     enumStrings(domain.AllComprehensionEventKinds),
		"RecommendationSource":       enumStrings(domain.AllRecommendationSources),
		"JudgmentSite":               enumStrings(domain.AllJudgmentSites),
		"DisplayNameSource":          enumStrings(domain.AllDisplayNameSources),
		"TaskSubmissionOrigin":       enumStrings(domain.AllTaskSubmissionOrigins),
		"StageName":                  enumStrings(domain.AllStageNames),
		"TaskLineRole":               enumStrings(domain.TaskLineRoles),
		"TaskLineSource":             enumStrings(domain.AllTaskLineSources),
		"TaskLineSelectionTier":      enumStrings(domain.AllTaskLineSelectionTiers),
		"TriageOutcome":              enumStrings(domain.AllTriageOutcomes),
		"TriageTier":                 enumStrings(domain.AllTriageTiers),
		"BlockedWaitKind":            enumStrings(domain.AllBlockedWaitKinds),
		"BlockedKind":                enumStrings(domain.AllBlockedKinds),
		"AnswerRoute":                enumStrings(domain.AllAnswerRoutes),
		"ConclusionCause":            enumStrings(domain.AllConclusionCauses),
		"ObservedInvocationStatus":   enumStrings(domain.AllObservedInvocationStatuses),
		"ExecutionOutcomeStatus":     enumStrings(domain.AllExecutionOutcomeStatuses),
		"FailureCause":               enumStrings(domain.AllFailureCauses),
		"InterpretationFailureCause": enumStrings(domain.AllInterpretationFailureCauses),
		"ImpairedCapability":         enumStrings(domain.AllImpairedCapabilities),
		"EgressProfile":              enumStrings(domain.AllEgressProfiles),
		"TrustRule":                  enumStrings(domain.AllTrustRules),
		"RunMilestoneKind":           enumStrings(domain.AllRunMilestoneKinds),
		"ReviewSubjectKind":          enumStrings(domain.AllReviewSubjectKinds),
		"TaskEventKind":              enumStrings(domain.AllTaskEventKinds),
		"TaskRunRole":                enumStrings(domain.AllTaskRunRoles),
		"RunHoldReason":              enumStrings(domain.AllRunHoldReasons),
		"AttemptPurpose":             enumStrings(domain.AllAttemptPurposes),
		"AdmissionCheck":             enumStrings(domain.AllAdmissionChecks),
		"AgentSelectionPart":         enumStrings(domain.AllAgentSelectionParts),
		"ImageUpkeepPending":         enumStrings(domain.AllImageUpkeepPendings),
		"RunOutcome":                 enumStrings(domain.AllRunOutcomes),
		"RunLifecycle":               enumStrings(domain.AllRunLifecycles),
		"AdjudicationProducer":       enumStrings(domain.AllAdjudicationProducers),
		"DriftVerdict":               enumStrings(domain.AllDriftVerdicts),
		"ReviewDiminishingCause":     enumStrings(domain.AllReviewDiminishingCauses),
		"ReviewAttention":            enumStrings(domain.AllReviewAttentions),
		"ReviewExcerptSide":          enumStrings(domain.AllReviewExcerptSides),
		"ReviewExcerptUnresolved":    enumStrings(domain.AllReviewExcerptUnresolved),
		"EvidenceMediaType":          enumStrings(domain.AllEvidenceMediaTypes),
		"EvidenceSource":             enumStrings(domain.AllEvidenceSources),
		"EvidenceAvailability":       enumStrings(domain.AllEvidenceAvailabilities),
		"ConnectionMode":             enumStrings(domain.AllConnectionModes),
		"DeviceScope":                enumStrings(domain.AllDeviceScopes),
		"ReadinessRequirementState":  enumStrings(domain.AllReadinessRequirementStates),
		"VerificationCheckClass":     enumStrings(domain.AllVerificationCheckClasses),
		"RequirementKind":            enumStrings(domain.AllRequirementKinds),
		"WaiverGrantingAuthority":    enumStrings(domain.AllWaiverGrantingAuthorities),
		"AdmissionKind":              enumStrings(domain.AllAdmissionKinds),
		"RoleName":                   enumStrings(domain.AllRoleNames),
		"LineageRelation":            enumStrings(domain.AllLineageRelations),
		"HarnessClientKind":          enumStrings(domain.AllHarnessClientKinds),
		"EffortLevel":                enumStrings(domain.AllEffortLevels),
		"AgentSelectionSource":       enumStrings(domain.AllAgentSelectionSources),
		"CredentialMode":             enumStrings(domain.AllCredentialModes),
		"OperatingMode":              enumStrings(domain.AllOperatingModes),
		"CollectionAttemptStatus":    enumStrings(domain.AllCollectionAttemptStatuses),
		"AvailabilityAnswer":         enumStrings(domain.AllAvailabilityAnswers),
		"WindowStatus":               enumStrings(domain.AllWindowStatuses),
		"ExhaustedMarkSource":        enumStrings(domain.AllExhaustedMarkSources),
		"RoutingPolicySource":        enumStrings(domain.AllRoutingPolicySources),

		"AdjudicationConfidence":      enumStrings(domain.AllAdjudicationConfidences),
		"AdjudicationRoute":           enumStrings(domain.AllAdjudicationRoutes),
		"AlternateAgentQualification": enumStrings(domain.AllAlternateAgentQualifications),
		"Author":                      enumStrings(domain.AllAuthors),
		"CommitPlanNoticeReason":      enumStrings(domain.AllCommitPlanNoticeReasons),
		"ConversationStatus":          enumStrings(domain.AllConversationStatuses),
		"GoalRelationship":            enumStrings(domain.AllGoalRelationships),
		"HealthPosture":               enumStrings(domain.AllHealthPostures),
		"InterruptionClass":           enumStrings(domain.AllInterruptionClasses),
		"ItemStatus":                  enumStrings(domain.AllItemStatuses),
		"Priority":                    enumStrings(domain.AllPriorities),
		"ReadinessInvalidationReason": enumStrings(domain.AllReadinessInvalidationReasons),
		"ReviewEvidenceAvailability":  enumStrings(domain.AllReviewEvidenceAvailabilities),
		"ReviewOutcome":               enumStrings(domain.AllReviewOutcomes),
		"ReviewProgressState":         enumStrings(domain.AllReviewProgressStates),
		"ScheduleKind":                enumStrings(domain.AllScheduleKinds),
		"ScheduleResolutionReason":    enumStrings(domain.AllScheduleResolutionReasons),
		"ScheduleStatus":              enumStrings(domain.AllScheduleStatuses),
		"ScheduleSubjectType":         enumStrings(domain.AllScheduleSubjectTypes),
		"SensitivityClass":            enumStrings(domain.AllSensitivityClasses),
		"SupersessionKind":            enumStrings(domain.AllSupersessionKinds),
		"TaskAttachmentMediaType":     enumStrings(domain.AllTaskAttachmentMediaTypes),
		"TaskCancellationState":       enumStrings(domain.AllTaskCancellationStates),
		"TaskProposalIntent":          enumStrings(domain.AllTaskProposalIntents),
		"UnattendedAdmission":         enumStrings(domain.AllUnattendedAdmissions),
		"UnattendedStopKind":          enumStrings(domain.AllUnattendedStopKinds),
		"WorkUnitCompatibility":       enumStrings(domain.AllWorkUnitCompatibilities),
	}
	// A schema with an enum key decodes to a non-nil slice; enum: [null]
	// decodes to [""].
	spec := map[string][]string{}
	for name, schema := range document.Components.Schemas {
		if schema.Enum != nil {
			spec[name] = schema.Enum
		}
	}
	for _, problem := range openAPIEnumMismatches(spec, want, openAPIEnumExemptions) {
		t.Error(problem)
	}
}

// openAPIEnumMismatches compares each enum schema in spec with its domain
// registration in want, skipping the names only in exempt, and returns one
// message per problem in schema-name order. A want or exempt entry whose
// schema is absent from spec is a problem too, so a renamed or removed
// schema can't leave an entry behind that checks nothing.
func openAPIEnumMismatches(spec, want map[string][]string, exempt map[string]string) []string {
	names := slices.Concat(
		slices.Collect(maps.Keys(spec)),
		slices.Collect(maps.Keys(want)),
		slices.Collect(maps.Keys(exempt)),
	)
	slices.Sort(names)
	names = slices.Compact(names)

	var problems []string
	for _, name := range names {
		values, inSpec := spec[name]
		expected, mapped := want[name]
		_, exempted := exempt[name]
		if !inSpec {
			if mapped {
				problems = append(problems, fmt.Sprintf("domain registration mapped for %s, but the spec has no enum schema by that name", name))
			}
			if exempted {
				problems = append(problems, fmt.Sprintf("exemption listed for %s, but the spec has no enum schema by that name", name))
			}
			continue
		}
		switch {
		case exempted && mapped:
			problems = append(problems, fmt.Sprintf("OpenAPI enum schema %s is both mapped and exempted: drop the exemption so its values are checked", name))
		case exempted:
		case !mapped:
			problems = append(problems, fmt.Sprintf("OpenAPI enum schema %s is unchecked: map it to its domain registration or exempt it with a reason", name))
		default:
			got := slices.Sorted(slices.Values(values))
			registered := slices.Sorted(slices.Values(expected))
			if !slices.Equal(got, registered) {
				problems = append(problems, fmt.Sprintf("OpenAPI %s = %v, domain registration = %v", name, got, registered))
			}
		}
	}
	return problems
}

func TestOpenAPIEnumMismatches(t *testing.T) {
	t.Parallel()
	exempt := map[string]string{"Marker": "fixture"}
	tests := []struct {
		name string
		spec map[string][]string
		want map[string][]string
		// One substring per expected message, in order.
		problems []string
	}{
		{
			name: "all mapped or exempt",
			spec: map[string][]string{"Color": {"red", "blue"}, "Marker": {""}},
			want: map[string][]string{"Color": {"blue", "red"}},
		},
		{
			name:     "unmapped spec enum",
			spec:     map[string][]string{"Color": {"red"}, "Shape": {"square"}, "Marker": {""}},
			want:     map[string][]string{"Color": {"red"}},
			problems: []string{"schema Shape is unchecked: map it to its domain registration or exempt it"},
		},
		{
			name:     "mapped schema missing from spec",
			spec:     map[string][]string{"Color": {"red"}, "Marker": {""}},
			want:     map[string][]string{"Color": {"red"}, "Shape": {"square"}},
			problems: []string{"mapped for Shape, but the spec has no enum schema"},
		},
		{
			name:     "exempt schema missing from spec",
			spec:     map[string][]string{"Color": {"red"}},
			want:     map[string][]string{"Color": {"red"}},
			problems: []string{"exemption listed for Marker, but the spec has no enum schema"},
		},
		{
			name:     "schema both mapped and exempt",
			spec:     map[string][]string{"Marker": {""}},
			want:     map[string][]string{"Marker": {""}},
			problems: []string{"schema Marker is both mapped and exempted"},
		},
		{
			name:     "values differ",
			spec:     map[string][]string{"Color": {"red", "green"}, "Marker": {""}},
			want:     map[string][]string{"Color": {"red", "blue"}},
			problems: []string{"OpenAPI Color = [green red], domain registration = [blue red]"},
		},
		{
			name: "problems in schema-name order",
			spec: map[string][]string{"Zebra": {"z"}, "Color": {"red"}, "Apple": {"a"}, "Marker": {""}},
			want: map[string][]string{"Color": {"blue"}, "Mango": {"m"}},
			problems: []string{
				"schema Apple is unchecked",
				"OpenAPI Color =",
				"mapped for Mango,",
				"schema Zebra is unchecked",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := openAPIEnumMismatches(test.spec, test.want, exempt)
			if len(got) != len(test.problems) {
				t.Fatalf("problems = %q, want %d matching %q", got, len(test.problems), test.problems)
			}
			for i, substring := range test.problems {
				if !strings.Contains(got[i], substring) {
					t.Errorf("problem %d = %q, want it to contain %q", i, got[i], substring)
				}
			}
		})
	}
}

func enumStrings[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = string(value)
	}
	return out
}
