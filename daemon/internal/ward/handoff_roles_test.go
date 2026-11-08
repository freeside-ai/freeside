package ward

import (
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/wardname"
)

// TestHandoffRoleTableCoversEveryField fails when a runState objectClaim or
// a handoffNames field has no handoffRoles row, or more than one, so a role
// added to either struct cannot be missed by teardown, recovery, or the
// runtime namespace.
func TestHandoffRoleTableCoversEveryField(t *testing.T) {
	t.Parallel()
	var st runState
	var names handoffNames
	claimRows := rowsByField(t, reflect.ValueOf(&st).Elem(), reflect.TypeFor[objectClaim](),
		func(r handoffRole) any { return r.claim(&st) })
	nameRows := rowsByField(t, reflect.ValueOf(&names).Elem(), reflect.TypeFor[string](),
		func(r handoffRole) any { return r.name(&names) })
	// A row binds the claim and the name of the same role: the two structs
	// spell each role's field alike apart from case.
	for i := range handoffRoles {
		if !strings.EqualFold(claimRows[i], nameRows[i]) {
			t.Errorf("row %d (%s) binds claim %q to name %q", i, handoffRoles[i].role, claimRows[i], nameRows[i])
		}
	}
}

// rowsByField maps each handoffRoles row index to the field of v its
// accessor returns, failing when a field of type want has no row or more
// than one.
func rowsByField(
	t *testing.T, v reflect.Value, want reflect.Type, accessor func(handoffRole) any,
) map[int]string {
	t.Helper()
	fields := map[uintptr]string{}
	for i := range v.NumField() {
		if v.Field(i).Type() == want {
			fields[v.Field(i).Addr().Pointer()] = v.Type().Field(i).Name
		}
	}
	rows := map[int]string{}
	bound := map[string]int{}
	for i, r := range handoffRoles {
		field, ok := fields[reflect.ValueOf(accessor(r)).Pointer()]
		if !ok {
			t.Fatalf("row %d (%s) binds no %s field of %s", i, r.role, want, v.Type())
		}
		rows[i] = field
		bound[field]++
	}
	for _, field := range fields {
		if bound[field] != 1 {
			t.Errorf("%s.%s is bound by %d rows, want 1", v.Type(), field, bound[field])
		}
	}
	return rows
}

// TestHandoffRoleTableRows checks each row's own fields: a unique role, a
// registered kind and feature, and recovery's reap order as exactly the
// places 1 through n over containers other than the agent.
func TestHandoffRoleTableRows(t *testing.T) {
	t.Parallel()
	roles := map[string]bool{}
	places := map[int]string{}
	for _, r := range handoffRoles {
		if r.role == "" || roles[r.role] {
			t.Errorf("role %q is empty or repeated", r.role)
		}
		roles[r.role] = true
		if !r.kind.valid() || !r.feature.valid() {
			t.Errorf("role %q has kind %q and feature %q", r.role, r.kind, r.feature)
		}
		if r.recoveryReap == 0 {
			continue
		}
		// Adoption proves the writer absent and reaps it, when a failure
		// path does, only through reapRecoveredAgent, which journals a
		// memory-limit kill first; the plain adoption reap would skip that.
		if r.role == wardname.Agent {
			t.Errorf("the agent has recovery reap place %d, want none", r.recoveryReap)
		}
		if r.kind != handoffContainer {
			t.Errorf("role %q is a %s with a recovery reap place", r.role, r.kind)
		}
		if prior, dup := places[r.recoveryReap]; dup {
			t.Errorf("roles %q and %q share recovery reap place %d", prior, r.role, r.recoveryReap)
		}
		places[r.recoveryReap] = r.role
	}
	for place := 1; place <= len(places); place++ {
		if _, ok := places[place]; !ok {
			t.Errorf("recovery reap places skip %d (have %d places)", place, len(places))
		}
	}
}

func TestHandoffRoleKindEnum(t *testing.T) {
	t.Parallel()
	for _, k := range AllHandoffRoleKinds {
		if !k.valid() {
			t.Errorf("registered kind %q invalid", k)
		}
	}
	if handoffRoleKind("").valid() {
		t.Error("zero kind valid")
	}
}

func TestHandoffFeatureEnum(t *testing.T) {
	t.Parallel()
	for _, f := range AllHandoffFeatures {
		if !f.valid() {
			t.Errorf("registered feature %q invalid", f)
		}
	}
	if handoffFeature("").valid() {
		t.Error("zero feature valid")
	}
}
