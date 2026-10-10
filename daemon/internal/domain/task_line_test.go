package domain_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestTaskLineRoles holds the eligibility predicate and the listed roles
// together: the four ward roles a task line may choose, and never the shadow
// reviewer or a wardless role.
func TestTaskLineRoles(t *testing.T) {
	t.Parallel()
	want := []domain.RoleName{
		domain.RoleSpecifier, domain.RoleImplementer, domain.RoleRemediator, domain.RoleReviewer,
	}
	if !slices.Equal(domain.TaskLineRoles, want) {
		t.Fatalf("task-line roles = %v, want %v", domain.TaskLineRoles, want)
	}
	for _, role := range domain.AllRoleNames {
		if got := role.TaskLineEligible(); got != slices.Contains(want, role) {
			t.Fatalf("%s eligible = %t", role, got)
		}
	}
	if domain.RoleName("").TaskLineEligible() || domain.RoleName("researcher").TaskLineEligible() {
		t.Fatal("an unknown role is task-line eligible")
	}
}

func TestValidateTaskLineChoices(t *testing.T) {
	t.Parallel()
	valid := []domain.TaskLineChoice{
		{Role: domain.RoleImplementer, Agent: "codex"},
		{Role: domain.RoleReviewer, Agent: "claude-b"},
	}
	if err := domain.ValidateTaskLineChoices(valid); err != nil {
		t.Fatalf("valid choices: %v", err)
	}
	if err := domain.ValidateTaskLineChoices(nil); err != nil {
		t.Fatalf("no choices: %v", err)
	}
	for name, tc := range map[string]struct {
		choices []domain.TaskLineChoice
		want    error
	}{
		"shadow reviewer": {[]domain.TaskLineChoice{{Role: domain.RoleShadowReviewer, Agent: "codex"}}, domain.ErrTaskLineRoleIneligible},
		"wardless role":   {[]domain.TaskLineChoice{{Role: domain.RoleTaskNamer, Agent: "codex"}}, domain.ErrTaskLineRoleIneligible},
		"unknown role":    {[]domain.TaskLineChoice{{Role: "researcher", Agent: "codex"}}, domain.ErrTaskLineRoleIneligible},
		"duplicate role": {[]domain.TaskLineChoice{
			{Role: domain.RoleReviewer, Agent: "codex"}, {Role: domain.RoleReviewer, Agent: "claude"},
		}, domain.ErrDuplicateTaskLineRole},
		"empty agent":     {[]domain.TaskLineChoice{{Role: domain.RoleReviewer}}, domain.ErrInvalidAgentName},
		"uppercase agent": {[]domain.TaskLineChoice{{Role: domain.RoleReviewer, Agent: "Codex"}}, domain.ErrInvalidAgentName},
		"path agent":      {[]domain.TaskLineChoice{{Role: domain.RoleReviewer, Agent: "../codex"}}, domain.ErrInvalidAgentName},
	} {
		if err := domain.ValidateTaskLineChoices(tc.choices); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestTaskLineValidate(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	input := domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleImplementer, Agent: "codex",
		Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
	}
	first, err := domain.NewTaskLine(input, 1, nil, at)
	if err != nil {
		t.Fatalf("version 1: %v", err)
	}
	changed := input
	changed.Agent = "claude-b"
	second, err := domain.NewTaskLine(changed, 2, &first.ID, at.Add(time.Minute))
	if err != nil {
		t.Fatalf("version 2: %v", err)
	}
	if second.PredecessorID == nil || *second.PredecessorID != first.ID || second.ID == first.ID {
		t.Fatalf("version 2 = %+v, want a new id that names %s", second, first.ID)
	}
	// A non-UTC instant is normalized, so one instant has one identity.
	zoned, err := domain.NewTaskLine(input, 1, nil, at.In(time.FixedZone("x", 3600)))
	if err != nil || zoned.ID != first.ID {
		t.Fatalf("zoned line id = %s (%v), want %s", zoned.ID, err, first.ID)
	}

	bogus := domain.Digest("sha256:" + strings.Repeat("0", 64))
	for name, tc := range map[string]struct {
		mutate func(*domain.TaskLine)
		want   error
	}{
		"empty task":             {func(l *domain.TaskLine) { l.TaskID = "" }, domain.ErrEmptyID},
		"ineligible role":        {func(l *domain.TaskLine) { l.Role = domain.RoleShadowReviewer }, domain.ErrTaskLineRoleIneligible},
		"bad agent":              {func(l *domain.TaskLine) { l.Agent = "Bad Agent" }, domain.ErrInvalidAgentName},
		"zero version":           {func(l *domain.TaskLine) { l.Version = 0 }, domain.ErrNonPositive},
		"first with predecessor": {func(l *domain.TaskLine) { l.PredecessorID = &bogus }, domain.ErrTaskLineInconsistent},
		"later without one":      {func(l *domain.TaskLine) { l.Version = 2 }, domain.ErrTaskLineInconsistent},
		// Intake is not an operator path, so it is not a source at all.
		"intake source":  {func(l *domain.TaskLine) { l.Source = "intake" }, domain.ErrInvalidTaskLineSource},
		"empty source":   {func(l *domain.TaskLine) { l.Source = "" }, domain.ErrInvalidTaskLineSource},
		"empty set_by":   {func(l *domain.TaskLine) { l.SetBy = "" }, domain.ErrEmptyField},
		"zero set_at":    {func(l *domain.TaskLine) { l.SetAt = time.Time{} }, domain.ErrMissingTimestamp},
		"non-UTC set_at": {func(l *domain.TaskLine) { l.SetAt = l.SetAt.In(time.FixedZone("x", 3600)) }, domain.ErrTimestampNotUTC},
		"empty id":       {func(l *domain.TaskLine) { l.ID = "" }, domain.ErrEmptyID},
		// An edited row keeps its old id, which no longer addresses it.
		"edited agent": {func(l *domain.TaskLine) { l.Agent = "claude-b" }, domain.ErrTaskLineInconsistent},
	} {
		line := first
		tc.mutate(&line)
		if err := line.Validate(); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	bad := domain.Digest("not-a-digest")
	if _, err := domain.NewTaskLine(changed, 2, &bad, at); !errors.Is(err, domain.ErrInvalidDigest) {
		t.Fatalf("malformed predecessor: err = %v, want %v", err, domain.ErrInvalidDigest)
	}
}

// TestValidateTaskLineChoicesErrorOmitsRefusedText pins that a refused role
// or agent never appears in the error: both are request text that a response
// or a log line would otherwise repeat.
func TestValidateTaskLineChoicesErrorOmitsRefusedText(t *testing.T) {
	t.Parallel()
	const pasted = "sk-not-a-role-or-agent"
	for name, choices := range map[string][]domain.TaskLineChoice{
		"refused role":  {{Role: pasted, Agent: "codex"}},
		"refused agent": {{Role: domain.RoleReviewer, Agent: pasted + "/"}},
	} {
		err := domain.ValidateTaskLineChoices(choices)
		if err == nil || strings.Contains(err.Error(), pasted) {
			t.Fatalf("%s: err = %v, want an error without the refused text", name, err)
		}
	}
}
