package exec_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func execStageDigest(fill string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(fill, 64))
}

// agentBinding is a well-formed attended binding; the caller sets the model
// and effort fields it exercises.
func agentBinding() *domain.AdmissionAgentBinding {
	return &domain.AdmissionAgentBinding{
		AgentDigest: execStageDigest("a"), LaunchDigest: execStageDigest("b"),
		TreatmentDigest: execStageDigest("d"), PricingRevision: "pricing-2026-01",
		LineupRevision: execStageDigest("c"), EnrollmentID: "enroll-1",
		EnrollmentGeneration: 1, StoreManifestDigest: execStageDigest("9"),
		EffectiveEgress: []string{"api.anthropic.com"},
	}
}

func explicitAgentBinding() *domain.AdmissionAgentBinding {
	b := agentBinding()
	b.RouteModelID = "claude-opus-5-5"
	b.RequestedEffort = domain.EffortMax
	b.NativeEffort = "max"
	return b
}

func fullAdmission(
	t *testing.T, identity *domain.AuthIdentityID, egress domain.EgressProfile,
	binding *domain.AdmissionAgentBinding,
) domain.ExecutionAdmission {
	t.Helper()
	conversationDigest := execStageDigest("7")
	vendorDigest := execStageDigest("8")
	stageInputs, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:         execStageDigest("1"),
		SpecificationDigest: execStageDigest("2"),
		PromptPackageDigest: execStageDigest("3"),
		PolicyDigest:        execStageDigest("4"),
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor:   domain.AgentVendorClaude,
			Delivery: domain.VendorInstructionDeliveryAppendFile,
			Digest:   &vendorDigest,
		},
		ConversationDigest:   &conversationDigest,
		PriorArtifactDigests: []domain.Digest{execStageDigest("5")},
		ImageInputDigests:    []domain.Digest{execStageDigest("6")},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: "inv-1", RunID: "run-1", StageID: "stage-1", AttemptID: "attempt-1",
		Backend:        "fresh_vm_read_only_volume_handoff",
		Capabilities:   domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  egress,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     execStageDigest("2"), PolicyDigest: execStageDigest("4"), InputDigest: execStageDigest("1"),
		StageInputs:    &stageInputs,
		Base:           domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:      "ws-1",
		AuthIdentityID: identity,
		AgentBinding:   binding,
		AdmittedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	return a
}

// skippingHydration re-addresses an admission with the hydration skip set, the
// one fact fullAdmission leaves unset.
func skippingHydration(t *testing.T, a domain.ExecutionAdmission) domain.ExecutionAdmission {
	t.Helper()
	a.WorkspaceHydration = domain.WorkspaceHydrationNone
	id, err := a.ComputeID()
	if err != nil {
		t.Fatal(err)
	}
	a.ID = id
	if err := a.Validate(); err != nil {
		t.Fatalf("skipping admission: %v", err)
	}
	return a
}

// TestStartSpecFromAdmissionCarriesEveryField is the drift guard between the
// spec and the record it is rendered from: a field added to StartSpec and left
// unmapped stays zero here and fails, rather than reaching a driver empty.
func TestStartSpecFromAdmissionCarriesEveryField(t *testing.T) {
	identity := domain.AuthIdentityID("auth-1")
	admission := skippingHydration(t, fullAdmission(t, &identity, domain.EgressProviderOnly, explicitAgentBinding()))
	spec := exec.StartSpecFromAdmission(admission)

	v := reflect.ValueOf(spec)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("StartSpec.%s is zero: the admission carries a value for it", v.Type().Field(i).Name)
		}
	}

	if spec.RunID != admission.RunID || spec.StageID != admission.StageID || spec.AttemptID != admission.AttemptID {
		t.Errorf("spec identity %v disagrees with the admission", spec)
	}
	if spec.AdmissionID != admission.ID {
		t.Errorf("spec admission_id = %q, want %q", spec.AdmissionID, admission.ID)
	}
	if spec.Base != admission.Base || spec.ImageRef != admission.ImageRef {
		t.Errorf("spec base/image disagree with the admission")
	}
	if spec.AuthIdentityID != identity {
		t.Errorf("spec auth_identity_id = %q, want %q", spec.AuthIdentityID, identity)
	}
	if spec.RouteModelID != "claude-opus-5-5" || spec.NativeEffort != "max" {
		t.Errorf("spec model/effort = %q/%q, want the binding's claude-opus-5-5/max",
			spec.RouteModelID, spec.NativeEffort)
	}
}

// The stage driver reads the hydration skip from the spec, so the conversion
// must carry it when set and leave it unset otherwise: an unset value keeps
// the composition's hydration and the journaled spec equal to today's.
func TestStartSpecFromAdmissionCarriesWorkspaceHydration(t *testing.T) {
	identity := domain.AuthIdentityID("auth-1")
	unset := fullAdmission(t, &identity, domain.EgressProviderOnly, agentBinding())
	for name, tc := range map[string]struct {
		admission domain.ExecutionAdmission
		want      domain.WorkspaceHydration
	}{
		"unset":   {unset, ""},
		"skipped": {skippingHydration(t, unset), domain.WorkspaceHydrationNone},
	} {
		t.Run(name, func(t *testing.T) {
			if got := exec.StartSpecFromAdmission(tc.admission).WorkspaceHydration; got != tc.want {
				t.Fatalf("spec workspace_hydration = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStartSpecFromAdmissionPassesNoModelOrEffort covers the two admissions
// whose launch passes neither: a binding for the native-default offer at
// harness_default records no model or effort, and a legacy admission has no
// binding at all.
func TestStartSpecFromAdmissionPassesNoModelOrEffort(t *testing.T) {
	identity := domain.AuthIdentityID("auth-1")
	for name, binding := range map[string]*domain.AdmissionAgentBinding{
		"native default": agentBinding(),
		"no binding":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			spec := exec.StartSpecFromAdmission(fullAdmission(t, &identity, domain.EgressProviderOnly, binding))
			if spec.RouteModelID != "" || spec.NativeEffort != "" {
				t.Fatalf("spec model/effort = %q/%q, want both empty", spec.RouteModelID, spec.NativeEffort)
			}
		})
	}
}

// TestStartSpecFromAdmissionWithoutIdentity covers the clean-verification
// branch: no provider identity is recorded, so none is passed to the driver.
func TestStartSpecFromAdmissionWithoutIdentity(t *testing.T) {
	spec := exec.StartSpecFromAdmission(fullAdmission(t, nil, domain.EgressCleanVerification, nil))
	if spec.AuthIdentityID != "" {
		t.Fatalf("spec auth_identity_id = %q, want empty", spec.AuthIdentityID)
	}
	if spec.EgressProfile != domain.EgressCleanVerification {
		t.Fatalf("spec egress_profile = %q, want %q", spec.EgressProfile, domain.EgressCleanVerification)
	}
}

func TestStartSpecFromAdmissionDetachesStageInputs(t *testing.T) {
	identity := domain.AuthIdentityID("auth-1")
	admission := fullAdmission(t, &identity, domain.EgressProviderOnly, nil)
	spec := exec.StartSpecFromAdmission(admission)
	*admission.StageInputs.VendorInstructions.Digest = execStageDigest("e")
	*admission.StageInputs.ConversationDigest = execStageDigest("f")
	admission.StageInputs.PriorArtifactDigests[0] = "sha256:changed"
	admission.StageInputs.ImageInputDigests[0] = "sha256:changed"
	if *spec.StageInputs.VendorInstructions.Digest != execStageDigest("8") ||
		*spec.StageInputs.ConversationDigest != execStageDigest("7") ||
		spec.StageInputs.PriorArtifactDigests[0] != execStageDigest("5") ||
		spec.StageInputs.ImageInputDigests[0] != execStageDigest("6") {
		t.Fatal("start spec followed mutable admission slice storage")
	}
}

func TestStageResultQuotaRefusal(t *testing.T) {
	t.Parallel()
	resetsAt := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		cause   domain.FailureCause
		refusal domain.QuotaRefusal
		want    error
	}{
		{"quota with a reset", domain.FailureCauseQuota, domain.QuotaRefusal{Window: "five_hour", ResetsAt: &resetsAt}, nil},
		{"quota without a reset", domain.FailureCauseQuota, domain.QuotaRefusal{Window: "five_hour"}, nil},
		{
			"beside another cause", domain.FailureCauseServiceCapacity,
			domain.QuotaRefusal{Window: "five_hour"},
			exec.ErrQuotaRefusalWithoutQuota,
		},
		{"beside no cause", "", domain.QuotaRefusal{Window: "five_hour"}, exec.ErrQuotaRefusalWithoutQuota},
		{"empty window", domain.FailureCauseQuota, domain.QuotaRefusal{}, domain.ErrEmptyField},
		{"non-UTC reset", domain.FailureCauseQuota, domain.QuotaRefusal{
			Window: "five_hour", ResetsAt: new(resetsAt.In(time.FixedZone("PST", -8*3600))),
		}, domain.ErrTimestampNotUTC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := exec.StageResult{
				InvocationID: "inv-1", Status: exec.StatusFailed, Cause: tc.cause, QuotaRefusal: &tc.refusal,
			}.Validate()
			if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestStageResultClaudeUsage(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	resets := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	local := resets.In(time.FixedZone("PST", -8*3600))
	window := exec.ClaudeUsageWindow{Window: "seven_day", UsedPercent: 57, ResetsAt: &resets}
	event := exec.ClaudeRateLimitEvent{Window: "seven_day", Status: "allowed", ResetsAt: &resets}
	valid := exec.ClaudeUsageCapture{ObservedAt: at, Windows: []exec.ClaudeUsageWindow{window}}
	for _, tc := range []struct {
		name    string
		capture func(exec.ClaudeUsageCapture) exec.ClaudeUsageCapture
		valid   bool
	}{
		{"one window", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture { return c }, true},
		{"event only", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows, c.Events = nil, []exec.ClaudeRateLimitEvent{event}
			return c
		}, true},
		{"no reset", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows = []exec.ClaudeUsageWindow{{Window: "seven_day", UsedPercent: 100}}
			return c
		}, true},
		{"nothing captured", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows = nil
			return c
		}, false},
		{"zero observed_at", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.ObservedAt = time.Time{}
			return c
		}, false},
		{"local observed_at", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.ObservedAt = local
			return c
		}, false},
		{"share above 100", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows = []exec.ClaudeUsageWindow{{Window: "seven_day", UsedPercent: 101}}
			return c
		}, false},
		{"negative share", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows = []exec.ClaudeUsageWindow{{Window: "seven_day", UsedPercent: -1}}
			return c
		}, false},
		{"uppercase window", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows = []exec.ClaudeUsageWindow{{Window: "Seven_day"}}
			return c
		}, false},
		{"unsorted windows", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows = []exec.ClaudeUsageWindow{{Window: "seven_day"}, {Window: "five_hour"}}
			return c
		}, false},
		{"duplicate window", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows = []exec.ClaudeUsageWindow{{Window: "seven_day"}, {Window: "seven_day"}}
			return c
		}, false},
		{"local window reset", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows = []exec.ClaudeUsageWindow{{Window: "seven_day", ResetsAt: &local}}
			return c
		}, false},
		{"empty event status", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Events = []exec.ClaudeRateLimitEvent{{Window: "seven_day"}}
			return c
		}, false},
		{"event status with a space", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Events = []exec.ClaudeRateLimitEvent{{Window: "seven_day", Status: "allowed warning"}}
			return c
		}, false},
		{"local event reset", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Events = []exec.ClaudeRateLimitEvent{{Window: "seven_day", Status: "allowed", ResetsAt: &local}}
			return c
		}, false},
		// A reset JSON cannot encode would make the result unwritable.
		{"last encodable reset", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows = []exec.ClaudeUsageWindow{{Window: "seven_day", ResetsAt: new(time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))}}
			return c
		}, true},
		{"window reset in year 10000", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows = []exec.ClaudeUsageWindow{{Window: "seven_day", ResetsAt: new(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))}}
			return c
		}, false},
		{"event reset in year -1", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Events = []exec.ClaudeRateLimitEvent{{Window: "seven_day", Status: "allowed", ResetsAt: new(time.Date(-1, 12, 31, 0, 0, 0, 0, time.UTC))}}
			return c
		}, false},
		{"unsorted events", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Events = []exec.ClaudeRateLimitEvent{{Window: "seven_day", Status: "allowed"}, {Window: "five_hour", Status: "allowed"}}
			return c
		}, false},
		{"duplicate event", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Events = []exec.ClaudeRateLimitEvent{event, event}
			return c
		}, false},
		{"sixteen windows and events", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Windows, c.Events = nil, nil
			for i := range 16 {
				name := fmt.Sprintf("w%02d", i)
				c.Windows = append(c.Windows, exec.ClaudeUsageWindow{Window: name})
				c.Events = append(c.Events, exec.ClaudeRateLimitEvent{Window: name, Status: "allowed"})
			}
			return c
		}, true},
		{"seventeen events", func(c exec.ClaudeUsageCapture) exec.ClaudeUsageCapture {
			c.Events = nil
			for i := range 17 {
				c.Events = append(c.Events, exec.ClaudeRateLimitEvent{Window: fmt.Sprintf("w%02d", i), Status: "allowed"})
			}
			return c
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			capture := tc.capture(valid)
			err := exec.StageResult{
				InvocationID: "inv-1", Status: exec.StatusCompleted, ClaudeUsage: &capture,
			}.Validate()
			if tc.valid && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if _, marshalErr := json.Marshal(capture); tc.valid && marshalErr != nil {
				t.Fatalf("a valid capture does not marshal: %v", marshalErr)
			}
			if !tc.valid && !errors.Is(err, exec.ErrInvalidClaudeUsage) {
				t.Fatalf("Validate() = %v, want ErrInvalidClaudeUsage", err)
			}
		})
	}
}

func TestStageResultCause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status exec.Status
		cause  domain.FailureCause
		want   error
	}{
		{"failed with a cause", exec.StatusFailed, domain.FailureCauseQuota, nil},
		// Empty on a failed result means unknown; the stage driver records it.
		{"failed without a cause", exec.StatusFailed, "", nil},
		{"failed with an invalid cause", exec.StatusFailed, "transient", domain.ErrInvalidFailureCause},
		{"completed with a cause", exec.StatusCompleted, domain.FailureCauseUnknown, exec.ErrCauseWithoutFailure},
		{"canceled with a cause", exec.StatusCanceled, domain.FailureCauseQuota, exec.ErrCauseWithoutFailure},
		{"blocked with a cause", exec.StatusBlocked, domain.FailureCauseQuota, exec.ErrCauseWithoutFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := exec.StageResult{InvocationID: "inv-1", Status: tc.status, Cause: tc.cause}.Validate()
			if tc.want == nil && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestStartSpecCloneDetachesAttachments(t *testing.T) {
	identity := domain.AuthIdentityID("auth-1")
	admission := fullAdmission(t, &identity, domain.EgressProviderOnly, agentBinding())
	in := domain.StageInputSnapshotInput{
		InputDigest: admission.StageInputs.InputDigest, SpecificationDigest: admission.StageInputs.SpecificationDigest,
		PromptPackageDigest: admission.StageInputs.PromptPackageDigest, PolicyDigest: admission.StageInputs.PolicyDigest,
		Attachments: []domain.StageInputAttachment{{
			Digest: execStageDigest("e"), Name: "notes/readme.md",
			MediaType: domain.TaskAttachmentMediaOctetStream, SizeBytes: 3,
		}},
	}
	snapshot, err := domain.NewStageInputSnapshot(in)
	if err != nil {
		t.Fatal(err)
	}
	spec := exec.StartSpec{StageInputs: &snapshot}
	cloned := spec.Clone()
	snapshot.Attachments[0].Name = "changed.md"
	if cloned.StageInputs.Attachments[0].Name != "notes/readme.md" {
		t.Fatal("StartSpec.Clone shared the snapshot's attachment slice")
	}
}
