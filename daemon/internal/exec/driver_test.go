package exec_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func fullAdmission(t *testing.T, identity *domain.AuthIdentityID, egress domain.EgressProfile) domain.ExecutionAdmission {
	t.Helper()
	a, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: "inv-1", RunID: "run-1", StageID: "stage-1", AttemptID: "attempt-1",
		Backend:        "fresh_vm_read_only_volume_handoff",
		Capabilities:   domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  egress,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     "sha256:spec", PolicyDigest: "sha256:policy", InputDigest: "sha256:input",
		Base:           domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:      "ws-1",
		AuthIdentityID: identity,
		AdmittedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	return a
}

// TestStartSpecFromAdmissionCarriesEveryField is the drift guard between the
// spec and the record it is rendered from: a field added to StartSpec and left
// unmapped stays zero here and fails, rather than reaching a driver empty.
func TestStartSpecFromAdmissionCarriesEveryField(t *testing.T) {
	identity := domain.AuthIdentityID("auth-1")
	admission := fullAdmission(t, &identity, domain.EgressProviderOnly)
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
}

// TestStartSpecFromAdmissionWithoutIdentity covers the clean-verification
// branch: no provider identity is recorded, so none is passed to the driver.
func TestStartSpecFromAdmissionWithoutIdentity(t *testing.T) {
	spec := exec.StartSpecFromAdmission(fullAdmission(t, nil, domain.EgressCleanVerification))
	if spec.AuthIdentityID != "" {
		t.Fatalf("spec auth_identity_id = %q, want empty", spec.AuthIdentityID)
	}
	if spec.EgressProfile != domain.EgressCleanVerification {
		t.Fatalf("spec egress_profile = %q, want %q", spec.EgressProfile, domain.EgressCleanVerification)
	}
}
