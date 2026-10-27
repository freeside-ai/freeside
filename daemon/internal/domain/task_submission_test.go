package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validDeviceSubmission() domain.TaskSubmission {
	return domain.TaskSubmission{
		CommandID: "cmd-1", Origin: domain.TaskSubmissionOriginDevice,
		DeviceID: new(domain.DeviceID("device-1")), ProjectID: "proj-1",
		SourceDigest: domain.Digest("sha256:" + strings.Repeat("1", 64)),
		TaskID:       "task-1", SpecificationRunID: "run-1",
		Name: domain.DisplayName{Text: "Improve task navigation", Source: domain.DisplayNameSourceOperator},
	}
}

func TestTaskSubmissionOriginValidate(t *testing.T) {
	t.Parallel()
	if err := validDeviceSubmission().Validate(); err != nil {
		t.Fatalf("device submission: %v", err)
	}
	daemon := validDeviceSubmission()
	daemon.Origin, daemon.DeviceID = domain.TaskSubmissionOriginDaemon, nil
	if err := daemon.Validate(); err != nil {
		t.Fatalf("daemon submission: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*domain.TaskSubmission)
		want   error
	}{
		"no origin": {func(s *domain.TaskSubmission) { s.Origin = "" }, domain.ErrInvalidTaskSubmissionOrigin},
		"device without device": {
			func(s *domain.TaskSubmission) { s.DeviceID = nil },
			domain.ErrTaskSubmissionOriginMismatch,
		},
		"daemon with device": {
			func(s *domain.TaskSubmission) { s.Origin = domain.TaskSubmissionOriginDaemon },
			domain.ErrTaskSubmissionOriginMismatch,
		},
		"device in the daemon namespace": {
			func(s *domain.TaskSubmission) {
				s.CommandID = domain.ImageUpkeepSubmissionIdentity(s.SourceDigest)
			},
			domain.ErrTaskSubmissionOriginMismatch,
		},
		"empty device": {
			func(s *domain.TaskSubmission) { s.DeviceID = new(domain.DeviceID("")) },
			domain.ErrEmptyID,
		},
	} {
		submission := validDeviceSubmission()
		tc.mutate(&submission)
		if err := submission.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", name, err, tc.want)
		}
	}
}

// A body recorded before origins existed carries no origin; it decodes as
// the device submission it was.
func TestTaskSubmissionCanonicalizesPreOriginBody(t *testing.T) {
	t.Parallel()
	body := `{"command_id":"cmd-1","device_id":"device-1","project_id":"proj-1",` +
		`"source_digest":"sha256:` + strings.Repeat("1", 64) + `","task_id":"task-1",` +
		`"specification_run_id":"run-1","name":{"text":"Improve task navigation","source":"operator"}}`
	var submission domain.TaskSubmission
	if err := json.Unmarshal([]byte(body), &submission); err != nil {
		t.Fatal(err)
	}
	submission.CanonicalizeStoredRow()
	if submission.Origin != domain.TaskSubmissionOriginDevice || submission.Validate() != nil {
		t.Fatalf("pre-origin body = %+v, want a valid device submission", submission)
	}
}

func TestImageUpkeepSubmissionIdentityIsAManualIdentity(t *testing.T) {
	t.Parallel()
	refusalID := domain.Digest("sha256:" + strings.Repeat("e1", 32))
	identity := domain.ImageUpkeepSubmissionIdentity(refusalID)
	if identity != "daemon:image-upkeep:"+string(refusalID) {
		t.Fatalf("identity = %q", identity)
	}
	submission := domain.ManualSubmission{
		Identity: identity, ProjectID: "proj-1", SourceArtifactID: "artifact-1",
		SourceDigest: refusalID, RequestDigest: refusalID, ImplementationRunID: "run-1",
	}
	if err := submission.Validate(); err != nil {
		t.Fatalf("daemon identity: %v", err)
	}
	submission.Identity = "daemon:"
	if err := submission.Validate(); err == nil {
		t.Fatal("bare daemon: prefix: Validate = nil")
	}
	submission.Identity = "other:image-upkeep:x"
	if err := submission.Validate(); err == nil {
		t.Fatal("unknown namespace: Validate = nil")
	}
}
