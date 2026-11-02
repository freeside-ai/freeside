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

// TestSubmissionRecordsHoldTheirLinesInRecordedForm: both records accept no
// lines or a canonical set, and refuse what no writer stores. A body or row
// out of form fails at the store's decode, because this is its backstop.
func TestSubmissionRecordsHoldTheirLinesInRecordedForm(t *testing.T) {
	t.Parallel()
	implementer := domain.TaskLineChoice{Role: domain.RoleImplementer, Agent: "claude-b"}
	reviewer := domain.TaskLineChoice{Role: domain.RoleReviewer, Agent: "codex"}
	digest := domain.Digest("sha256:" + strings.Repeat("e1", 32))
	cases := map[string]struct {
		lines []domain.TaskLineChoice
		want  error
	}{
		"none":              {nil, nil},
		"one":               {[]domain.TaskLineChoice{reviewer}, nil},
		"canonical":         {[]domain.TaskLineChoice{implementer, reviewer}, nil},
		"empty":             {[]domain.TaskLineChoice{}, domain.ErrTaskLinesNotCanonical},
		"out of role order": {[]domain.TaskLineChoice{reviewer, implementer}, domain.ErrTaskLinesNotCanonical},
		"duplicate role":    {[]domain.TaskLineChoice{implementer, implementer}, domain.ErrDuplicateTaskLineRole},
		"ineligible role":   {[]domain.TaskLineChoice{{Role: domain.RoleShadowReviewer, Agent: "codex"}}, domain.ErrTaskLineRoleIneligible},
		"malformed agent":   {[]domain.TaskLineChoice{{Role: domain.RoleImplementer, Agent: "Not An Agent"}}, domain.ErrInvalidAgentName},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			submission := validDeviceSubmission()
			submission.TaskLines = tc.lines
			if err := submission.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("TaskSubmission.Validate = %v, want %v", err, tc.want)
			}
			manual := domain.ManualSubmission{
				Identity: "cli:lines", ProjectID: "proj-1", SourceArtifactID: "artifact-1",
				SourceDigest: digest, RequestDigest: digest, ImplementationRunID: "run-1", TaskLines: tc.lines,
			}
			if err := manual.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("ManualSubmission.Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestManualSubmissionSameRequest: the request digest stands for the lines, so
// a record from before they were stored is still the request that carried
// them. Every other field still tells two requests apart.
func TestManualSubmissionSameRequest(t *testing.T) {
	t.Parallel()
	digest := domain.Digest("sha256:" + strings.Repeat("e1", 32))
	recorded := domain.ManualSubmission{
		Identity: "cli:lines", ProjectID: "proj-1", SourceArtifactID: "artifact-1",
		SourceDigest: digest, RequestDigest: digest, ImplementationRunID: "run-1",
	}
	retry := recorded
	retry.TaskLines = []domain.TaskLineChoice{{Role: domain.RoleReviewer, Agent: "codex"}}
	if !recorded.SameRequest(retry) {
		t.Fatal("a retry of a record from before the field is not the same request")
	}
	for name, mutate := range map[string]func(*domain.ManualSubmission){
		"identity":        func(s *domain.ManualSubmission) { s.Identity = "cli:other" },
		"project":         func(s *domain.ManualSubmission) { s.ProjectID = "proj-2" },
		"source artifact": func(s *domain.ManualSubmission) { s.SourceArtifactID = "artifact-2" },
		"source digest":   func(s *domain.ManualSubmission) { s.SourceDigest = "sha256:" + domain.Digest(strings.Repeat("f1", 32)) },
		"request digest": func(s *domain.ManualSubmission) {
			s.RequestDigest = "sha256:" + domain.Digest(strings.Repeat("f1", 32))
		},
		"implementation run": func(s *domain.ManualSubmission) { s.ImplementationRunID = "run-2" },
	} {
		other := recorded
		mutate(&other)
		if recorded.SameRequest(other) {
			t.Errorf("records that differ in %s are the same request", name)
		}
	}
}
