package domain_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var (
	routingPolicyAt       = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	routingPolicyRevision = domain.Digest("sha256:" + strings.Repeat("a", 64))
)

// enrollments is an order over enrollment ids.
func enrollments(ids ...string) []domain.ClientEnrollmentID {
	order := make([]domain.ClientEnrollmentID, len(ids))
	for i, id := range ids {
		order[i] = domain.ClientEnrollmentID(id)
	}
	return order
}

func routingPolicyInput(roles ...domain.RoutingPolicyRole) domain.RoutingPolicyInput {
	return domain.RoutingPolicyInput{
		Roles: roles, Source: domain.RoutingPolicySourceCLISet, SetBy: "cli:operator",
		LineupRevision: routingPolicyRevision,
	}
}

func TestNewRoutingPolicyCanonicalizes(t *testing.T) {
	t.Parallel()
	order := enrollments("claude-a/claude_code", "claude-b/claude_code")
	policy, err := domain.NewRoutingPolicy(routingPolicyInput(
		domain.RoutingPolicyRole{Role: domain.RoleReviewer, Enrollments: enrollments("codex-a/codex")},
		domain.RoutingPolicyRole{Role: domain.RoleSpecifier, Enrollments: order},
	), 1, routingPolicyAt.In(time.FixedZone("elsewhere", 3600)))
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.RoutingPolicyRole{
		{Role: domain.RoleSpecifier, Enrollments: enrollments("claude-a/claude_code", "claude-b/claude_code")},
		{Role: domain.RoleReviewer, Enrollments: enrollments("codex-a/codex")},
	}
	if !reflect.DeepEqual(policy.Roles, want) {
		t.Fatalf("roles = %+v, want %+v", policy.Roles, want)
	}
	if policy.SetAt.Location() != time.UTC || !policy.SetAt.Equal(routingPolicyAt) {
		t.Fatalf("set_at = %v", policy.SetAt)
	}
	// A revision is never edited, so it must not share the caller's slice.
	order[0] = "edited"
	if policy.Roles[0].Enrollments[0] != "claude-a/claude_code" {
		t.Fatalf("roles alias the input: %+v", policy.Roles)
	}
	if policy.DrainFirstAccount != nil {
		t.Fatalf("drain_first_account = %v, want none", *policy.DrainFirstAccount)
	}

	account := domain.UsagePoolID("pool-a")
	in := routingPolicyInput(domain.RoutingPolicyRole{Role: domain.RoleImplementer, Enrollments: enrollments("claude-a/claude_code")})
	in.DrainFirstAccount = &account
	drained, err := domain.NewRoutingPolicy(in, 2, routingPolicyAt)
	if err != nil {
		t.Fatal(err)
	}
	account = "edited"
	if drained.DrainFirstAccount == nil || *drained.DrainFirstAccount != "pool-a" {
		t.Fatalf("drain_first_account = %v, want pool-a held apart from the input", drained.DrainFirstAccount)
	}
}

func TestRoutingPolicySources(t *testing.T) {
	t.Parallel()
	for _, source := range domain.AllRoutingPolicySources {
		in := routingPolicyInput(domain.RoutingPolicyRole{Role: domain.RoleImplementer, Enrollments: enrollments("claude-a/claude_code")})
		in.Source = source
		if _, err := domain.NewRoutingPolicy(in, 1, routingPolicyAt); err != nil {
			t.Errorf("%s: %v", source, err)
		}
	}
}

// TestRoutingPolicyRefusals pins each refusal. A refused role or enrollment
// is request text, so the error must not repeat it.
func TestRoutingPolicyRefusals(t *testing.T) {
	t.Parallel()
	const pasted = "SECRET-TOKEN"
	implementer := func(ids ...string) domain.RoutingPolicyRole {
		return domain.RoutingPolicyRole{Role: domain.RoleImplementer, Enrollments: enrollments(ids...)}
	}
	for name, tc := range map[string]struct {
		roles []domain.RoutingPolicyRole
		want  error
	}{
		"no roles":            {nil, domain.ErrRoutingPolicyNoRoles},
		"shadow reviewer":     {[]domain.RoutingPolicyRole{{Role: domain.RoleShadowReviewer, Enrollments: enrollments("a")}}, domain.ErrTaskLineRoleIneligible},
		"wardless role":       {[]domain.RoutingPolicyRole{{Role: domain.RoleBriefer, Enrollments: enrollments("a")}}, domain.ErrTaskLineRoleIneligible},
		"refused role":        {[]domain.RoutingPolicyRole{{Role: pasted, Enrollments: enrollments("a")}}, domain.ErrTaskLineRoleIneligible},
		"repeated role":       {[]domain.RoutingPolicyRole{implementer("a"), implementer("b")}, domain.ErrRoutingPolicyRoleRepeated},
		"nil order":           {[]domain.RoutingPolicyRole{{Role: domain.RoleImplementer}}, domain.ErrRoutingPolicyOrderEmpty},
		"empty order":         {[]domain.RoutingPolicyRole{implementer()}, domain.ErrRoutingPolicyOrderEmpty},
		"repeated enrollment": {[]domain.RoutingPolicyRole{implementer("a", "b", "a")}, domain.ErrRoutingPolicyEnrollmentRepeated},
		"empty enrollment":    {[]domain.RoutingPolicyRole{implementer("")}, domain.ErrInvalidRoutingPolicyEnrollment},
		"refused enrollment":  {[]domain.RoutingPolicyRole{implementer("a", "-"+pasted)}, domain.ErrInvalidRoutingPolicyEnrollment},
		"spaced enrollment":   {[]domain.RoutingPolicyRole{implementer("a", pasted+" x")}, domain.ErrInvalidRoutingPolicyEnrollment},
		// A new revision orders enrollments; the agents form is one only a
		// stored revision may hold.
		"agents form": {[]domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Agents: []string{"claude-a"}}}, domain.ErrRoutingPolicyAgentsForm},
		"mixed form": {[]domain.RoutingPolicyRole{{
			Role: domain.RoleImplementer, Enrollments: enrollments("a"), Agents: []string{"claude-a"},
		}}, domain.ErrRoutingPolicyFormMixed},
	} {
		_, err := domain.NewRoutingPolicy(routingPolicyInput(tc.roles...), 1, routingPolicyAt)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), pasted) {
			t.Errorf("%s: the error repeats the refused text: %v", name, err)
		}
	}
}

// TestRoutingPolicyValidateRefusesDecodedShapes covers what no constructor
// builds: a stored body reaches Validate directly.
func TestRoutingPolicyValidateRefusesDecodedShapes(t *testing.T) {
	t.Parallel()
	valid := func() domain.RoutingPolicy {
		return domain.RoutingPolicy{
			Revision: 1,
			Roles: []domain.RoutingPolicyRole{
				{Role: domain.RoleSpecifier, Enrollments: enrollments("claude-a/claude_code")},
				{Role: domain.RoleImplementer, Enrollments: enrollments("claude-a/claude_code", "claude-b/claude_code")},
			},
			Source: domain.RoutingPolicySourceCLISet, SetBy: "cli:operator", SetAt: routingPolicyAt,
			LineupRevision: routingPolicyRevision,
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	// A revision stored before enrollments holds the agents form, and the
	// store re-runs Validate on its decode.
	legacy := valid()
	legacy.Roles = []domain.RoutingPolicyRole{
		{Role: domain.RoleSpecifier, Agents: []string{"claude-a"}},
		{Role: domain.RoleImplementer, Agents: []string{"claude-a", "claude-b"}},
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("a pre-enrollment revision: %v", err)
	}
	for name, tc := range map[string]struct {
		edit func(*domain.RoutingPolicy)
		want error
	}{
		"mixed form": {func(p *domain.RoutingPolicy) { p.Roles[0].Agents = []string{"claude-a"} }, domain.ErrRoutingPolicyFormMixed},
		"repeated agent": {func(p *domain.RoutingPolicy) {
			p.Roles[0] = domain.RoutingPolicyRole{Role: domain.RoleSpecifier, Agents: []string{"claude-a", "claude-a"}}
		}, domain.ErrRoutingPolicyAgentRepeated},
		"bad agent": {func(p *domain.RoutingPolicy) {
			p.Roles[0] = domain.RoutingPolicyRole{Role: domain.RoleSpecifier, Agents: []string{"Claude A"}}
		}, domain.ErrInvalidAgentName},
		"revision zero":   {func(p *domain.RoutingPolicy) { p.Revision = 0 }, domain.ErrNonPositive},
		"revision below":  {func(p *domain.RoutingPolicy) { p.Revision = -1 }, domain.ErrNonPositive},
		"no roles":        {func(p *domain.RoutingPolicy) { p.Roles = nil }, domain.ErrRoutingPolicyNoRoles},
		"out of order":    {func(p *domain.RoutingPolicy) { p.Roles[0], p.Roles[1] = p.Roles[1], p.Roles[0] }, domain.ErrRoutingPolicyNotCanonical},
		"unknown source":  {func(p *domain.RoutingPolicy) { p.Source = "intake" }, domain.ErrInvalidRoutingPolicySource},
		"empty source":    {func(p *domain.RoutingPolicy) { p.Source = "" }, domain.ErrInvalidRoutingPolicySource},
		"empty set_by":    {func(p *domain.RoutingPolicy) { p.SetBy = "" }, domain.ErrEmptyField},
		"zero set_at":     {func(p *domain.RoutingPolicy) { p.SetAt = time.Time{} }, domain.ErrMissingTimestamp},
		"set_at not UTC":  {func(p *domain.RoutingPolicy) { p.SetAt = p.SetAt.In(time.FixedZone("x", 60)) }, domain.ErrTimestampNotUTC},
		"lineup revision": {func(p *domain.RoutingPolicy) { p.LineupRevision = "not-a-digest" }, domain.ErrInvalidDigest},
		"empty account":   {func(p *domain.RoutingPolicy) { p.DrainFirstAccount = new(domain.UsagePoolID) }, domain.ErrEmptyID},
	} {
		policy := valid()
		tc.edit(&policy)
		if err := policy.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

func TestRoutingPolicySameRule(t *testing.T) {
	t.Parallel()
	in := routingPolicyInput(
		domain.RoutingPolicyRole{Role: domain.RoleImplementer, Enrollments: enrollments("claude-a/claude_code", "claude-b/claude_code")},
	)
	in.DrainFirstAccount = new(domain.UsagePoolID("pool-a"))
	policy, err := domain.NewRoutingPolicy(in, 3, routingPolicyAt)
	if err != nil {
		t.Fatal(err)
	}
	same := []domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Enrollments: enrollments("claude-a/claude_code", "claude-b/claude_code")}}
	for name, tc := range map[string]struct {
		roles   []domain.RoutingPolicyRole
		account *domain.UsagePoolID
		want    bool
	}{
		"same":          {same, new(domain.UsagePoolID("pool-a")), true},
		"swapped order": {[]domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Enrollments: enrollments("claude-b/claude_code", "claude-a/claude_code")}}, new(domain.UsagePoolID("pool-a")), false},
		"another role":  {[]domain.RoutingPolicyRole{{Role: domain.RoleSpecifier, Enrollments: enrollments("claude-a/claude_code", "claude-b/claude_code")}}, new(domain.UsagePoolID("pool-a")), false},
		"added role": {[]domain.RoutingPolicyRole{
			{Role: domain.RoleImplementer, Enrollments: enrollments("claude-a/claude_code", "claude-b/claude_code")},
			{Role: domain.RoleReviewer, Enrollments: enrollments("codex-a/codex")},
		}, new(domain.UsagePoolID("pool-a")), false},
		// The account is part of the rule: the same orders with another
		// account, or with none, are a change.
		"another account": {same, new(domain.UsagePoolID("pool-b")), false},
		"account cleared": {same, nil, false},
	} {
		if got := policy.SameRule(tc.roles, tc.account); got != tc.want {
			t.Errorf("%s: %t, want %t", name, got, tc.want)
		}
	}
	in.DrainFirstAccount = nil
	unset, err := domain.NewRoutingPolicy(in, 4, routingPolicyAt)
	if err != nil {
		t.Fatal(err)
	}
	if !unset.SameRule(same, nil) || unset.SameRule(same, new(domain.UsagePoolID("pool-a"))) {
		t.Error("a policy with no account matches only a request with none")
	}
}
