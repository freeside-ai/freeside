package signet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrTaskLineSelectionUnserved refuses a client's task line selection whose
// enrollment the daemon lacks, or whose model or effort that enrollment's
// discovered catalog lacks. Its error names the refused field and never the
// operator's text.
var ErrTaskLineSelectionUnserved = errors.New("task line selection names what the daemon does not serve")

// validateLineSelection checks a client's selection against the store: the
// enrollment must exist, and when the daemon has recorded the enrollment's
// discovered catalog, the catalog must serve the model at the effort
// (domain.DiscoveredModelCatalog.Serves, the check admission makes). With no
// catalog recorded the shape alone passes, and admission decides at the
// admitting revision, as it always re-checks. field is the selection's wire
// path, such as task_lines[0].selection; the error names field and its leg
// only. The reserved native-default model id names no catalog model, so
// admission alone judges it.
func validateLineSelection(ctx context.Context, tx *store.ReadTx, selection domain.TaskLineSelection, field string) error {
	if _, err := tx.GetClientEnrollment(ctx, selection.EnrollmentID); errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s.enrollment_id: no enrollment the daemon holds: %w", field, ErrTaskLineSelectionUnserved)
	} else if err != nil {
		return err
	}
	if selection.ModelID == domain.ClaudeCodeNativeDefaultRouteModelID {
		return nil
	}
	catalog, err := tx.GetDiscoveredModelCatalog(ctx, selection.EnrollmentID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch err := catalog.Serves(selection.ModelID, selection.Effort); {
	case errors.Is(err, domain.ErrDiscoveredModelUnserved):
		return fmt.Errorf("%s.model_id: not a model the enrollment's discovered catalog lists: %w",
			field, ErrTaskLineSelectionUnserved)
	case err != nil:
		return fmt.Errorf("%s.effort: not an effort the model's discovered catalog entry lists: %w",
			field, ErrTaskLineSelectionUnserved)
	}
	return nil
}

// validateLineSelections checks a command's task lines, in the order the
// client sent them, so the refused field's index is the client's own.
func validateLineSelections(ctx context.Context, tx *store.ReadTx, lines []domain.TaskLineChoice) error {
	for i, line := range lines {
		if err := validateLineSelection(ctx, tx, *line.Selection, fmt.Sprintf("task_lines[%d].selection", i)); err != nil {
			return err
		}
	}
	return nil
}

// TaskLine is the API's TaskLine: one version of a task's line as a client
// reads it. It carries the line's own encoding: a v2 line its Selection with
// a nil Agent, and a v1 line, which only records from before #2597 hold, its
// Agent name with a nil Selection, so no projection reads the tree. A
// cleared version never reaches the wire.
type TaskLine struct {
	ID            domain.Digest              `json:"id"`
	TaskID        domain.TaskID              `json:"task_id"`
	Role          domain.RoleName            `json:"role"`
	Agent         *string                    `json:"agent"`
	Selection     *domain.TaskLineSelection  `json:"selection"`
	Version       int                        `json:"version"`
	PredecessorID *domain.Digest             `json:"predecessor_id"`
	Source        domain.TaskLineSource      `json:"source"`
	SetBy         string                     `json:"set_by"`
	SetAt         time.Time                  `json:"set_at"`
	Provenance    *domain.TaskLineProvenance `json:"provenance"`
}

// TaskLineChoice is the API's TaskLineChoice: a role and what a recorded
// choice holds, exactly one of a v1 name and a selection.
type TaskLineChoice struct {
	Role      domain.RoleName           `json:"role"`
	Agent     *string                   `json:"agent"`
	Selection *domain.TaskLineSelection `json:"selection"`
}

// choiceEncoding splits a recorded choice into the wire's pair: the name a
// v1 choice holds, or the selection a v2 one holds. A cleared line's choice
// holds neither and yields neither.
func choiceEncoding(choice domain.TaskLineChoice) (*string, *domain.TaskLineSelection) {
	if choice.Selection != nil {
		selection := *choice.Selection
		return nil, &selection
	}
	if choice.Agent == "" {
		return nil, nil
	}
	agent := choice.Agent
	return &agent, nil
}

// wireTaskLine projects an uncleared line.
func wireTaskLine(line domain.TaskLine) TaskLine {
	agent, selection := choiceEncoding(line.Choice())
	return TaskLine{
		ID: line.ID, TaskID: line.TaskID, Role: line.Role, Agent: agent, Selection: selection,
		Version: line.Version, PredecessorID: line.PredecessorID, Source: line.Source, SetBy: line.SetBy,
		SetAt: line.SetAt, Provenance: line.Provenance,
	}
}

// wireTaskLines projects lines that are never cleared, such as a move's.
func wireTaskLines(lines []domain.TaskLine) []TaskLine {
	if lines == nil {
		return nil
	}
	wire := make([]TaskLine, 0, len(lines))
	for _, line := range lines {
		wire = append(wire, wireTaskLine(line))
	}
	return wire
}

// wireTaskLineChoices projects a record's choices.
func wireTaskLineChoices(choices []domain.TaskLineChoice) []TaskLineChoice {
	if choices == nil {
		return nil
	}
	wire := make([]TaskLineChoice, 0, len(choices))
	for _, choice := range choices {
		agent, selection := choiceEncoding(choice)
		wire = append(wire, TaskLineChoice{Role: choice.Role, Agent: agent, Selection: selection})
	}
	return wire
}
