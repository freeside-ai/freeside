package ward

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/wardname"
)

// These tests tie ward's runtime names to the namespace the production rig
// admits (wardname.RigShapes, rendered into internal/daemonlock's patterns).
// A new ward role fails here until it is registered there, and a registered
// role ward no longer produces fails here until it is removed.

// wardRigNameSource is one enumerator's output for a sample ID of the shape
// its production caller mints.
type wardRigNameSource struct {
	enumerator string
	names      RuntimeResourceNames
}

const (
	coverageProductionRunID = "c0123456789abcdef0123456789abcde"
	coverageReviewRunID     = "review-0123456789abcdef01234567"
	coverageConfRunID       = "conf-0123456789abcdef"
	coverageConfRegistryID  = "conf-fedcba9876543210"
)

func wardRigNameSources(t *testing.T) []wardRigNameSource {
	t.Helper()
	owner, err := newOwnershipLabel()
	if err != nil {
		t.Fatal(err)
	}
	slice := owner.Value[:12]
	sources := []wardRigNameSource{
		{"RuntimeResourceNamesFor", RuntimeResourceNamesFor(coverageProductionRunID)},
		{"CodexReviewRuntimeResourceNamesFor", CodexReviewRuntimeResourceNamesFor(coverageReviewRunID)},
		{"codexReviewWorkspaceRuntimeResourceNames", codexReviewWorkspaceRuntimeResourceNames(coverageReviewRunID)},
		{"legacyCodexReviewNames", codexReviewRuntimeResourceNames(
			coverageReviewRunID, legacyCodexReviewNames(coverageReviewRunID))},
		{"preSnapshotCodexReviewNames", codexReviewRuntimeResourceNames(
			coverageReviewRunID, preSnapshotCodexReviewNames(coverageReviewRunID))},
		{"FullConformanceRuntimeResourceNamesFor", FullConformanceRuntimeResourceNamesFor(
			coverageConfRunID, coverageConfRegistryID)},
		{"PreJobContainerNameForInvocation", RuntimeResourceNames{
			Containers: []string{PreJobContainerNameForInvocation(domain.InvocationID("inv-rig-coverage"))},
		}},
		{"preflightCredentialContainerName", RuntimeResourceNames{
			Containers: []string{preflightCredentialContainerName(slice)},
		}},
		{"lineupProbe names", RuntimeResourceNames{
			Containers: []string{lineupProbeContainerName(slice)},
			Networks:   []string{lineupProbeNetworkName(slice)},
		}},
	}
	for _, provider := range allReviewProviders() {
		sources = append(sources, wardRigNameSource{
			"reviewRuntimeResourceNamesFor/" + provider.sourceLabel(),
			reviewRuntimeResourceNamesFor(provider, coverageReviewRunID),
		})
	}
	return sources
}

type wardRigClass struct {
	name    string
	pattern *regexp.Regexp
	names   func(RuntimeResourceNames) []string
	roles   func(wardname.Shape) []string
	bare    func(wardname.Shape) bool
}

// wardRigClasses compiles the leaf's patterns exactly as daemonlock does.
func wardRigClasses() []wardRigClass {
	return []wardRigClass{
		{
			name: "container", pattern: regexp.MustCompile(wardname.RigContainerPattern()),
			names: func(r RuntimeResourceNames) []string { return r.Containers },
			roles: func(s wardname.Shape) []string { return s.Containers },
			bare:  func(s wardname.Shape) bool { return s.BareContainer },
		},
		{
			name: "volume", pattern: regexp.MustCompile(wardname.RigVolumePattern()),
			names: func(r RuntimeResourceNames) []string { return r.Volumes },
			roles: func(s wardname.Shape) []string { return s.Volumes },
			bare:  func(wardname.Shape) bool { return false },
		},
		{
			name: "network", pattern: regexp.MustCompile(wardname.RigNetworkPattern()),
			names: func(r RuntimeResourceNames) []string { return r.Networks },
			roles: func(s wardname.Shape) []string { return s.Networks },
			bare:  func(wardname.Shape) bool { return false },
		},
	}
}

func TestWardRigNamesAreAdmittedByTheRig(t *testing.T) {
	t.Parallel()
	for _, source := range wardRigNameSources(t) {
		for _, class := range wardRigClasses() {
			for _, name := range class.names(source.names) {
				if !class.pattern.MatchString(name) {
					t.Errorf("%s produces %s %q, which the rig patterns refuse; register its role in wardname.RigShapes",
						source.enumerator, class.name, name)
				}
			}
		}
	}
}

// TestRigShapesAreProducedByWard fails for a shape-and-role pair the rig
// admits but no ward enumerator produces, so the rig never admits more than
// ward creates.
func TestRigShapesAreProducedByWard(t *testing.T) {
	t.Parallel()
	sources := wardRigNameSources(t)
	for _, shape := range wardname.RigShapes() {
		stem := regexp.QuoteMeta(shape.Prefix) + regexp.QuoteMeta(shape.RunID.Lead) +
			"[0-9a-f]{" + strconv.Itoa(shape.RunID.HexDigits) + "}"
		for _, class := range wardRigClasses() {
			var suffixes []string
			for _, role := range class.roles(shape) {
				suffixes = append(suffixes, "-"+role)
			}
			if class.bare(shape) {
				suffixes = append(suffixes, "")
			}
			for _, suffix := range suffixes {
				exact := regexp.MustCompile("^" + stem + regexp.QuoteMeta(suffix) + "$")
				if !anyWardRigName(sources, class, exact) {
					t.Errorf("rig admits %s %s<%s ID>%s, but no ward enumerator produces it",
						class.name, shape.Prefix, shapeIDLabel(shape.RunID), suffix)
				}
			}
		}
	}
}

// TestWardNameStructsReachAnEnumerator fails when a field of a name struct is
// built but no enumerator lists it: such a name would never be bound to the
// rig, so the two tests above couldn't see it.
func TestWardNameStructsReachAnEnumerator(t *testing.T) {
	t.Parallel()
	produced := map[string]bool{}
	for _, source := range wardRigNameSources(t) {
		for _, class := range wardRigClasses() {
			for _, name := range class.names(source.names) {
				produced[name] = true
			}
		}
	}
	structs := []struct {
		label string
		value any
	}{
		{"handoffNames", namesFor(coverageProductionRunID)},
		{"legacyCodexReviewNames", legacyCodexReviewNames(coverageReviewRunID)},
		{"preSnapshotCodexReviewNames", preSnapshotCodexReviewNames(coverageReviewRunID)},
	}
	for _, provider := range allReviewProviders() {
		structs = append(structs, struct {
			label string
			value any
		}{"reviewNames/" + provider.sourceLabel(), reviewNames(provider, coverageReviewRunID)})
	}
	for _, s := range structs {
		value := reflect.ValueOf(s.value)
		for i := range value.NumField() {
			field := value.Type().Field(i)
			if field.Type.Kind() != reflect.String {
				t.Fatalf("%s.%s is not a name string; extend this test", s.label, field.Name)
			}
			// Empty fields are the legacy generations' absent resources.
			if name := value.Field(i).String(); name != "" && !produced[name] {
				t.Errorf("%s.%s = %q reaches no rig enumerator", s.label, field.Name, name)
			}
		}
	}
}

func anyWardRigName(sources []wardRigNameSource, class wardRigClass, exact *regexp.Regexp) bool {
	for _, source := range sources {
		for _, name := range class.names(source.names) {
			if exact.MatchString(name) {
				return true
			}
		}
	}
	return false
}

func shapeIDLabel(id wardname.IDShape) string {
	return strings.TrimSuffix(id.Lead, "-") + strconv.Itoa(id.HexDigits) + "hex"
}
