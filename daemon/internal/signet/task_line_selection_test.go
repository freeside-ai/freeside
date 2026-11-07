package signet_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// testLineNames names every line the signet tests choose, each run by its
// own enrollment (seedLineEnrollments).
var testLineNames = []string{
	"codex-primary", "claude-primary", "claude-implementer", "claude-reviewer", "claude-specifier",
	"codex-implementer", "codex-reviewer", "claude-max-b", "codex-pro", "claude", "claude-b", "codex", "other",
}

// lineSelectionOf is the selection the tests send for name: its own seeded
// enrollment, which has no discovery record, so any model and effort pass.
func lineSelectionOf(name string) domain.TaskLineSelection {
	return domain.TaskLineSelection{
		EnrollmentID: domain.ClientEnrollmentID("line-" + name + "/codex_cli"), ModelID: "model-" + name, Effort: "high",
	}
}

// seedLineEnrollments records the enrollment of every testLineNames
// selection, as signet refuses a selection whose enrollment the store lacks.
func seedLineEnrollments(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := t.Context()
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		for _, name := range testLineNames {
			identity := domain.AuthIdentity{
				ID: domain.AuthIdentityID("line-" + name), Provider: "openai", AccountBinding: "acct-line-" + name,
				AuthStoreMutationLease: true, Enabled: true,
			}
			if err := tx.RecordAuthIdentity(ctx, identity, operatorAt); err != nil {
				return err
			}
			if err := tx.RecordClientEnrollment(ctx, domain.ClientEnrollment{
				ID: lineSelectionOf(name).EnrollmentID, AuthIdentityID: identity.ID,
				HarnessClient: domain.HarnessClientCodexCLI, Route: "openai_chatgpt_codex",
				AuthMethod: domain.AuthMethodOAuth, CredentialMode: domain.CredentialSubscriptionContained,
				RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
				AccountBinding: identity.AccountBinding,
			}, operatorAt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// selectionChoice is the choice a command sends, and signet records, for
// role run by name.
func selectionChoice(role domain.RoleName, name string) domain.TaskLineChoice {
	selection := lineSelectionOf(name)
	return domain.TaskLineChoice{Role: role, Selection: &selection}
}

// wireLine is a line as the wire carries it: a v2 line its selection with a
// null agent, a v1 line its agent name with a null selection.
func wireLine(line domain.TaskLine) signet.TaskLine {
	wire := signet.TaskLine{
		ID: line.ID, TaskID: line.TaskID, Role: line.Role, Version: line.Version,
		PredecessorID: line.PredecessorID, Source: line.Source, SetBy: line.SetBy, SetAt: line.SetAt,
		Provenance: line.Provenance,
	}
	if line.Selection != nil {
		wire.Selection = line.Selection
	} else {
		wire.Agent = &line.Agent
	}
	return wire
}

// TestChangeTaskLineValidatesTheSelection: a line change records the
// selection it sends and answers with it, the line carrying no agent name.
// An enrollment the store lacks, or a model or effort the enrollment's
// discovered catalog doesn't list, is refused naming the field without
// repeating the operator's text, and commits nothing. An enrollment with no
// discovery record takes any well-formed model and effort.
func TestChangeTaskLineValidatesTheSelection(t *testing.T) {
	t.Parallel()
	svc, st, id := newOperatorService(t, signet.WithTaskLineChanger(engineTaskLineChanger))
	result, err := svc.Submit(t.Context(), lineCommand("line-a", id, new("codex-primary")))
	if err != nil {
		t.Fatal(err)
	}
	line := result.Receipt.TaskLineChange.Line
	if line == nil || line.Encoding != domain.TaskLineEncodingV2 || line.Selection == nil ||
		*line.Selection != lineSelectionOf("codex-primary") {
		t.Fatalf("recorded line = %+v", line)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := json.Marshal(lineSelectionOf("codex-primary"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"selection":`+string(selection)) || !strings.Contains(string(body), `"agent":null`) {
		t.Fatalf("receipt on the wire = %s, want the selection and a null agent", body)
	}

	discovered := lineSelectionOf("claude-reviewer")
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordDiscoveredModelCatalog(t.Context(), domain.DiscoveredModelCatalog{
			EnrollmentID: discovered.EnrollmentID, HarnessBuild: "codex-cli 0.50.0", ObservedAt: operatorAt,
			Models: []domain.DiscoveredModel{{
				ID: discovered.ModelID, DisplayName: "Reviewer", EffortLevels: []string{discovered.Effort},
			}},
		})
	}); err != nil {
		t.Fatal(err)
	}
	served := lineCommand("line-served", id, nil)
	served.ChangeTaskLine.Selection = &discovered
	if _, err := svc.Submit(t.Context(), served); err != nil {
		t.Fatalf("a selection the discovered catalog serves = %v", err)
	}

	for i, tc := range []struct {
		name, field, operatorText string
		selection                 domain.TaskLineSelection
	}{
		{
			name: "an enrollment the store lacks", field: "selection.enrollment_id", operatorText: "nobody-known",
			selection: domain.TaskLineSelection{EnrollmentID: "nobody-known/codex_cli", ModelID: "model-x", Effort: "high"},
		},
		{
			name: "a model the catalog lacks", field: "selection.model_id", operatorText: "model-unlisted",
			selection: domain.TaskLineSelection{EnrollmentID: discovered.EnrollmentID, ModelID: "model-unlisted", Effort: "high"},
		},
		{
			name: "an effort the model lacks", field: "selection.effort", operatorText: "turbo",
			selection: domain.TaskLineSelection{EnrollmentID: discovered.EnrollmentID, ModelID: discovered.ModelID, Effort: "turbo"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := serverRevision(t, st)
			command := lineCommand("refused-"+strconv.Itoa(i), id, nil)
			command.ChangeTaskLine.Selection = &tc.selection
			_, err := svc.Submit(t.Context(), command)
			if !errors.Is(err, signet.ErrTaskLineSelectionUnserved) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("change = %v, want ErrTaskLineSelectionUnserved naming %s", err, tc.field)
			}
			if strings.Contains(err.Error(), tc.operatorText) {
				t.Errorf("refusal %q repeats the operator's text", err)
			}
			if serverRevision(t, st) != before {
				t.Fatal("a refused line change committed")
			}
		})
	}
}
