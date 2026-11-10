package engine

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// An admission refusal of a task line the daemon does not serve names which
// part failed: the enrollment, the model, or the effort (issue #2992). It
// names the held enrollment for a model or effort, and never what the
// operator typed: a credential can be pasted into a line.

// unservedCanary stands for a credential pasted into a line's selection.
const unservedCanary = "sk-ant-oat01-unserved-canary"

// unservedCatalogModel is the model the fixture catalog lists. The effort
// leg types the canary as this model, which the catalog then lists too: an
// effort no adapter can send is refused before the served-tuple rule runs
// (ErrNoAdapterForLaunch, which names no part), so the effort leg needs a
// native effort, and the canary rides on the model instead.
const unservedCatalogModel = "claude-implementer-test"

// refusalHoldAndNotice records refusal as an admission hold on a run, ages
// the hold past the notice threshold, and returns the stored hold and the
// held-work notice it raised.
func refusalHoldAndNotice(t *testing.T, refusal error) (domain.RunHoldObservation, domain.AttentionItem) {
	t.Helper()
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "unused")
	observe := func(engine *Engine) {
		t.Helper()
		if err := engine.observeRefusalHold(t.Context(), f.run.ID, f.attempt.InvocationID, refusal); err != nil {
			t.Fatal(err)
		}
	}
	observe(f.newEngine(t, f.driver))
	backdateRunHold(t, f.store, f.run.ID, heldWorkNoticeAfter+time.Minute)
	// A fresh engine, so the pacer does not hold back the second pass.
	observe(f.newEngine(t, f.driver))
	var hold domain.RunHoldObservation
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		hold, _, err = tx.GetRunHold(t.Context(), f.run.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	open := openHeldWorkNotices(t, f.store)
	if len(open) != 1 {
		t.Fatalf("open notices = %+v, want one", open)
	}
	return hold, open[0]
}

func TestUnservedTaskLineNamesThePart(t *testing.T) {
	t.Parallel()
	enrollment := domain.ClientEnrollmentID(agentTestEnrollment)
	cases := []struct {
		name      string
		catalog   string // the model the catalog lists, with efforts low and high
		selection domain.TaskLineSelection
		want      domain.AdmissionUnserved
		clause    string
	}{
		{
			name:    "an enrollment the daemon does not hold",
			catalog: unservedCatalogModel,
			selection: domain.TaskLineSelection{
				EnrollmentID: unservedCanary, ModelID: unservedCatalogModel, Effort: "high",
			},
			want:   domain.AdmissionUnserved{Part: domain.AgentSelectionPartEnrollment},
			clause: "because the daemon holds no such enrollment.",
		},
		{
			name:    "a model the enrollment does not serve",
			catalog: unservedCatalogModel,
			selection: domain.TaskLineSelection{
				EnrollmentID: enrollment, ModelID: unservedCanary, Effort: "high",
			},
			want:   domain.AdmissionUnserved{Part: domain.AgentSelectionPartModel, EnrollmentID: &enrollment},
			clause: "because enrollment " + agentTestEnrollment + " does not serve its model.",
		},
		{
			name:    "an effort the enrollment does not serve for the model",
			catalog: unservedCanary,
			selection: domain.TaskLineSelection{
				EnrollmentID: enrollment, ModelID: unservedCanary, Effort: "medium",
			},
			want:   domain.AdmissionUnserved{Part: domain.AgentSelectionPartEffort, EnrollmentID: &enrollment},
			clause: "because enrollment " + agentTestEnrollment + " does not serve its effort for that model.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAgentAdmissionFixture(t)
			f.recordCatalog(t, f.enrollment.ID, tc.catalog, "low", "high")
			f.submitLineSelection(t, domain.RoleImplementer, &tc.selection)
			_, admitted, err := f.admit(t)
			if admitted || !errors.Is(err, ErrAgentNotAdmissible) {
				t.Fatalf("admitAttempt = %t, %v; want a refusal", admitted, err)
			}
			detail := admissionRefusalDetail(err)
			if detail == nil || detail.Check != domain.AdmissionCheckAgentResolution || detail.Agent != nil ||
				detail.Unserved == nil || !reflect.DeepEqual(*detail.Unserved, tc.want) {
				t.Fatalf("hold detail = %s, want agent_resolution, no agent, and unserved %+v",
					renderDetail(detail), tc.want)
			}
			if strings.Contains(err.Error(), unservedCanary) {
				t.Errorf("refusal %q repeats the line's selection", err)
			}

			hold, notice := refusalHoldAndNotice(t, err)
			if !reflect.DeepEqual(hold.Detail, detail) {
				t.Errorf("stored detail = %s, want %s", renderDetail(hold.Detail), renderDetail(detail))
			}
			if stored := renderDetail(hold.Detail); strings.Contains(stored, unservedCanary) {
				t.Errorf("stored hold %s repeats the line's selection", stored)
			}
			if !strings.Contains(notice.Reason, tc.clause) {
				t.Errorf("notice reason %q does not contain %q", notice.Reason, tc.clause)
			}
			if strings.Contains(notice.Reason, unservedCanary) {
				t.Errorf("notice reason %q repeats the line's selection", notice.Reason)
			}
		})
	}
}

// TestOtherResolutionRefusalsNameNoPart: a resolution refusal outside the
// served-tuple rule records no part: a route model list that does not admit
// the model, and an effort no adapter can send.
func TestOtherResolutionRefusalsNameNoPart(t *testing.T) {
	t.Parallel()
	t.Run("a route model list that does not admit the model", func(t *testing.T) {
		t.Parallel()
		f := newAgentAdmissionFixture(t)
		routes := slices.Clone(f.selection.Tree.Routes)
		i := slices.IndexFunc(routes, func(r agenttree.Route) bool { return r.Name == f.enrollment.Route })
		if i < 0 {
			t.Fatalf("the tree has no route %s", f.enrollment.Route)
		}
		routes[i].Models = &domain.RouteModelList{Mode: domain.ModelListDeny, Models: []string{"claude-implementer-test"}}
		f.selection.Tree.Routes = routes
		_, err := resolveAgentAdmission(t.Context(), f.store, f.selection, f.run.ID, domain.RoleImplementer,
			agentTestPrompts[domain.RoleImplementer].Digest, agentTestAt)
		assertNoPart(t, err)
	})
	t.Run("an effort no adapter can send", func(t *testing.T) {
		t.Parallel()
		f := newAgentAdmissionFixture(t)
		f.submitLineSelection(t, domain.RoleImplementer, &domain.TaskLineSelection{
			EnrollmentID: agentTestEnrollment, ModelID: unservedCatalogModel, Effort: unservedCanary,
		})
		_, _, err := f.admit(t)
		assertNoPart(t, err)
	})
}

func assertNoPart(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrAgentNotAdmissible) {
		t.Fatalf("admission = %v, want a refusal", err)
	}
	detail := admissionRefusalDetail(err)
	if detail == nil || detail.Check != domain.AdmissionCheckAgentResolution || detail.Unserved != nil {
		t.Fatalf("hold detail = %s, want agent_resolution with no part", renderDetail(detail))
	}
}

// TestUnservedPartChangesThePaceState: a held line whose unserved part or
// enrollment changes is a changed hold, so the pacer writes it at once
// rather than leaving the old part on the Mac until the next refresh.
func TestUnservedPartChangesThePaceState(t *testing.T) {
	t.Parallel()
	held := func(id domain.ClientEnrollmentID) *domain.ClientEnrollmentID { return &id }
	parts := []*domain.AdmissionUnserved{
		nil,
		{Part: domain.AgentSelectionPartEnrollment},
		{Part: domain.AgentSelectionPartModel, EnrollmentID: held("claude-a")},
		{Part: domain.AgentSelectionPartModel, EnrollmentID: held("claude-b")},
		{Part: domain.AgentSelectionPartEffort, EnrollmentID: held("claude-a")},
	}
	seen := map[string]*domain.AdmissionUnserved{}
	for _, unserved := range parts {
		state := holdPaceState(domain.HoldAdmissionPolicyRefused, holdDetail{refusal: &domain.AdmissionRefusal{
			Check: domain.AdmissionCheckAgentResolution, Unserved: unserved,
		}})
		if prior, ok := seen[state]; ok {
			t.Errorf("unserved %+v and %+v share pace state %q", prior, unserved, state)
		}
		seen[state] = unserved
	}
}

// TestCardRefusalNamesNoPart: a refusal from an attempt's alternate-agent
// card names no part, whether the card's enrollment is gone or its catalog
// no longer serves its model, because changing the task's agent would not
// reach the attempt the card governs. A task line's same refusal does.
func TestCardRefusalNamesNoPart(t *testing.T) {
	t.Parallel()
	notHeld := enrollmentNotHeld{err: store.ErrNotFound}
	unserved := fmt.Errorf("selection: %w", domain.ErrDiscoveredModelUnserved)
	resolved := RoleAgent{Enrollment: domain.ClientEnrollment{ID: agentTestEnrollment}}
	carded := resolved
	carded.Card = &domain.AlternateAgentRetry{}
	if got := unservedPart(notHeld, RoleAgent{}); got == nil || got.Part != domain.AgentSelectionPartEnrollment {
		t.Fatalf("a line's unheld enrollment = %+v, want the enrollment part", got)
	}
	if got := unservedPart(unserved, resolved); got == nil || got.Part != domain.AgentSelectionPartModel {
		t.Fatalf("a line's unserved model = %+v, want the model part", got)
	}
	if got := unservedPart(cardRefusal{err: notHeld}, RoleAgent{}); got != nil {
		t.Errorf("a card's unheld enrollment = %+v, want no part", got)
	}
	if got := unservedPart(unserved, carded); got != nil {
		t.Errorf("a card's unserved model = %+v, want no part", got)
	}
}
