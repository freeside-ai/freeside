package engine

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// specificationPreviousAnswers finds the owner's earlier answers to the
// decisions a specification question is about to ask (#3443), so the card
// opens with each one chosen for the owner to confirm. Earlier questions are
// the answered specification agent_questions of every task in the run's
// project that holds the run's submitted source, the run's own task
// included, so both a resubmission and a re-ask on the same task carry
// them. The source is the submitted work-item artifact, the same identity
// submit's duplicate-source check uses; a task with another kind of source
// carries nothing.
//
// A decision matches an earlier one with the same set of option labels, in
// any order, and otherwise one with the same question text; the newest
// earlier question decides. Its answer carries when it is an option the new
// decision still offers, or the text the owner typed. The lookup only
// reads: it never answers or retries anything, and the result is a default
// on the new item, outside its digest.
func specificationPreviousAnswers(
	ctx context.Context, tx *store.ReadTx, run domain.Run, decisions []domain.Decision,
) ([]domain.PreviousAnswer, error) {
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return nil, err
	}
	source := submittedSourceArtifact(task)
	if source == "" {
		return nil, nil
	}
	tasks, err := tx.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	sameSource := map[domain.TaskID]bool{}
	for _, row := range tasks {
		if row.Value.ProjectID == run.ProjectID && submittedSourceArtifact(row.Value) == source {
			sameSource[row.Value.ID] = true
		}
	}
	runs, err := tx.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	sourceRuns := map[domain.RunID]bool{}
	for _, row := range runs {
		if sameSource[row.Value.TaskID] {
			sourceRuns[row.Value.ID] = true
		}
	}
	items, err := tx.ListAttentionItems(ctx)
	if err != nil {
		return nil, err
	}
	var earlier []answeredQuestion
	for _, row := range items {
		item := row.Value
		if item.Type != domain.AttentionAgentQuestion || item.AgentQuestion == nil ||
			item.AgentQuestion.Stage != domain.StageNameSpecification ||
			item.Subject.RunID == nil || !sourceRuns[*item.Subject.RunID] || item.Status != domain.StatusSuperseded {
			continue
		}
		commands, err := tx.ListCommandsForItem(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		commands = slices.DeleteFunc(commands, func(command domain.Command) bool {
			return command.Action != domain.ActionAnswerAndRetry || !operatorFeedbackCommandMatchesItem(command, item)
		})
		if len(commands) != 1 {
			continue
		}
		earlier = append(earlier, answeredQuestion{item: item, answers: decisionAnswers(*item.AgentQuestion, commands[0])})
	}
	// Newest first, so the first match is the one that decides.
	// An item stored before items carried a creation time sorts as oldest.
	created := func(item domain.AttentionItem) time.Time {
		if item.CreatedAt == nil {
			return time.Time{}
		}
		return *item.CreatedAt
	}
	slices.SortFunc(earlier, func(a, b answeredQuestion) int {
		if c := created(a.item).Compare(created(b.item)); c != 0 {
			return -c
		}
		return -cmp.Compare(a.item.ID, b.item.ID)
	})
	var previous []domain.PreviousAnswer
	for index, decision := range decisions {
		answer, from, found := matchEarlierAnswer(decision, earlier)
		if !found {
			continue
		}
		carried := domain.PreviousAnswer{DecisionIndex: index, ItemID: from}
		switch {
		case answer.note != nil:
			carried.Note = answer.note
		case slices.ContainsFunc(decision.Options, func(option domain.DecisionOption) bool {
			return option.Label == *answer.label
		}):
			carried.OptionLabel = answer.label
		default:
			continue
		}
		previous = append(previous, carried)
	}
	return previous, nil
}

// submittedSourceArtifact is the work-item artifact a submitted task holds,
// or "" for a task with another kind of source.
func submittedSourceArtifact(task domain.Task) domain.ArtifactID {
	if task.Source == nil || task.Source.Kind != domain.SpecificationSourceWorkItemArtifact {
		return ""
	}
	return task.Source.WorkItemArtifactID
}

// answeredQuestion is an earlier specification question and the answer the
// owner gave each of its decisions, keyed by decision index. A decision the
// answer can't be read for is absent.
type answeredQuestion struct {
	item    domain.AttentionItem
	answers map[int]earlierAnswer
}

// earlierAnswer is one decision's answer: exactly one of an option label and
// the owner's typed text.
type earlierAnswer struct {
	label, note *string
}

// matchEarlierAnswer finds the answer for decision in the newest earlier
// question with a decision of the same option labels, and failing that in
// the newest with the same question text. When that question has several
// decisions with those labels, such as two Yes/No questions, the one with the
// same question text wins, so each finds its own answer.
func matchEarlierAnswer(decision domain.Decision, earlier []answeredQuestion) (earlierAnswer, domain.ItemID, bool) {
	sameText := func(other domain.Decision) bool { return other.Question == decision.Question }
	for _, same := range []func(domain.Decision) bool{
		func(other domain.Decision) bool { return sameOptionLabels(decision, other) },
		sameText,
	} {
		for _, question := range earlier {
			matched := -1
			for index, other := range question.item.AgentQuestion.Decisions {
				if _, ok := question.answers[index]; !ok || !same(other) {
					continue
				}
				if sameText(other) {
					matched = index
					break
				}
				if matched < 0 {
					matched = index
				}
			}
			if matched >= 0 {
				return question.answers[matched], question.item.ID, true
			}
		}
	}
	return earlierAnswer{}, "", false
}

func sameOptionLabels(a, b domain.Decision) bool {
	labels := func(d domain.Decision) []string {
		out := make([]string, len(d.Options))
		for i, option := range d.Options {
			out[i] = option.Label
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(labels(a), labels(b))
}

// decisionAnswers reads each decision's answer from the answer_and_retry
// command. With #3444's question_answers, a chosen label is the answer and a
// decision answered in the note carries the note. The note is what follows
// the rendered "Decision N:" lines; it is found by rendering those lines
// again rather than parsing them, since a label may hold a newline, and a
// message that doesn't start with them yields no note. An answer from before
// #3444 is all typed text, which can't be split across decisions, so it
// carries only for a one-decision question, and only within the bound a
// note has, since the card offers it again as the answer's note.
func decisionAnswers(question domain.AgentQuestionFacts, command domain.Command) map[int]earlierAnswer {
	answers := map[int]earlierAnswer{}
	if len(command.QuestionAnswers) == 0 {
		if len(question.Decisions) == 1 && strings.TrimSpace(command.Message) != "" &&
			len(command.Message) <= domain.MaxQuestionAnswerNoteBytes {
			message := command.Message
			answers[0] = earlierAnswer{note: &message}
		}
		return answers
	}
	note := questionAnswerNote(command)
	for _, answer := range command.QuestionAnswers {
		switch {
		case answer.OptionLabel != nil:
			label := *answer.OptionLabel
			answers[answer.DecisionIndex] = earlierAnswer{label: &label}
		case note != nil:
			answers[answer.DecisionIndex] = earlierAnswer{note: note}
		}
	}
	return answers
}

// questionAnswerNote returns the note of an answer that chose options, or nil
// when it has none or its message isn't the rendering of its answers.
func questionAnswerNote(command domain.Command) *string {
	const marker = "x"
	rendered, err := domain.RenderQuestionAnswers(command.QuestionAnswers, marker)
	if err != nil {
		return nil
	}
	note, found := strings.CutPrefix(command.Message, strings.TrimSuffix(rendered, marker))
	if !found || strings.TrimSpace(note) == "" {
		return nil
	}
	return &note
}
