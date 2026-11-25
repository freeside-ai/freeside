package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// newWorkWrites are the store writes that create a task or a run, or seal a
// re-entry cycle's authority: the writes new work starts with.
var newWorkWrites = map[string]bool{
	"AssignTask":                 true,
	"AcceptManualSubmission":     true,
	"PutRun":                     true,
	"RecordPublicationSuccessor": true,
}

// readinessGateCall is the call that applies the project readiness gate.
const readinessGateCall = "requireProjectReady"

// readinessRelays write new work but leave the gate to their callers, each
// with the reason. A relay's own name joins newWorkWrites, so each of its
// callers must call the gate or be listed too.
var readinessRelays = map[string]string{
	"specification.go:submitSpecificationRunTx": "creates the run for both a new submission and a " +
		"specification revision; submitSpecArtifactRunTx gates the new submission",
}

// readinessExemptions write a task, a run, or a re-entry authority without
// the gate, each with the reason. None starts new work for a project: each
// continues or records work that already exists, and the work it leads to
// is an admitted invocation, which the project_not_ready card holds through
// the operating gate.
var readinessExemptions = map[string]string{
	"fake_workflow.go:Engine.StartFakeRun":        fakeLane,
	"fake_workflow.go:Engine.ensureFeedbackStage": fakeLane,

	"invocation.go:Engine.recordAttempt":                               "records an attempt on an existing run",
	"operator_feedback.go:Engine.enqueueSpecificationRevisionCampaign": existingTask,
	"operator_feedback.go:Engine.persistImplementationFeedback":        existingTask,
	"operator_feedback_retry.go:Engine.enqueueOperatorFeedbackRetry":   existingTask,
	"production_workflow.go:submitProductionRunTx": "starts the implementation run an approved " +
		"specification already authorized, on the specification's task",
	"remediation.go:preparedRemediationIntent.persist": existingTask,
	"production_handoff.go:preparedRemediationIntent.persistHandoffConflict": "remediates a handoff " +
		"conflict on an existing run",
	"production_handoff.go:preparedRemediationIntent.commitFirstRound": "starts a handoff conflict's " +
		"or a check failure's remediation on an existing run",
	"production_export_record.go:RecordProductionExecutionExport": "records an existing run's export " +
		"and the publication task that replays it",
	"publication_continuation.go:persistContinuationAuthority": "continues an existing run's publication",
}

const (
	fakeLane     = "the development fake lane has no project settings and no readiness record"
	existingTask = "continues an existing task's work at the operator's or the daemon's request"
)

// TestEveryNewWorkPathChecksProjectReadiness is the #3205 enumeration. A
// project that is not ready takes no new work: every engine function that
// creates a task or a run, or seals a re-entry cycle, either applies the
// readiness gate, leaves it to callers that do (readinessRelays), or is
// listed with the reason it needs none (readinessExemptions).
//
// What it proves is narrow: the function names the gate call, not that the
// call precedes every write on every branch. The behavioural tests beside
// each gated path cover the order.
func TestEveryNewWorkPathChecksProjectReadiness(t *testing.T) {
	t.Parallel()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	writes := map[string]bool{}
	for name := range newWorkWrites {
		writes[name] = true
	}
	for site := range readinessRelays {
		writes[site[strings.LastIndex(site, ":")+1:]] = true
	}
	fset := token.NewFileSet()
	var found []string
	seen := map[string]bool{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == readinessGateCall {
				continue
			}
			if !callsAny(fn, writes) {
				continue
			}
			site := name + ":" + funcKey(fn)
			_, relay := readinessRelays[site]
			_, exempt := readinessExemptions[site]
			if relay || exempt {
				seen[site] = true
				continue
			}
			if !callsAny(fn, map[string]bool{readinessGateCall: true}) {
				found = append(found, site)
			}
		}
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Errorf("these functions start new work without the project readiness gate:\n\t%s\n"+
			"Call %s before the first write, or list the function in readinessExemptions "+
			"with the reason it needs none.", strings.Join(found, "\n\t"), readinessGateCall)
	}
	for _, list := range []map[string]string{readinessRelays, readinessExemptions} {
		for site, reason := range list {
			if !seen[site] {
				t.Errorf("%s is listed (%s), but no such function writes new work", site, reason)
			}
		}
	}
}
