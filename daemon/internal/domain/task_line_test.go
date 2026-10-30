package domain_test

import (
	"errors"
	"reflect"
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

func TestCanonicalTaskLineChoices(t *testing.T) {
	t.Parallel()
	reviewer := domain.TaskLineChoice{Role: domain.RoleReviewer, Agent: "codex"}
	specifier := domain.TaskLineChoice{Role: domain.RoleSpecifier, Agent: "claude-b"}
	remediator := domain.TaskLineChoice{Role: domain.RoleRemediator, Agent: "codex"}
	got, err := domain.CanonicalTaskLineChoices([]domain.TaskLineChoice{reviewer, specifier, remediator})
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if want := []domain.TaskLineChoice{specifier, remediator, reviewer}; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical = %+v, want %+v", got, want)
	}
	for name, none := range map[string][]domain.TaskLineChoice{"nil": nil, "empty": {}} {
		if got, err := domain.CanonicalTaskLineChoices(none); err != nil || got != nil {
			t.Fatalf("%s: canonical = %+v, %v; want nil, nil", name, got, err)
		}
	}
	if _, err := domain.CanonicalTaskLineChoices([]domain.TaskLineChoice{reviewer, reviewer}); !errors.Is(err, domain.ErrDuplicateTaskLineRole) {
		t.Fatalf("duplicate role: err = %v, want ErrDuplicateTaskLineRole", err)
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

// TestTaskLineProvenanceValidate pins that provenance rides exactly on a
// router line, and that each trigger carries exactly the citations it acted
// on: a reading and a passed-over agent for an exhausted mark, neither for a
// plain first admission.
func TestTaskLineProvenanceValidate(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	passedOver := "claude-a"
	input := domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleImplementer, Agent: "claude-b",
		Source: domain.TaskLineSourceRouter, SetBy: "router:task-1",
		Provenance: &domain.TaskLineProvenance{
			PolicyRevision: 2, Trigger: domain.TaskLineSelectionExhaustedMark,
			Reading: &domain.TaskLineSelectionReading{
				Pool: "claude-a", Window: "seven_day_opus", ObservedAt: at.Add(-time.Hour).In(time.FixedZone("x", 3600)),
			},
			PassedOver: &passedOver,
		},
	}
	routed, err := domain.NewTaskLine(input, 1, nil, at)
	if err != nil {
		t.Fatalf("router line: %v", err)
	}
	// The reading's instant is normalized, and the input stays the caller's.
	if routed.Provenance.Reading.ObservedAt.Location() != time.UTC || routed.Provenance == input.Provenance {
		t.Fatalf("router line provenance = %+v, want a detached UTC copy", routed.Provenance)
	}
	// Provenance is bound into the id.
	plain := input
	plain.Provenance = &domain.TaskLineProvenance{PolicyRevision: 2, Trigger: domain.TaskLineSelectionFirstAdmission}
	first, err := domain.NewTaskLine(plain, 1, nil, at)
	if err != nil {
		t.Fatalf("first-admission line: %v", err)
	}
	if first.ID == routed.ID {
		t.Fatalf("provenance does not change the id %s", first.ID)
	}

	other := "codex"
	for name, tc := range map[string]struct {
		mutate func(*domain.TaskLine)
		want   error
	}{
		"router without provenance": {func(l *domain.TaskLine) { l.Provenance = nil }, domain.ErrTaskLineProvenanceInconsistent},
		"operator with provenance":  {func(l *domain.TaskLine) { l.Source = domain.TaskLineSourceSubmitTask }, domain.ErrTaskLineProvenanceInconsistent},
		"zero revision":             {func(l *domain.TaskLine) { l.Provenance.PolicyRevision = 0 }, domain.ErrNonPositive},
		"unknown trigger":           {func(l *domain.TaskLine) { l.Provenance.Trigger = "quota_refusal" }, domain.ErrInvalidTaskLineSelectionTrigger},
		"mark without reading":      {func(l *domain.TaskLine) { l.Provenance.Reading = nil }, domain.ErrTaskLineProvenanceInconsistent},
		"mark without passed over":  {func(l *domain.TaskLine) { l.Provenance.PassedOver = nil }, domain.ErrTaskLineProvenanceInconsistent},
		"first admission with mark": {func(l *domain.TaskLine) { l.Provenance.Trigger = domain.TaskLineSelectionFirstAdmission }, domain.ErrTaskLineProvenanceInconsistent},
		"empty pool":                {func(l *domain.TaskLine) { l.Provenance.Reading.Pool = "" }, domain.ErrEmptyField},
		"empty window":              {func(l *domain.TaskLine) { l.Provenance.Reading.Window = "" }, domain.ErrEmptyField},
		"zero observed_at":          {func(l *domain.TaskLine) { l.Provenance.Reading.ObservedAt = time.Time{} }, domain.ErrMissingTimestamp},
		// JSON would store a replacement rune the id was not computed over.
		"non-UTF-8 pool":   {func(l *domain.TaskLine) { l.Provenance.Reading.Pool = "pool\xff" }, domain.ErrTaskLineReadingNotUTF8},
		"non-UTF-8 window": {func(l *domain.TaskLine) { l.Provenance.Reading.Window = "window\xfe" }, domain.ErrTaskLineReadingNotUTF8},
		"non-UTC observed_at": {func(l *domain.TaskLine) {
			l.Provenance.Reading.ObservedAt = l.Provenance.Reading.ObservedAt.In(time.FixedZone("x", 3600))
		}, domain.ErrTimestampNotUTC},
		"bad passed over":        {func(l *domain.TaskLine) { bad := "Bad Agent"; l.Provenance.PassedOver = &bad }, domain.ErrInvalidAgentName},
		"passed over the chosen": {func(l *domain.TaskLine) { l.Provenance.PassedOver = &l.Agent }, domain.ErrTaskLineProvenanceInconsistent},
		// An edited provenance keeps the old id, which no longer addresses it.
		"edited passed over": {func(l *domain.TaskLine) { l.Provenance.PassedOver = &other }, domain.ErrTaskLineInconsistent},
	} {
		line := routed
		line.Provenance = cloneProvenance(routed.Provenance)
		tc.mutate(&line)
		if err := line.Validate(); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	firstWithReading := first
	firstWithReading.Provenance = &domain.TaskLineProvenance{
		PolicyRevision: 2, Trigger: domain.TaskLineSelectionFirstAdmission, PassedOver: &passedOver,
	}
	if err := firstWithReading.Validate(); !errors.Is(err, domain.ErrTaskLineProvenanceInconsistent) {
		t.Fatalf("first admission with passed over: err = %v", err)
	}
}

func cloneProvenance(p *domain.TaskLineProvenance) *domain.TaskLineProvenance {
	out := *p
	if p.Reading != nil {
		reading := *p.Reading
		out.Reading = &reading
	}
	if p.PassedOver != nil {
		passed := *p.PassedOver
		out.PassedOver = &passed
	}
	return &out
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
