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

// pauseClassifiers are the calls that decide a run pauses without
// concluding. Their definitions are skipped: a classifier is not an exit.
var pauseClassifiers = map[string]bool{
	"MutableAdmissionPolicyRefusal": true,
	"dispatchHoldReason":            true,
	"invocationDispatchHold":        true,
	"unattendedDispatchRefusal":     true,
}

// pauseSentinels are the errors whose errors.Is check marks a pause exit,
// keyed as the source spells them.
var pauseSentinels = map[string]bool{
	"errShadowReviewBlocksReady":           true,
	"store.ErrTaskCancellationFenced":      true,
	"domain.ErrUnattendedOperationStopped": true,
	"domain.ErrBlockingSystemHealth":       true,
}

// holdRecorders are the calls that write a typed run hold.
var holdRecorders = map[string]bool{
	"observeRunHold":                   true,
	"observeRefusalHold":               true,
	"recordRunHold":                    true,
	"holdBlockedTask":                  true,
	"recordPublicationEnvironmentHold": true,
}

// holdlessPauseExits names the functions that use a pause classifier yet
// deliberately record no hold, each with the reason. An entry that no
// longer uses a classifier fails the test, so the list cannot claim an
// exception that is not there.
//
// A function listed for one quiet branch may still record on its others:
// the check is per function, so a branch that pauses without a hold inside a
// function that records elsewhere has to be listed by hand.
var holdlessPauseExits = map[string]string{
	// Task cancellation in progress: a task being cancelled is not held, it
	// is ending, and the cancellation owns what the operator sees.
	"external_review_reentry.go:planExternalReviewReentry":              cancellationInProgress,
	"operator_feedback.go:Engine.enqueueSpecificationAnswer":            cancellationInProgress,
	"operator_feedback.go:Engine.persistImplementationFeedback":         cancellationInProgress,
	"operator_feedback_retry.go:Engine.enqueueOperatorFeedbackRetry":    cancellationInProgress,
	"production_publication.go:RecordProductionExecutionExport":         cancellationInProgress,
	"publication_continuation.go:Engine.enqueuePublicationContinuation": cancellationInProgress,
	"readiness_reentry.go:StartReadinessReentry":                        cancellationInProgress,
	"specification.go:Engine.enqueueSpecRevision":                       cancellationInProgress,
	"specification.go:Engine.startApprovedImplementation":               cancellationInProgress,
	"specification_discussion.go:Engine.enqueueSpecDiscussion":          cancellationInProgress,
	"task_execution.go:Engine.ResumeTaskInvocation":                     cancellationInProgress,
	"task_execution.go:Engine.launchTaskInvocation":                     cancellationInProgress,
	"task_namer.go:Engine.nameTaskIfUnnamed":                            cancellationInProgress,

	// An attention discussion is a reply to an operator, not a run, so there
	// is no run to hold; the stop notice or the blocking item is already open.
	"attention_discussion.go:Engine.attentionDiscussionHeld": "gates a conversation reply, which has no run to hold",

	// Reads the cancellation fence to decide whether a held-work notice still
	// applies. It pauses nothing: the hold it reports on is already recorded.
	"held_work_notice.go:heldWorkTaskStopped": "reads the fence for the held-work notice; pauses no run",

	// Listed for their errShadowReviewBlocksReady branch alone; the other
	// branches record through holdBlockedTask. A shadow review waiting on its
	// open item already shows that item, and no reason code fits the wait.
	// A missing or damaged item takes the same quiet branch with no card
	// (#1645).
	"production_publication.go:productionPublicationWorkflow.reconcileTask":         shadowReviewItemWait,
	"production_publication.go:productionPublicationWorkflow.completePublishedTask": shadowReviewItemWait,
}

const (
	cancellationInProgress = "task cancellation in progress (store.ErrTaskCancellationFenced)"
	shadowReviewItemWait   = "shadow review waits on its open attention item; no reason code fits"
)

// unclassifiedPauseExits names deliberate no-hold pauses that use none of
// the classifiers, so the enumeration above cannot see them. They are
// listed so the exceptions stay in one place. Each must still exist and
// must still use no classifier; one that starts using a classifier belongs
// in the enumeration instead. A quiet branch inside a function that does use
// a classifier cannot be listed here; the known one is
// dispatchPendingInvocations skipping specification intents when no
// specification workflow is configured (#1646).
var unclassifiedPauseExits = map[string]string{
	"production_publication.go:productionPublicationWorkflow.recordPublicationEnvironmentHold": "a cancelled " +
		"context is the daemon shutting down, not a cause an operator can act on",
	"production_publication.go:productionPublicationWorkflow.reconcileReviewGate": "the review gate's " +
		"pending round shows its own review state; no reason code fits",
	"finding_adjudication.go:productionPublicationWorkflow.reconcileFindingAdjudication": "finding " +
		"adjudication waits on its open attention item; no reason code fits",
	"production_workflow.go:Engine.quarantinePendingProductionMarker": "a quarantined marker " +
		"raises its own quarantine attention item; no reason code fits",
}

// TestEveryPauseExitRecordsAHold is the #435 enumeration as a ratchet.
// PR #434 wired the hold sites it knew about, and three review rounds in a
// row then found another; nothing mechanical stood behind the rule that a
// pause whose cause has a reason code records it.
//
// What it proves is narrow: a function that uses a pause classifier also
// names a hold recorder, or is listed with a reason. It does not prove each
// branch records (a large function can record on one branch and pause
// quietly on another), and a pause that uses no classifier at all, such as
// a bare nil check followed by continue, is invisible to it. The
// behavioural tests beside each recorder cover the branches.
func TestEveryPauseExitRecordsAHold(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var found []string
	matched := map[string]bool{}
	unclassifiedSeen := map[string]bool{}
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
			if !ok || fn.Body == nil || pauseClassifiers[fn.Name.Name] {
				continue
			}
			site := name + ":" + funcKey(fn)
			if _, listed := unclassifiedPauseExits[site]; listed {
				unclassifiedSeen[site] = true
				if usesPauseClassifier(fn) {
					t.Errorf("unclassifiedPauseExits lists %s, but it now uses a pause classifier; "+
						"move it to holdlessPauseExits or record its hold", site)
				}
				continue
			}
			if !usesPauseClassifier(fn) {
				continue
			}
			if _, listed := holdlessPauseExits[site]; listed {
				matched[site] = true
				continue
			}
			if !callsAny(fn, holdRecorders) {
				found = append(found, site)
			}
		}
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Errorf("these functions pause a run on a classified cause without recording a hold:\n\t%s\n"+
			"Record the hold through one of the hold recorders, or list the function in "+
			"holdlessPauseExits with the reason it records none.",
			strings.Join(found, "\n\t"))
	}
	for site, reason := range holdlessPauseExits {
		if !matched[site] {
			t.Errorf("holdlessPauseExits lists %s (%s), but it no longer uses a pause classifier", site, reason)
		}
	}
	for site, reason := range unclassifiedPauseExits {
		if !unclassifiedSeen[site] {
			t.Errorf("unclassifiedPauseExits lists %s (%s), but no such function exists", site, reason)
		}
	}
}

// funcKey names a function, qualifying a method by its receiver type so two
// methods of the same name in one file stay distinct.
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if ident, ok := recv.(*ast.Ident); ok {
		return ident.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

func usesPauseClassifier(fn *ast.FuncDecl) bool {
	var hit bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if hit {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calleeName(call)
		if pauseClassifiers[name] {
			hit = true
			return false
		}
		if name == "Is" && isSelector(call.Fun, "errors") && len(call.Args) == 2 &&
			pauseSentinels[exprString(call.Args[1])] {
			hit = true
			return false
		}
		return true
	})
	return hit
}

func callsAny(fn *ast.FuncDecl, names map[string]bool) bool {
	var hit bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if hit {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok && names[calleeName(call)] {
			hit = true
			return false
		}
		return true
	})
	return hit
}

// calleeName is the bare name of a call's function: f for f(...) and
// x.f(...) alike.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

func isSelector(expr ast.Expr, pkg string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if ident, ok := e.X.(*ast.Ident); ok {
			return ident.Name + "." + e.Sel.Name
		}
	}
	return ""
}
