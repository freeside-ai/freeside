package domain

import (
	"strings"
	"testing"
)

// enumValidity is the internal-package half of the enum tests: it needs the
// unexported valid() predicate. Every registered member is valid, and the zero
// value (empty string) is always invalid.
func TestEnumValidity(t *testing.T) {
	valids := map[string][]func() bool{}
	invalids := map[string]func() bool{}

	for _, v := range AllAttentionTypes {
		valids["AttentionType"] = append(valids["AttentionType"], v.valid)
	}
	invalids["AttentionType"] = AttentionType("").valid
	for _, v := range AllSubjectTypes {
		valids["SubjectType"] = append(valids["SubjectType"], v.valid)
	}
	invalids["SubjectType"] = SubjectType("").valid
	for _, v := range AllProducerClasses {
		valids["ProducerClass"] = append(valids["ProducerClass"], v.valid)
	}
	invalids["ProducerClass"] = ProducerClass("").valid
	for _, v := range AllAgentVendors {
		valids["AgentVendor"] = append(valids["AgentVendor"], v.valid)
	}
	invalids["AgentVendor"] = AgentVendor("").valid
	for _, v := range AllVendorInstructionDeliveries {
		valids["VendorInstructionDelivery"] = append(
			valids["VendorInstructionDelivery"], v.valid,
		)
	}
	invalids["VendorInstructionDelivery"] = VendorInstructionDelivery("").valid
	for _, v := range AllDeliveryStatuses {
		valids["DeliveryStatus"] = append(valids["DeliveryStatus"], v.valid)
	}
	invalids["DeliveryStatus"] = DeliveryStatus("").valid
	for _, v := range AllDeviceStatuses {
		valids["DeviceStatus"] = append(valids["DeviceStatus"], v.valid)
	}
	invalids["DeviceStatus"] = DeviceStatus("").valid
	for _, v := range AllDeviceCredentialKinds {
		valids["DeviceCredentialKind"] = append(valids["DeviceCredentialKind"], v.valid)
	}
	invalids["DeviceCredentialKind"] = DeviceCredentialKind("").valid
	for _, v := range AllInterruptionClasses {
		valids["InterruptionClass"] = append(valids["InterruptionClass"], v.valid)
	}
	invalids["InterruptionClass"] = InterruptionClass("").valid
	for _, v := range AllActions {
		valids["Action"] = append(valids["Action"], v.valid)
	}
	invalids["Action"] = Action("").valid
	for _, v := range AllPriorities {
		valids["Priority"] = append(valids["Priority"], v.valid)
	}
	invalids["Priority"] = Priority("").valid
	for _, v := range AllItemStatuses {
		valids["ItemStatus"] = append(valids["ItemStatus"], v.valid)
	}
	invalids["ItemStatus"] = ItemStatus("").valid
	for _, v := range AllSensitivityClasses {
		valids["SensitivityClass"] = append(valids["SensitivityClass"], v.valid)
	}
	invalids["SensitivityClass"] = SensitivityClass("").valid
	for _, v := range AllHeadBindings {
		valids["HeadBinding"] = append(valids["HeadBinding"], v.valid)
	}
	invalids["HeadBinding"] = HeadBinding("").valid
	for _, v := range AllAuthors {
		valids["Author"] = append(valids["Author"], v.valid)
	}
	invalids["Author"] = Author("").valid
	for _, v := range AllProvenanceSources {
		valids["ProvenanceSource"] = append(valids["ProvenanceSource"], v.valid)
	}
	invalids["ProvenanceSource"] = ProvenanceSource("").valid
	for _, v := range AllPRExecutionModes {
		valids["PRExecutionMode"] = append(valids["PRExecutionMode"], v.valid)
	}
	invalids["PRExecutionMode"] = PRExecutionMode("").valid
	for _, v := range AllAutomationChangePolicies {
		valids["AutomationChangePolicy"] = append(valids["AutomationChangePolicy"], v.valid)
	}
	invalids["AutomationChangePolicy"] = AutomationChangePolicy("").valid
	for _, v := range AllTokenPermissionsModes {
		valids["TokenPermissionsMode"] = append(valids["TokenPermissionsMode"], v.valid)
	}
	invalids["TokenPermissionsMode"] = TokenPermissionsMode("").valid
	for _, v := range AllReviewModes {
		valids["ReviewMode"] = append(valids["ReviewMode"], v.valid)
	}
	invalids["ReviewMode"] = ReviewMode("").valid
	for _, v := range AllCandidateFindingClasses {
		valids["CandidateFindingClass"] = append(valids["CandidateFindingClass"], v.valid)
	}
	invalids["CandidateFindingClass"] = CandidateFindingClass("").valid
	for _, v := range AllControlPlaneCategories {
		valids["ControlPlaneCategory"] = append(valids["ControlPlaneCategory"], v.valid)
	}
	invalids["ControlPlaneCategory"] = ControlPlaneCategory("").valid
	for _, v := range AllFindingDispositions {
		valids["FindingDisposition"] = append(valids["FindingDisposition"], v.valid)
	}
	invalids["FindingDisposition"] = FindingDisposition("").valid
	for _, v := range AllCandidateFindingOrigins {
		valids["CandidateFindingOrigin"] = append(valids["CandidateFindingOrigin"], v.valid)
	}
	invalids["CandidateFindingOrigin"] = CandidateFindingOrigin("").valid
	for _, v := range AllVerificationOutcomes {
		valids["VerificationOutcome"] = append(valids["VerificationOutcome"], v.valid)
	}
	invalids["VerificationOutcome"] = VerificationOutcome("").valid
	for _, v := range AllRunnerCapabilities {
		valids["RunnerCapability"] = append(valids["RunnerCapability"], v.valid)
	}
	invalids["RunnerCapability"] = RunnerCapability("").valid
	for _, v := range AllRunnerBackendClasses {
		valids["RunnerBackendClass"] = append(valids["RunnerBackendClass"], v.valid)
	}
	invalids["RunnerBackendClass"] = RunnerBackendClass("").valid
	for _, v := range AllConformanceOutcomes {
		valids["ConformanceOutcome"] = append(valids["ConformanceOutcome"], v.valid)
	}
	invalids["ConformanceOutcome"] = ConformanceOutcome("").valid
	for _, v := range AllOperatingModes {
		valids["OperatingMode"] = append(valids["OperatingMode"], v.valid)
	}
	invalids["OperatingMode"] = OperatingMode("").valid
	for _, v := range AllCredentialModes {
		valids["CredentialMode"] = append(valids["CredentialMode"], v.valid)
	}
	invalids["CredentialMode"] = CredentialMode("").valid
	for _, v := range AllEgressProfiles {
		valids["EgressProfile"] = append(valids["EgressProfile"], v.valid)
	}
	invalids["EgressProfile"] = EgressProfile("").valid
	for _, v := range AllRefreshStrategies {
		valids["RefreshStrategy"] = append(valids["RefreshStrategy"], v.valid)
	}
	invalids["RefreshStrategy"] = RefreshStrategy("").valid
	for _, v := range AllBackupHealthStatuses {
		valids["BackupHealthStatus"] = append(valids["BackupHealthStatus"], v.valid)
	}
	invalids["BackupHealthStatus"] = BackupHealthStatus("").valid
	for _, v := range AllUnattendedOperationStates {
		valids["UnattendedOperationState"] = append(valids["UnattendedOperationState"], v.valid)
	}
	invalids["UnattendedOperationState"] = UnattendedOperationState("").valid
	for _, v := range AllSupersessionKinds {
		valids["SupersessionKind"] = append(valids["SupersessionKind"], v.valid)
	}
	invalids["SupersessionKind"] = SupersessionKind("").valid
	for _, v := range AllRunMilestoneKinds {
		valids["RunMilestoneKind"] = append(valids["RunMilestoneKind"], v.valid)
	}
	invalids["RunMilestoneKind"] = RunMilestoneKind("").valid
	for _, v := range AllObservedInvocationStatuses {
		valids["ObservedInvocationStatus"] = append(valids["ObservedInvocationStatus"], v.valid)
	}
	invalids["ObservedInvocationStatus"] = ObservedInvocationStatus("").valid
	for _, v := range AllRunHoldReasons {
		valids["RunHoldReason"] = append(valids["RunHoldReason"], v.valid)
	}
	invalids["RunHoldReason"] = RunHoldReason("").valid
	for _, v := range AllInvocationLivenesses {
		valids["InvocationLiveness"] = append(valids["InvocationLiveness"], v.valid)
	}
	invalids["InvocationLiveness"] = InvocationLiveness("").valid
	for _, v := range AllScheduleKinds {
		valids["ScheduleKind"] = append(valids["ScheduleKind"], v.valid)
	}
	invalids["ScheduleKind"] = ScheduleKind("").valid
	for _, v := range AllScheduleStatuses {
		valids["ScheduleStatus"] = append(valids["ScheduleStatus"], v.valid)
	}
	invalids["ScheduleStatus"] = ScheduleStatus("").valid
	for _, v := range AllScheduleResolutionReasons {
		valids["ScheduleResolutionReason"] = append(valids["ScheduleResolutionReason"], v.valid)
	}
	invalids["ScheduleResolutionReason"] = ScheduleResolutionReason("").valid
	for _, v := range AllScheduleSubjectTypes {
		valids["ScheduleSubjectType"] = append(valids["ScheduleSubjectType"], v.valid)
	}
	invalids["ScheduleSubjectType"] = ScheduleSubjectType("").valid
	for _, v := range AllScheduleOccurrenceStatuses {
		valids["ScheduleOccurrenceStatus"] = append(valids["ScheduleOccurrenceStatus"], v.valid)
	}
	invalids["ScheduleOccurrenceStatus"] = ScheduleOccurrenceStatus("").valid
	for _, v := range AllScheduleOccurrenceOutcomes {
		valids["ScheduleOccurrenceOutcome"] = append(valids["ScheduleOccurrenceOutcome"], v.valid)
	}
	invalids["ScheduleOccurrenceOutcome"] = ScheduleOccurrenceOutcome("").valid
	for _, v := range AllCompletionCriterionKinds {
		valids["CompletionCriterionKind"] = append(valids["CompletionCriterionKind"], v.valid)
	}
	invalids["CompletionCriterionKind"] = CompletionCriterionKind("").valid
	for _, v := range AllPullRequestStates {
		valids["PullRequestState"] = append(valids["PullRequestState"], v.valid)
	}
	invalids["PullRequestState"] = PullRequestState("").valid
	for _, v := range AllIssueStates {
		valids["IssueState"] = append(valids["IssueState"], v.valid)
	}
	invalids["IssueState"] = IssueState("").valid
	for _, v := range AllReadinessInvalidationReasons {
		valids["ReadinessInvalidationReason"] = append(valids["ReadinessInvalidationReason"], v.valid)
	}
	invalids["ReadinessInvalidationReason"] = ReadinessInvalidationReason("").valid

	for name, checks := range valids {
		for i, check := range checks {
			if !check() {
				t.Errorf("%s member %d: valid() = false, want true", name, i)
			}
		}
	}
	for name, check := range invalids {
		if check() {
			t.Errorf("%s zero value: valid() = true, want false", name)
		}
	}
}

// TestDeliveryStatusVocabulary is acceptance criterion 3: the delivery-status
// vocabulary never calls a channel provider's acceptance "delivered". No member
// is or contains that word, and the vocabulary is exactly the three honest
// statuses.
func TestDeliveryStatusVocabulary(t *testing.T) {
	want := map[DeliveryStatus]bool{
		DeliverySubmitted:       true,
		DeliveryChannelAccepted: true,
		DeliveryOpened:          true,
	}
	if len(AllDeliveryStatuses) != len(want) {
		t.Fatalf("AllDeliveryStatuses = %v, want exactly %d honest statuses", AllDeliveryStatuses, len(want))
	}
	for _, s := range AllDeliveryStatuses {
		if !want[s] {
			t.Errorf("unexpected delivery status %q", s)
		}
		if strings.Contains(strings.ToLower(string(s)), "deliver") {
			t.Errorf("delivery status %q implies delivery; channel acceptance is never called delivered", s)
		}
	}
	// No status maps acceptance to "delivered": the accepted status is a
	// distinct, weaker word.
	if DeliveryChannelAccepted == "delivered" {
		t.Error("channel acceptance must not be represented as delivered")
	}
}
