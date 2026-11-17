package store

import (
	"go/ast"
	"slices"
	"strings"
	"testing"
)

// poolUsageReaders are the usage observation record's readers.
var poolUsageReaders = map[string]bool{
	"GetPoolUsageObservation":           true,
	"ListPoolUsageObservations":         true,
	"ListPoolUsageObservationSnapshots": true,
	"ExhaustedMarks":                    true,
	"ExhaustedMarksForModel":            true,
	"RecordedExhaustedMarks":            true,
	"WindowsForModel":                   true,
}

// poolUsageReaderFiles are the only non-test daemon files, or directories,
// that may name a reader, relative to daemon/. The record is observation,
// never authority (plan §5.4): no preflight, admission, or scheduling path
// reads it unless listed here. A new reader, such as the router, joins this
// list as a visible change that names its plan basis. The bootstrap's usage
// projection reads it because the record informs the account usage view in
// the clients (plan §5.4, usage observation); it projects, and decides
// nothing.
var poolUsageReaderFiles = []string{
	"internal/domain/pool_usage_observation.go",
	"internal/store/pool_usage_observation.go",
	"internal/usagecollect/",
	"cmd/freesided/auth_usage.go",
	// The exit-run fixture rewrites a pool's record to seed an exhausted
	// reading; it is an operator command and decides no admission.
	"cmd/freesided/auth_seed.go",
	"internal/signet/usage.go",
	// The quota park reads the refused window's mark at dispatch, and a
	// reading's reset when the refusal names none, to hold the next attempt
	// on its line until the window resets. It is §5.4's second reader above
	// admission beside the router, and it picks no agent, credential, or
	// pool (plan §4, §5.4, revision 103).
	"internal/engine/quota_park.go",
	// The router reads a pool's marks for an agent's model at a task's first
	// admission to pass a marked pool over, and before a routed role's
	// admission to hold it on its own pool's marked window (plan §5.4,
	// routing policy). Admission itself still reads only the task line.
	"internal/engine/router.go",
}

func TestOnlyAllowlistedFilesReadPoolUsageObservations(t *testing.T) {
	t.Parallel()
	parsed, err := daemonSources()
	if err != nil {
		t.Fatal(err)
	}
	inspected := 0
	for _, source := range parsed.sources {
		if slices.ContainsFunc(poolUsageReaderFiles, func(allowed string) bool {
			return source.rel == allowed || strings.HasSuffix(allowed, "/") && strings.HasPrefix(source.rel, allowed)
		}) {
			continue
		}
		inspected++
		ast.Inspect(source.file, func(node ast.Node) bool {
			if identifier, ok := node.(*ast.Ident); ok && poolUsageReaders[identifier.Name] {
				t.Errorf("%s references the pool usage record reader %s", source.rel, identifier.Name)
			}
			return true
		})
	}
	// A broken walk must not pass by inspecting nothing.
	if inspected < 500 {
		t.Fatalf("inspected %d daemon files", inspected)
	}
}
