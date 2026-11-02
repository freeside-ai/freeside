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

func routingPolicyInput(roles ...domain.RoutingPolicyRole) domain.RoutingPolicyInput {
	return domain.RoutingPolicyInput{
		Roles: roles, Source: domain.RoutingPolicySourceCLISet, SetBy: "cli:operator",
		LineupRevision: routingPolicyRevision,
	}
}

func TestNewRoutingPolicyCanonicalizes(t *testing.T) {
	t.Parallel()
	order := []string{"claude-a", "claude-b"}
	policy, err := domain.NewRoutingPolicy(routingPolicyInput(
		domain.RoutingPolicyRole{Role: domain.RoleReviewer, Agents: []string{"codex"}},
		domain.RoutingPolicyRole{Role: domain.RoleSpecifier, Agents: order},
	), 1, routingPolicyAt.In(time.FixedZone("elsewhere", 3600)))
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.RoutingPolicyRole{
		{Role: domain.RoleSpecifier, Agents: []string{"claude-a", "claude-b"}},
		{Role: domain.RoleReviewer, Agents: []string{"codex"}},
	}
	if !reflect.DeepEqual(policy.Roles, want) {
		t.Fatalf("roles = %+v, want %+v", policy.Roles, want)
	}
	if policy.SetAt.Location() != time.UTC || !policy.SetAt.Equal(routingPolicyAt) {
		t.Fatalf("set_at = %v", policy.SetAt)
	}
	// A revision is never edited, so it must not share the caller's slice.
	order[0] = "edited"
	if policy.Roles[0].Agents[0] != "claude-a" {
		t.Fatalf("roles alias the input: %+v", policy.Roles)
	}
}

func TestRoutingPolicySources(t *testing.T) {
	t.Parallel()
	for _, source := range domain.AllRoutingPolicySources {
		in := routingPolicyInput(domain.RoutingPolicyRole{Role: domain.RoleImplementer, Agents: []string{"claude-a"}})
		in.Source = source
		if _, err := domain.NewRoutingPolicy(in, 1, routingPolicyAt); err != nil {
			t.Errorf("%s: %v", source, err)
		}
	}
}

// TestRoutingPolicyRefusals pins each refusal. A refused role or agent is
// request text, so the error must not repeat it.
func TestRoutingPolicyRefusals(t *testing.T) {
	t.Parallel()
	const pasted = "SECRET-TOKEN"
	implementer := func(agents ...string) domain.RoutingPolicyRole {
		return domain.RoutingPolicyRole{Role: domain.RoleImplementer, Agents: agents}
	}
	for name, tc := range map[string]struct {
		roles []domain.RoutingPolicyRole
		want  error
	}{
		"no roles":        {nil, domain.ErrRoutingPolicyNoRoles},
		"shadow reviewer": {[]domain.RoutingPolicyRole{{Role: domain.RoleShadowReviewer, Agents: []string{"a"}}}, domain.ErrTaskLineRoleIneligible},
		"wardless role":   {[]domain.RoutingPolicyRole{{Role: domain.RoleBriefer, Agents: []string{"a"}}}, domain.ErrTaskLineRoleIneligible},
		"refused role":    {[]domain.RoutingPolicyRole{{Role: pasted, Agents: []string{"a"}}}, domain.ErrTaskLineRoleIneligible},
		"repeated role":   {[]domain.RoutingPolicyRole{implementer("a"), implementer("b")}, domain.ErrRoutingPolicyRoleRepeated},
		"nil order":       {[]domain.RoutingPolicyRole{{Role: domain.RoleImplementer}}, domain.ErrRoutingPolicyOrderEmpty},
		"empty order":     {[]domain.RoutingPolicyRole{implementer()}, domain.ErrRoutingPolicyOrderEmpty},
		"repeated agent":  {[]domain.RoutingPolicyRole{implementer("a", "b", "a")}, domain.ErrRoutingPolicyAgentRepeated},
		"empty agent":     {[]domain.RoutingPolicyRole{implementer("")}, domain.ErrInvalidAgentName},
		"refused agent":   {[]domain.RoutingPolicyRole{implementer("a", pasted)}, domain.ErrInvalidAgentName},
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
				{Role: domain.RoleSpecifier, Agents: []string{"claude-a"}},
				{Role: domain.RoleImplementer, Agents: []string{"claude-a", "claude-b"}},
			},
			Source: domain.RoutingPolicySourceCLISet, SetBy: "cli:operator", SetAt: routingPolicyAt,
			LineupRevision: routingPolicyRevision,
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		edit func(*domain.RoutingPolicy)
		want error
	}{
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
	} {
		policy := valid()
		tc.edit(&policy)
		if err := policy.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

func TestRoutingPolicySameOrder(t *testing.T) {
	t.Parallel()
	policy, err := domain.NewRoutingPolicy(routingPolicyInput(
		domain.RoutingPolicyRole{Role: domain.RoleImplementer, Agents: []string{"claude-a", "claude-b"}},
	), 3, routingPolicyAt)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		roles []domain.RoutingPolicyRole
		want  bool
	}{
		"same":           {[]domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Agents: []string{"claude-a", "claude-b"}}}, true},
		"swapped agents": {[]domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Agents: []string{"claude-b", "claude-a"}}}, false},
		"another role":   {[]domain.RoutingPolicyRole{{Role: domain.RoleSpecifier, Agents: []string{"claude-a", "claude-b"}}}, false},
		"added role": {[]domain.RoutingPolicyRole{
			{Role: domain.RoleImplementer, Agents: []string{"claude-a", "claude-b"}},
			{Role: domain.RoleReviewer, Agents: []string{"codex"}},
		}, false},
	} {
		if got := policy.SameOrder(tc.roles); got != tc.want {
			t.Errorf("%s: %t, want %t", name, got, tc.want)
		}
	}
}
