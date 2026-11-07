package wardname

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

var roleFormat = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// TestRigShapesAreWellFormed checks the table's own invariants: each shape is
// distinct and names something, and each role is a plain, unique suffix.
func TestRigShapesAreWellFormed(t *testing.T) {
	t.Parallel()
	shapes := RigShapes()
	for i, shape := range shapes {
		if !strings.HasPrefix(shape.Prefix, "freeside-") || !strings.HasSuffix(shape.Prefix, "-") {
			t.Errorf("shape %d prefix %q is not a freeside- namespace ending in -", i, shape.Prefix)
		}
		if shape.RunID.HexDigits <= 0 {
			t.Errorf("shape %d (%s) has no ID digits", i, shape.Prefix)
		}
		if len(shape.Containers)+len(shape.Volumes)+len(shape.Networks) == 0 && !shape.BareContainer {
			t.Errorf("shape %d (%s) admits nothing", i, shape.Prefix)
		}
		for class, roles := range map[string][]string{
			"container": shape.Containers, "volume": shape.Volumes, "network": shape.Networks,
		} {
			for _, role := range roles {
				if !roleFormat.MatchString(role) {
					t.Errorf("shape %d (%s) %s role %q is not a plain suffix", i, shape.Prefix, class, role)
				}
			}
			if sorted := slices.Sorted(slices.Values(roles)); len(slices.Compact(sorted)) != len(roles) {
				t.Errorf("shape %d (%s) repeats a %s role: %q", i, shape.Prefix, class, roles)
			}
		}
		for _, other := range shapes[:i] {
			if other.Prefix == shape.Prefix && other.RunID == shape.RunID {
				t.Errorf("shape %d repeats prefix %q with ID %+v", i, shape.Prefix, shape.RunID)
			}
		}
	}
}

// TestRigShapesReturnsAFreshTable checks that a caller's edit to one result
// can't widen the namespace another caller reads.
func TestRigShapesReturnsAFreshTable(t *testing.T) {
	t.Parallel()
	first := RigShapes()
	first[0].Containers[0] = "widened"
	first[0].Prefix = "widened-"
	if second := RigShapes(); second[0].Containers[0] == "widened" || second[0].Prefix == "widened-" {
		t.Fatal("RigShapes shares its table across calls")
	}
}

func TestRigPatternsMatchBuiltNames(t *testing.T) {
	t.Parallel()
	container := regexp.MustCompile(RigContainerPattern())
	volume := regexp.MustCompile(RigVolumePattern())
	network := regexp.MustCompile(RigNetworkPattern())
	// A class with no alternatives would render ^()$, which admits the empty
	// name.
	for _, pattern := range []*regexp.Regexp{container, volume, network} {
		if pattern.MatchString("") {
			t.Errorf("pattern %q admits the empty name", pattern)
		}
	}
	for _, shape := range RigShapes() {
		id := shape.RunID.Lead + strings.Repeat("a", shape.RunID.HexDigits)
		for _, check := range []struct {
			pattern *regexp.Regexp
			roles   []string
		}{{container, shape.Containers}, {volume, shape.Volumes}, {network, shape.Networks}} {
			for _, role := range check.roles {
				if name := Name(shape.Prefix, id, role); !check.pattern.MatchString(name) {
					t.Errorf("pattern refuses %q", name)
				}
			}
		}
		if got := container.MatchString(BareName(shape.Prefix, id)); got != shape.BareContainer {
			t.Errorf("container pattern MatchString(%q) = %t, want %t", BareName(shape.Prefix, id), got, shape.BareContainer)
		}
	}
}

// TestRigPatternsQuoteLiterals checks that a role's literal characters
// stay literal: a dot or other metacharacter can't stand for another byte.
func TestRigPatternsQuoteLiterals(t *testing.T) {
	t.Parallel()
	got := rigPattern(func(Shape) ([]string, bool) { return []string{"a.b"}, true })
	pattern := regexp.MustCompile(got)
	id := strings.Repeat("0", 31)
	if pattern.MatchString(HandoffPrefix + "c" + id + "-axb") {
		t.Errorf("pattern %q treats a role's dot as a wildcard", got)
	}
	if !pattern.MatchString(HandoffPrefix + "c" + id + "-a.b") {
		t.Errorf("pattern %q refuses the literal role", got)
	}
}
