package engine

import (
	"errors"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// triageClient is an inference client with only the triager's site, whose
// budget allows calls calls.
func triageClient(t *testing.T, driver inference.Driver, calls int) *inference.Client {
	t.Helper()
	root := t.TempDir()
	claims, err := advisory.Open(filepath.Join(root, "claims.json"), 100, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	limits := inference.Limits{Calls: int64(calls), ComputeUnits: 100_000, AttentionItems: 10, Starvation: time.Hour}
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(root, "ledger.json"), Advisory: claims,
		Roles: inferencefake.Roles{Provider: "fake", Model: "triager", Driver: driver},
		Sites: []inference.Site{inference.TriagerSite(inference.Budget{
			Window: time.Hour, Site: limits, Project: limits, Global: limits,
			MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// addTriagerLine rebuilds the fixture's tree with the call agent and a
// triager line on it, as adoption does when it is given a judgment model.
// Call it before the other tree helpers: it starts from the baseline.
func (f *agentAdmissionFixture) addTriagerLine(t *testing.T) {
	t.Helper()
	prompts := maps.Clone(agentTestPrompts)
	prompts[domain.RoleTriager] = agentbaseline.Prompt{Name: "triager", Digest: agentTestDigest("9")}
	tree, err := agentbaseline.Tree(agentbaseline.TreeInput{
		ClaudeEnrollment: f.enrollment, Writers: agentTestWriters(), TermsBasisDate: "2026-08-01",
		PricingRevision: "pricing-2026-08", OfferNotAfter: agentTestAt.Add(30 * 24 * time.Hour),
		JudgmentModel: "claude-judgment-test", JudgmentEffort: domain.EffortLow, Prompts: prompts,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.useTree(t, tree)
}

// triageFixture is the admission fixture with an escalated specifier line, a
// triager line, and a triager client whose driver answers with output.
func triageFixture(t *testing.T, output string) (*agentAdmissionFixture, *inferencefake.Driver) {
	t.Helper()
	return triageFixtureOn(t, output, agentTestEscalatedModel)
}

// triageFixtureOn is triageFixture with the escalated counterpart on model.
func triageFixtureOn(t *testing.T, output, model string) (*agentAdmissionFixture, *inferencefake.Driver) {
	t.Helper()
	f, driver := unregisteredTriageFixtureOn(t, output, model)
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RegisterProject(t.Context(), domain.Project{ID: f.run.ProjectID, Repo: "owner/repo", RepositoryID: 1})
	}); err != nil {
		t.Fatal(err)
	}
	return f, driver
}

// unregisteredTriageFixture is triageFixture for a run whose project was
// never registered, as for a task submitted for a repository label intake
// has not seen.
func unregisteredTriageFixture(t *testing.T, output string) (*agentAdmissionFixture, *inferencefake.Driver) {
	t.Helper()
	return unregisteredTriageFixtureOn(t, output, agentTestEscalatedModel)
}

func unregisteredTriageFixtureOn(t *testing.T, output, model string) (*agentAdmissionFixture, *inferencefake.Driver) {
	t.Helper()
	f := newAgentAdmissionFixture(t)
	f.addTriagerLine(t)
	f.addEscalatedSpecifierOn(t, true, model)
	driver := inferencefake.New()
	driver.Script(inference.TriagerSiteID, inferencefake.Script{Response: inference.Response{Output: []byte(output)}})
	f.engine.inference = triageClient(t, driver, 10)
	f.engine.logger = slog.New(slog.DiscardHandler)
	return f, driver
}

// admitSpecifier runs the engine's admission of the fixture run's
// specification stage under invocation.
func (f *agentAdmissionFixture) admitSpecifier(
	t *testing.T, invocation domain.InvocationID,
) (domain.ExecutionAdmission, error) {
	t.Helper()
	message, err := domain.NewMessage("message-1", "conversation-1", domain.AuthorUser, "please specify", nil, agentTestAt)
	if err != nil {
		t.Fatal(err)
	}
	conversation, _ := domain.Conversation{ID: "conversation-1", Status: domain.ConversationIdle}.Append(message)
	binding := f.binding
	binding.conversation = conversation
	if binding.invocation, err = domain.NewAgentInvocation(invocation, nil, &conversation.ID, 1); err != nil {
		t.Fatal(err)
	}
	stage := domain.Stage{ID: specificationStageID(f.run.ID), RunID: f.run.ID, Name: specificationStageName}
	admission, admitted, err := f.engine.admitAttempt(t.Context(), binding, stage, invocation)
	if err == nil && !admitted {
		t.Fatalf("admitAttempt(%s) admitted nothing", invocation)
	}
	return admission, err
}

// triageState reads the fixture task's triage record and lines.
func (f *agentAdmissionFixture) triageState(t *testing.T) (*domain.TaskTriage, []domain.TaskLine) {
	t.Helper()
	task := f.task(t, f.run.ID)
	var (
		record *domain.TaskTriage
		lines  []domain.TaskLine
	)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		got, found, err := tx.GetTaskTriage(t.Context(), task.ID)
		if err != nil {
			return err
		}
		if found {
			record = &got
		}
		lines, err = tx.TaskLines(t.Context(), task.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record, lines
}

// bindingRuns reports whether binding was admitted on the selection the
// tree agent name reduces to: a selection admission names no agent, so the
// agent a test means is the one whose enrollment, model, and effort it ran.
func (f *agentAdmissionFixture) bindingRuns(t *testing.T, binding *domain.AdmissionAgentBinding, name string) bool {
	t.Helper()
	return binding != nil && binding.SelectionAdmitted() && *f.lineSelection(t, name) == (domain.TaskLineSelection{
		EnrollmentID: binding.EnrollmentID, ModelID: binding.SelectionModelID, Effort: binding.SelectionEffort,
	})
}

func (f *agentAdmissionFixture) agentDigest(t *testing.T, name string) domain.Digest {
	t.Helper()
	digest, err := f.selection.Tree.AgentDigest(name)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// A standard answer records the tier, writes no line, and leaves the
// specifier on its lineup agent; a reattempt never calls again.
func TestTriageStandardKeepsTheLineupSpecifier(t *testing.T) {
	t.Parallel()
	f, driver := triageFixture(t, `{"tier":"standard","reason":"A small fix."}`)
	admission, err := f.admitSpecifier(t, "inv-specification-1")
	if err != nil {
		t.Fatal(err)
	}
	record, lines := f.triageState(t)
	if record == nil || record.Outcome != domain.TriageOutcomeAnswered || record.Escalated ||
		record.Tier == nil || *record.Tier != domain.TriageTierStandard ||
		record.Reason == nil || *record.Reason != "A small fix." || !record.RecordedAt.Equal(agentTestAt) {
		t.Fatalf("triage record = %+v", record)
	}
	if len(lines) != 0 {
		t.Fatalf("lines = %+v, want none", lines)
	}
	if admission.AgentBinding.SelectionSource != domain.AgentSelectionSourceLineup ||
		!f.bindingRuns(t, admission.AgentBinding, agentbaseline.WriterAgentName(domain.RoleSpecifier)) {
		t.Fatalf("binding = %+v, want the lineup's specifier", admission.AgentBinding)
	}
	if _, err := f.admitSpecifier(t, "inv-specification-2"); err != nil {
		t.Fatal(err)
	}
	if got := len(driver.Requests()); got != 1 {
		t.Fatalf("triager calls = %d, want 1", got)
	}
}

// An escalated answer writes the specifier's triage line at the record's
// instant, and admission runs the escalated counterpart under that line.
func TestTriageEscalatedAdmitsTheCounterpart(t *testing.T) {
	t.Parallel()
	f, driver := triageFixture(t, `{"tier":"escalated","reason":"Changes the API schema."}`)
	admission, err := f.admitSpecifier(t, "inv-specification-1")
	if err != nil {
		t.Fatal(err)
	}
	record, lines := f.triageState(t)
	if record == nil || record.Outcome != domain.TriageOutcomeAnswered || !record.Escalated {
		t.Fatalf("triage record = %+v", record)
	}
	if len(lines) != 1 || lines[0].Source != domain.TaskLineSourceTriage || lines[0].Version != 1 ||
		f.lineAgent(lines[0]) != agentTestEscalatedAgent || !lines[0].SetAt.Equal(record.RecordedAt) {
		t.Fatalf("lines = %+v, want one triage line on %s", lines, agentTestEscalatedAgent)
	}
	// Escalation moves the model and never the credential: the line keeps
	// the specifier's enrollment and effort and takes the escalated model.
	specifier, err := lineupLine(f.selection.Tree, domain.RoleSpecifier)
	if err != nil {
		t.Fatal(err)
	}
	if got := lines[0].Selection; got == nil || string(got.EnrollmentID) != specifier.Names.Enrollment ||
		got.ModelID != agentTestEscalatedModel || got.Effort != specifier.Names.Effort {
		t.Fatalf("triage line selection = %+v, want %s on the escalated model", got, specifier.Names.Enrollment)
	}
	if admission.AgentBinding.SelectionRecordID != lines[0].ID ||
		!f.bindingRuns(t, admission.AgentBinding, agentTestEscalatedAgent) {
		t.Fatalf("binding = %+v, want the triage line on the counterpart", admission.AgentBinding)
	}
	// The fixture task has no source to read: the call names the project.
	if fields := driver.Requests()[0].Fields; fields["project_name"] != "owner/repo" || fields["issue_title"] != "" {
		t.Fatalf("fields = %v", fields)
	}
	if _, err := f.admitSpecifier(t, "inv-specification-2"); err != nil {
		t.Fatal(err)
	}
	if got := len(driver.Requests()); got != 1 {
		t.Fatalf("triager calls = %d, want 1", got)
	}
}

// An escalated specifier on the implementer's model and effort is one
// definition under two names, as the baseline tree writes it when settings
// pick that pair. The triage line's selection is the implementer's too, and
// it still runs the counterpart.
func TestTriageEscalatesToTheImplementersSelection(t *testing.T) {
	t.Parallel()
	f, _ := triageFixtureOn(t, `{"tier":"escalated","reason":"Changes the API schema."}`, "claude-implementer-test")
	if f.agentDigest(t, agentTestEscalatedAgent) != f.agentDigest(t, agentTestWriterAgent) {
		t.Fatal("the counterpart is not the implementer's definition")
	}
	admission, err := f.admitSpecifier(t, "inv-specification-1")
	if err != nil {
		t.Fatal(err)
	}
	_, lines := f.triageState(t)
	if len(lines) != 1 || lines[0].Source != domain.TaskLineSourceTriage ||
		*lines[0].Selection != *f.lineSelection(t, agentTestWriterAgent) {
		t.Fatalf("lines = %+v, want one triage line on the implementer's selection", lines)
	}
	if admission.AgentBinding.SelectionRecordID != lines[0].ID ||
		!f.bindingRuns(t, admission.AgentBinding, agentTestEscalatedAgent) {
		t.Fatalf("binding = %+v, want the triage line on the counterpart", admission.AgentBinding)
	}
}

// A run whose project was never registered still triages: the call names no
// project, and the answer decides as for any other task.
func TestTriageUnregisteredProject(t *testing.T) {
	t.Parallel()
	f, driver := unregisteredTriageFixture(t, `{"tier":"escalated","reason":"Changes the API schema."}`)
	admission, err := f.admitSpecifier(t, "inv-specification-1")
	if err != nil {
		t.Fatal(err)
	}
	record, lines := f.triageState(t)
	if record == nil || record.Outcome != domain.TriageOutcomeAnswered || !record.Escalated || len(lines) != 1 ||
		admission.AgentBinding.SelectionRecordID != lines[0].ID {
		t.Fatalf("triage record = %+v, lines = %+v, binding = %+v", record, lines, admission.AgentBinding)
	}
	if fields := driver.Requests()[0].Fields; fields["project_name"] != "" {
		t.Fatalf("project_name = %q, want empty", fields["project_name"])
	}
}

// Every way the triager fails to answer escalates, with its own outcome, so
// an unavailable triager never puts a task on the smaller model.
func TestTriageFailuresEscalate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		setup   func(*testing.T, *agentAdmissionFixture, *inferencefake.Driver)
		outcome domain.TriageOutcome
		reason  string // empty: no reason
		calls   int
	}{
		{
			name: "driver error",
			setup: func(_ *testing.T, _ *agentAdmissionFixture, d *inferencefake.Driver) {
				d.Script(inference.TriagerSiteID, inferencefake.Script{Err: errors.New("unavailable")})
			},
			outcome: domain.TriageOutcomeCallFailed, reason: string(inference.FallbackDriverFailed), calls: 1,
		},
		{
			name: "invalid answer",
			setup: func(_ *testing.T, _ *agentAdmissionFixture, d *inferencefake.Driver) {
				d.Script(inference.TriagerSiteID, inferencefake.Script{Response: inference.Response{
					Output: []byte(`{"tier":"large","reason":"Big."}`),
				}})
			},
			outcome: domain.TriageOutcomeInvalidAnswer, reason: string(inference.FallbackOutputSchemaRejected), calls: 1,
		},
		{
			name: "budget spent",
			// The budget allows one call, which another task spent.
			setup: func(t *testing.T, f *agentAdmissionFixture, d *inferencefake.Driver) {
				f.engine.inference = triageClient(t, d, 1)
				if _, reason, err := f.engine.inference.TriageSpecifier(t.Context(), inference.TriagerInput{
					Project: string(f.run.ProjectID), RootLineage: "run-other", SourceText: "Other",
				}); err != nil || reason != "" {
					t.Fatalf("spend the budget: %q, %v", reason, err)
				}
			},
			outcome: domain.TriageOutcomeBudgetSpent, reason: string(inference.FallbackBudgetExhausted), calls: 1,
		},
		{
			name: "no triager site",
			setup: func(_ *testing.T, f *agentAdmissionFixture, _ *inferencefake.Driver) {
				f.engine.inference = nil
			},
			outcome: domain.TriageOutcomeNoTriagerLine, reason: string(inference.FallbackRoleSourceAbsent),
		},
		{
			name: "no triager line",
			setup: func(t *testing.T, f *agentAdmissionFixture, _ *inferencefake.Driver) {
				f.useTree(t, withoutLineupLine(t, f.selection.Tree, string(domain.RoleTriager)))
			},
			outcome: domain.TriageOutcomeNoTriagerLine,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, driver := triageFixture(t, `{"tier":"standard","reason":"A small fix."}`)
			tc.setup(t, f, driver)
			admission, err := f.admitSpecifier(t, "inv-specification-1")
			if err != nil {
				t.Fatal(err)
			}
			record, lines := f.triageState(t)
			if record == nil || record.Outcome != tc.outcome || !record.Escalated || record.Tier != nil {
				t.Fatalf("triage record = %+v, want escalated %s", record, tc.outcome)
			}
			if (record.Reason == nil) != (tc.reason == "") || (record.Reason != nil && *record.Reason != tc.reason) {
				t.Fatalf("triage reason = %v, want %q", record.Reason, tc.reason)
			}
			if len(lines) != 1 || f.lineAgent(lines[0]) != agentTestEscalatedAgent ||
				admission.AgentBinding.SelectionRecordID != lines[0].ID {
				t.Fatalf("lines = %+v, binding = %+v; want the triage line", lines, admission.AgentBinding)
			}
			if got := len(driver.Requests()); got != tc.calls {
				t.Fatalf("triager calls = %d, want %d", got, tc.calls)
			}
		})
	}
}

// Triage applies only to a task the lineup can escalate whose specifier has
// neither an operator's line nor an admitted attempt: otherwise there is no
// call and no record.
func TestTriageSkips(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *agentAdmissionFixture)
	}{
		{
			name: "operator line",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.submitLine(t, domain.RoleSpecifier, agentTestEscalatedAgent)
			},
		},
		{
			name: "cleared operator line",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.submitLine(t, domain.RoleSpecifier, agentTestEscalatedAgent)
				f.changeLine(t, domain.RoleSpecifier, "", true)
			},
		},
		{
			name: "no escalated line",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.useTree(t, withoutLineupLine(t, f.selection.Tree, escalatedSpecifierKey(t)))
			},
		},
		{
			name: "admitted specifier",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.recordSpecificationAdmission(t)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, driver := triageFixture(t, `{"tier":"escalated","reason":"Changes the API schema."}`)
			tc.setup(t, f)
			if _, err := f.admitSpecifier(t, "inv-specification-1"); err != nil {
				t.Fatal(err)
			}
			if record, _ := f.triageState(t); record != nil {
				t.Fatalf("triage record = %+v, want none", record)
			}
			if got := len(driver.Requests()); got != 0 {
				t.Fatalf("triager calls = %d, want 0", got)
			}
		})
	}
}

// An operator's change after triage supersedes the triage line: admission
// runs the agent the change names.
func TestTaskLineChangeSupersedesTriage(t *testing.T) {
	t.Parallel()
	f, _ := triageFixture(t, `{"tier":"escalated","reason":"Changes the API schema."}`)
	if _, err := f.admitSpecifier(t, "inv-specification-1"); err != nil {
		t.Fatal(err)
	}
	specifier := agentbaseline.WriterAgentName(domain.RoleSpecifier)
	line := f.changeLine(t, domain.RoleSpecifier, specifier, false).Line
	admission, err := f.admitSpecifier(t, "inv-specification-2")
	if err != nil {
		t.Fatal(err)
	}
	if admission.AgentBinding.SelectionRecordID != line.ID ||
		!f.bindingRuns(t, admission.AgentBinding, specifier) {
		t.Fatalf("binding = %+v, want the operator's line", admission.AgentBinding)
	}
}

// recordSpecificationAdmission admits and records the fixture run's
// specifier on a tree with no escalated line, then restores the tree: a task
// whose specifier ran before triage could apply to it.
func (f *agentAdmissionFixture) recordSpecificationAdmission(t *testing.T) {
	t.Helper()
	tree := f.selection.Tree
	f.useTree(t, withoutLineupLine(t, tree, escalatedSpecifierKey(t)))
	admission, err := f.admitSpecifier(t, "inv-specification-0")
	if err != nil {
		t.Fatal(err)
	}
	f.useTree(t, tree)
	run := f.run
	stage := domain.Stage{ID: specificationStageID(run.ID), RunID: run.ID, Name: specificationStageName}
	stage.Attempts = []domain.Attempt{{
		ID: admission.AttemptID, StageID: stage.ID, Number: 1, InvocationID: admission.InvocationID,
	}}
	run.Stages = []domain.Stage{stage}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), run); err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(t.Context(), admission)
	}); err != nil {
		t.Fatal(err)
	}
}

// escalatedSpecifierKey is the tree's key of the specifier agent's escalated
// line.
func escalatedSpecifierKey(t *testing.T) string {
	t.Helper()
	key, err := domain.LineupEscalatedKey(domain.RoleSpecifier, agentTestEnrollment)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(key, domain.LineupRoleKeyPrefix)
}

// withoutLineupLine returns tree without its lineup line under key.
func withoutLineupLine(t *testing.T, tree agenttree.Tree, key string) agenttree.Tree {
	t.Helper()
	i := slices.IndexFunc(tree.Lineup, func(l agenttree.LineupLine) bool { return l.Key == key })
	if i < 0 {
		t.Fatalf("the tree has no %s line", key)
	}
	tree.Lineup = slices.Delete(slices.Clone(tree.Lineup), i, i+1)
	return tree
}
