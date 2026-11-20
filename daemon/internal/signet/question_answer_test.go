package signet_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// twoDecisionQuestionItem is an open specification-stage agent_question with
// two decisions, so a test can answer one and leave the other.
func twoDecisionQuestionItem(t *testing.T) domain.AttentionItem {
	t.Helper()
	runID := domain.RunID("run-1")
	second := domain.Decision{
		Question:    "Which store backs the cache?",
		WhyBlocking: "The specification cannot name the migration without it.",
		Options: []domain.DecisionOption{
			{Label: "SQLite", Tradeoffs: "One file, no server."},
			{Label: "Postgres", Tradeoffs: "Shared, needs a server."},
		},
		Recommendation: "SQLite",
	}
	facts := &domain.AgentQuestionFacts{
		Stage:        domain.StageNameSpecification,
		InvocationID: domain.SpecificationInvocationID(runID, 7),
		Decisions:    append(decisionsFixture(), second),
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID("question-" + facts.InvocationID), ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionAgentQuestion, Priority: domain.PriorityNormal,
		Reason:            "the specifier needs an owner decision",
		RequestedDecision: []domain.Action{domain.ActionAnswerAndRetry, domain.ActionStop},
		AgentClaims:       []domain.AgentClaim{questionClaimFixture(facts)},
		AgentQuestion:     facts,
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func optionAnswer(index int, label string) domain.QuestionAnswer {
	return domain.QuestionAnswer{DecisionIndex: index, OptionLabel: &label}
}

// TestSubmitQuestionAnswersPolicy: question_answers are accepted only on an
// answer to an agent_question, only as a complete and exact set of that
// question's options, and a decision answered in the note needs a note. An
// accepted answer records the typed answers and renders them into the
// message, and a retry under the same command id replays the stored record.
func TestSubmitQuestionAnswersPolicy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	question := twoDecisionQuestionItem(t)
	if err := putTestItem(t, f, question); err != nil {
		t.Fatal(err)
	}
	complete := []domain.QuestionAnswer{optionAnswer(0, "Current only"), optionAnswer(1, "SQLite")}
	submit := func(commandID string, item domain.AttentionItem, action domain.Action,
		answers []domain.QuestionAnswer, note string,
	) (signet.CommandResult, error) {
		t.Helper()
		snapshot, err := f.service.GetAttentionItem(ctx, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		command := f.command(commandID, action)
		command.Payload.ItemID = item.ID
		command.Payload.ItemVersion = snapshot.Item.ItemVersion
		command.Payload.PRHeadSHA = snapshot.Item.PRHeadSHA
		command.Payload.ArtifactDigests = snapshot.Item.ArtifactDigests
		command.Payload.QuestionAnswers = answers
		command.Payload.Message = note
		command.ExpectedEntityVersion = snapshot.EntityVersion
		return f.service.Submit(ctx, command)
	}

	refusals := []struct {
		name    string
		item    domain.AttentionItem
		action  domain.Action
		answers []domain.QuestionAnswer
		note    string
		want    error
	}{
		{"empty set", question, domain.ActionAnswerAndRetry, []domain.QuestionAnswer{}, "", domain.ErrQuestionAnswerInvalid},
		{"incomplete set", question, domain.ActionAnswerAndRetry, complete[:1], "", domain.ErrQuestionAnswersIncomplete},
		{
			"duplicate index", question, domain.ActionAnswerAndRetry,
			[]domain.QuestionAnswer{optionAnswer(0, "Current only"), optionAnswer(0, "Current and previous")},
			"", domain.ErrDuplicate,
		},
		{
			"index past the decisions", question, domain.ActionAnswerAndRetry,
			[]domain.QuestionAnswer{optionAnswer(0, "Current only"), optionAnswer(2, "SQLite")},
			"", domain.ErrQuestionAnswerInvalid,
		},
		{
			"negative index", question, domain.ActionAnswerAndRetry,
			[]domain.QuestionAnswer{optionAnswer(-1, "Current only"), optionAnswer(1, "SQLite")},
			"", domain.ErrQuestionAnswerInvalid,
		},
		{
			"label of another decision", question, domain.ActionAnswerAndRetry,
			[]domain.QuestionAnswer{optionAnswer(0, "SQLite"), optionAnswer(1, "SQLite")},
			"", domain.ErrQuestionAnswerOptionUnknown,
		},
		{
			"other answer without a note", question, domain.ActionAnswerAndRetry,
			[]domain.QuestionAnswer{optionAnswer(0, "Current only"), {DecisionIndex: 1}},
			" \n", domain.ErrQuestionAnswerNoteRequired,
		},
		{
			"note over the answer limit", question, domain.ActionAnswerAndRetry, complete,
			strings.Repeat("n", domain.MaxQuestionAnswerNoteBytes+1), domain.ErrClaimTextTooLarge,
		},
		{"stop", question, domain.ActionStop, complete, "", signet.ErrContentNotAllowed},
		{"another item type", f.item, domain.ActionAnswerAndRetry, complete, "", signet.ErrContentNotAllowed},
	}
	for index, test := range refusals {
		t.Run(test.name, func(t *testing.T) {
			if _, err := submit(fmt.Sprintf("cmd-answers-refused-%d", index),
				test.item, test.action, test.answers, test.note); !errors.Is(err, test.want) {
				t.Fatalf("Submit = %v, want %v", err, test.want)
			}
		})
	}

	// Listed out of decision order: the record keeps them in decision order.
	answers := []domain.QuestionAnswer{{DecisionIndex: 1}, optionAnswer(0, "Current only")}
	const note = "Use Redis; the cache is shared across hosts."
	result, err := submit("cmd-answers", question, domain.ActionAnswerAndRetry, answers, note)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	wantMessage := "Decision 1: Current only\nDecision 2: Other answer, see the note below\n\n" + note
	wantAnswers := []domain.QuestionAnswer{answers[1], answers[0]}
	if result.Record.Message != wantMessage || !reflect.DeepEqual(result.Record.QuestionAnswers, wantAnswers) {
		t.Fatalf("record message %q answers %#v, want %q %#v",
			result.Record.Message, result.Record.QuestionAnswers, wantMessage, wantAnswers)
	}

	// The answer superseded the item, so the replay must come from the
	// stored record rather than a fresh acceptance.
	command := f.command("cmd-answers", domain.ActionAnswerAndRetry)
	command.Payload.ItemID = question.ID
	command.Payload.PRHeadSHA = result.Record.PRHeadSHA
	command.Payload.ArtifactDigests = result.Record.ArtifactDigests
	command.Payload.QuestionAnswers = answers
	command.Payload.Message = note
	replay, err := f.service.Submit(ctx, command)
	if err != nil || !reflect.DeepEqual(replay, result) {
		t.Fatalf("replay = %#v, %v; want %#v", replay, err, result)
	}
	command.Payload.QuestionAnswers = complete
	command.Payload.Message = ""
	if _, err := f.service.Submit(ctx, command); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("changed answers under the same id = %v, want %v", err, store.ErrImmutableConflict)
	}
}
