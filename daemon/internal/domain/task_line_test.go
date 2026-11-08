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
// router line and holds one consistent shape: the tier, the readings that
// ranked the candidates, and the mark passed over (plan §5.4, revision 107).
func TestTaskLineProvenanceValidate(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	elsewhere := time.FixedZone("x", 3600)
	// pool-a resets first, but its five-hour window is marked, so the routing
	// passes over claude-a and takes pool-b, the next soonest.
	input := domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleImplementer, Agent: "claude-b",
		Source: domain.TaskLineSourceRouter, SetBy: "router:task-1",
		Provenance: &domain.TaskLineProvenance{
			PolicyRevision: 2, Tier: domain.TaskLineSelectionSoonestReset, Pool: "pool-b",
			RankedBy: []domain.TaskLineRankingReading{
				{Pool: "pool-a", Window: "seven_day", ResetsAt: at.Add(24 * time.Hour).In(elsewhere), ObservedAt: at.Add(-time.Hour)},
				{Pool: "pool-b", Window: "seven_day", ResetsAt: at.Add(48 * time.Hour), ObservedAt: at.Add(-time.Hour)},
			},
			PassedOver: &domain.TaskLinePassedOverMark{
				Agent: "claude-a", Pool: "pool-a", Window: "five_hour", Source: domain.ExhaustedMarkProviderRefusal,
				MarkedAt: at.Add(-30 * time.Minute).In(elsewhere), ResetsAt: new(at.Add(2 * time.Hour).In(elsewhere)),
			},
		},
	}
	routed, err := domain.NewTaskLine(input, 1, nil, at)
	if err != nil {
		t.Fatalf("router line: %v", err)
	}
	// Every instant is normalized, and the input stays the caller's.
	got := routed.Provenance
	if got == input.Provenance || &got.RankedBy[0] == &input.Provenance.RankedBy[0] || got.PassedOver == input.Provenance.PassedOver ||
		got.RankedBy[0].ResetsAt.Location() != time.UTC || got.PassedOver.MarkedAt.Location() != time.UTC ||
		got.PassedOver.ResetsAt.Location() != time.UTC {
		t.Fatalf("router line provenance = %+v, want a detached UTC copy", got)
	}
	// Provenance is bound into the id.
	plain := input
	plain.Provenance = &domain.TaskLineProvenance{PolicyRevision: 2, Tier: domain.TaskLineSelectionPolicyOrder, Pool: "pool-b"}
	first, err := domain.NewTaskLine(plain, 1, nil, at)
	if err != nil {
		t.Fatalf("policy-order line: %v", err)
	}
	if first.ID == routed.ID {
		t.Fatalf("provenance does not change the id %s", first.ID)
	}
	// No ranking is [], one byte form for the id, however the caller said it.
	if first.Provenance.RankedBy == nil {
		t.Fatal("a nil ranking stays nil; want []")
	}

	for name, tc := range map[string]struct {
		mutate func(*domain.TaskLineProvenance)
		want   error
	}{
		"zero revision": {func(p *domain.TaskLineProvenance) { p.PolicyRevision = 0 }, domain.ErrNonPositive},
		"unknown tier":  {func(p *domain.TaskLineProvenance) { p.Tier = "exhausted_mark" }, domain.ErrInvalidTaskLineSelectionTier},
		"empty pool":    {func(p *domain.TaskLineProvenance) { p.Pool = "" }, domain.ErrEmptyField},
		// JSON would store a replacement rune the id was not computed over.
		"non-UTF-8 pool":    {func(p *domain.TaskLineProvenance) { p.Pool = "pool\xff" }, domain.ErrTaskLineReadingNotUTF8},
		"null ranking":      {func(p *domain.TaskLineProvenance) { p.RankedBy = nil }, domain.ErrTaskLineProvenanceInconsistent},
		"empty ranked pool": {func(p *domain.TaskLineProvenance) { p.RankedBy[1].Pool = "" }, domain.ErrEmptyField},
		"empty window":      {func(p *domain.TaskLineProvenance) { p.RankedBy[0].Window = "" }, domain.ErrEmptyField},
		"non-UTF-8 window":  {func(p *domain.TaskLineProvenance) { p.RankedBy[0].Window = "window\xfe" }, domain.ErrTaskLineReadingNotUTF8},
		"zero resets_at":    {func(p *domain.TaskLineProvenance) { p.RankedBy[0].ResetsAt = time.Time{} }, domain.ErrMissingTimestamp},
		"non-UTC observed_at": {func(p *domain.TaskLineProvenance) {
			p.RankedBy[1].ObservedAt = p.RankedBy[1].ObservedAt.In(elsewhere)
		}, domain.ErrTimestampNotUTC},
		"repeated pool": {func(p *domain.TaskLineProvenance) { p.RankedBy[0].Pool = "pool-b" }, domain.ErrTaskLineProvenanceInconsistent},
		"out of reset order": {func(p *domain.TaskLineProvenance) {
			p.RankedBy[0], p.RankedBy[1] = p.RankedBy[1], p.RankedBy[0]
		}, domain.ErrTaskLineProvenanceInconsistent},
		"soonest reset unranked":         {func(p *domain.TaskLineProvenance) { p.Pool = "pool-c" }, domain.ErrTaskLineProvenanceInconsistent},
		"soonest reset no ranking":       {func(p *domain.TaskLineProvenance) { p.RankedBy = []domain.TaskLineRankingReading{} }, domain.ErrTaskLineProvenanceInconsistent},
		"soonest reset skipped unmarked": {func(p *domain.TaskLineProvenance) { p.PassedOver = nil }, domain.ErrTaskLineProvenanceInconsistent},
		"policy order with a known reset": {func(p *domain.TaskLineProvenance) {
			p.Tier = domain.TaskLineSelectionPolicyOrder
		}, domain.ErrTaskLineProvenanceInconsistent},
		"policy order past ranked pools unmarked": {func(p *domain.TaskLineProvenance) {
			p.Tier, p.Pool, p.PassedOver = domain.TaskLineSelectionPolicyOrder, "pool-c", nil
		}, domain.ErrTaskLineProvenanceInconsistent},
		"drain-first passed over": {func(p *domain.TaskLineProvenance) {
			p.Tier = domain.TaskLineSelectionDrainFirstAccount
		}, domain.ErrTaskLineProvenanceInconsistent},
		"bad passed-over agent": {func(p *domain.TaskLineProvenance) { p.PassedOver.Agent = "Bad Agent" }, domain.ErrInvalidAgentName},
		// A mark names an agent (before #2736) or an enrollment, never both
		// and never neither.
		"passed over neither": {func(p *domain.TaskLineProvenance) { p.PassedOver.Agent = "" }, domain.ErrTaskLineProvenanceInconsistent},
		"passed over both": {func(p *domain.TaskLineProvenance) {
			p.PassedOver.Enrollment = "claude-a/claude_code"
		}, domain.ErrTaskLineProvenanceInconsistent},
		"bad passed-over enrollment": {func(p *domain.TaskLineProvenance) {
			p.PassedOver.Agent, p.PassedOver.Enrollment = "", "claude a"
		}, domain.ErrInvalidTaskLineSelection},
		"empty mark pool":     {func(p *domain.TaskLineProvenance) { p.PassedOver.Pool = "" }, domain.ErrEmptyField},
		"empty mark window":   {func(p *domain.TaskLineProvenance) { p.PassedOver.Window = "" }, domain.ErrEmptyField},
		"unknown mark source": {func(p *domain.TaskLineProvenance) { p.PassedOver.Source = "guess" }, domain.ErrInvalidExhaustedMarkSource},
		"zero marked_at":      {func(p *domain.TaskLineProvenance) { p.PassedOver.MarkedAt = time.Time{} }, domain.ErrMissingTimestamp},
		"non-UTC mark reset": {func(p *domain.TaskLineProvenance) {
			p.PassedOver.ResetsAt = new(p.PassedOver.ResetsAt.In(elsewhere))
		}, domain.ErrTimestampNotUTC},
		// An edited provenance keeps the old id, which no longer addresses it.
		"edited passed over": {func(p *domain.TaskLineProvenance) { p.PassedOver.Agent = "codex" }, domain.ErrTaskLineInconsistent},
	} {
		line := routed
		line.Provenance = cloneProvenance(routed.Provenance)
		tc.mutate(line.Provenance)
		if err := line.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	operator := routed
	operator.Source = domain.TaskLineSourceSubmitTask
	if err := operator.Validate(); !errors.Is(err, domain.ErrTaskLineProvenanceInconsistent) {
		t.Errorf("operator with provenance: err = %v", err)
	}
	unrouted := routed
	unrouted.Provenance = nil
	if err := unrouted.Validate(); !errors.Is(err, domain.ErrTaskLineProvenanceInconsistent) {
		t.Errorf("router without provenance: err = %v", err)
	}

	// Shapes each tier may take. The passed-over agent may be the line's own:
	// one routing's provenance rides every role's line, and two agents on one
	// pool may run different models.
	for name, provenance := range map[string]domain.TaskLineProvenance{
		"drain-first, nothing ranked": {PolicyRevision: 1, Tier: domain.TaskLineSelectionDrainFirstAccount, Pool: "pool-c"},
		"drain-first ranked behind": {
			PolicyRevision: 1, Tier: domain.TaskLineSelectionDrainFirstAccount, Pool: "pool-b",
			RankedBy: input.Provenance.RankedBy,
		},
		"soonest reset first": {
			PolicyRevision: 1, Tier: domain.TaskLineSelectionSoonestReset, Pool: "pool-a",
			RankedBy: input.Provenance.RankedBy,
		},
		"policy order past marked pools": {
			PolicyRevision: 1, Tier: domain.TaskLineSelectionPolicyOrder, Pool: "pool-c",
			RankedBy: input.Provenance.RankedBy, PassedOver: input.Provenance.PassedOver,
		},
		"passed over the line's agent": {
			PolicyRevision: 1, Tier: domain.TaskLineSelectionSoonestReset, Pool: "pool-b",
			RankedBy: input.Provenance.RankedBy,
			PassedOver: &domain.TaskLinePassedOverMark{
				Agent: "claude-b", Pool: "pool-a", Window: "seven_day_opus",
				Source: domain.ExhaustedMarkUsageReading, MarkedAt: at,
			},
		},
	} {
		in := input
		in.Provenance = &provenance
		if _, err := domain.NewTaskLine(in, 1, nil, at); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func cloneProvenance(p *domain.TaskLineProvenance) *domain.TaskLineProvenance {
	out := *p
	out.RankedBy = slices.Clone(p.RankedBy)
	if p.PassedOver != nil {
		mark := *p.PassedOver
		out.PassedOver = &mark
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
