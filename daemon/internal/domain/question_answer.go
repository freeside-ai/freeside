package domain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Bounds on a question answer that chooses options. The rendered lines stay
// under MaxQuestionAnswerLinesBytes by construction (eight decisions, each
// label cut at MaxQuestionAnswerLabelBytes), and the note's own limit drops to
// MaxQuestionAnswerNoteBytes, so the rendered message always fits the 8 KiB
// MaxSpecRevisionCommentBytes every engine consumer of an answer already
// accepts (devlog/2026-11-20-1415-question-option-answers.md).
const (
	MaxQuestionAnswerNoteBytes  = 6 << 10
	MaxQuestionAnswerLabelBytes = 200
	MaxQuestionAnswerLinesBytes = 2 << 10
)

// questionAnswerEllipsis ends a label cut at MaxQuestionAnswerLabelBytes.
const questionAnswerEllipsis = "…"

// questionAnswerOtherText is the rendered choice for a decision the owner
// answers in the note instead of with an option.
const questionAnswerOtherText = "Other answer, see the note below"

// QuestionAnswer is the owner's choice for one decision of an agent_question.
// The option is named by its exact label: Decision.Validate keeps labels
// unique within a decision, and a re-asked question that reorders its options
// keeps each label, so the label is a stable id without minting one. A nil
// OptionLabel means the owner answers that decision in the note.
type QuestionAnswer struct {
	DecisionIndex int     `json:"decision_index"`
	OptionLabel   *string `json:"option_label"`
}

// validateQuestionAnswerShape checks what holds without the question: one to
// MaxDecisionsPerResult answers, each index in that range and named once, and
// each label non-empty. It is the stored-record backstop Command.Validate runs.
func validateQuestionAnswerShape(answers []QuestionAnswer) error {
	if len(answers) == 0 || len(answers) > MaxDecisionsPerResult {
		return fmt.Errorf("%d question answers, want 1 to %d: %w",
			len(answers), MaxDecisionsPerResult, ErrQuestionAnswerInvalid)
	}
	seen := make(map[int]struct{}, len(answers))
	for position, answer := range answers {
		if answer.DecisionIndex < 0 || answer.DecisionIndex >= MaxDecisionsPerResult {
			return fmt.Errorf("question_answers[%d] decision_index %d: %w",
				position, answer.DecisionIndex, ErrQuestionAnswerInvalid)
		}
		if _, duplicate := seen[answer.DecisionIndex]; duplicate {
			return fmt.Errorf("question_answers[%d] decision_index %d: %w",
				position, answer.DecisionIndex, ErrDuplicate)
		}
		seen[answer.DecisionIndex] = struct{}{}
		if answer.OptionLabel != nil && *answer.OptionLabel == "" {
			return fmt.Errorf("question_answers[%d] option_label: %w", position, ErrQuestionAnswerInvalid)
		}
	}
	return nil
}

// ValidateQuestionAnswers checks that the answers are complete and exact for
// the question they answer: one answer per decision, and every chosen label
// equal to one of that decision's option labels. The note rules are
// RenderQuestionAnswers', which runs before the question is known.
func ValidateQuestionAnswers(answers []QuestionAnswer, facts AgentQuestionFacts) error {
	if err := validateQuestionAnswerShape(answers); err != nil {
		return err
	}
	if len(answers) != len(facts.Decisions) {
		return fmt.Errorf("%d question answers for %d decisions: %w",
			len(answers), len(facts.Decisions), ErrQuestionAnswersIncomplete)
	}
	for position, answer := range answers {
		if answer.DecisionIndex >= len(facts.Decisions) {
			return fmt.Errorf("question_answers[%d] decision_index %d for %d decisions: %w",
				position, answer.DecisionIndex, len(facts.Decisions), ErrQuestionAnswerInvalid)
		}
		if answer.OptionLabel == nil {
			continue
		}
		options := facts.Decisions[answer.DecisionIndex].Options
		if !slices.ContainsFunc(options, func(option DecisionOption) bool {
			return option.Label == *answer.OptionLabel
		}) {
			return fmt.Errorf("question_answers[%d] option_label for decision %d: %w",
				position, answer.DecisionIndex, ErrQuestionAnswerOptionUnknown)
		}
	}
	return nil
}

// RenderQuestionAnswers renders the answers and the owner's note into the
// answer text every engine consumer reads: one "Decision N: <label>" line per
// answer in decision order (N counts from 1), then a blank line and the note
// when there is one. A nil label renders as a pointer to the note, so it
// requires a non-blank note. The note is at most MaxQuestionAnswerNoteBytes.
// The result is deterministic in its inputs, so a retry under the same
// command id renders the same stored message.
func RenderQuestionAnswers(answers []QuestionAnswer, note string) (string, error) {
	if err := validateQuestionAnswerShape(answers); err != nil {
		return "", err
	}
	if len(note) > MaxQuestionAnswerNoteBytes {
		return "", fmt.Errorf("question answer note is %d bytes, want at most %d: %w",
			len(note), MaxQuestionAnswerNoteBytes, ErrClaimTextTooLarge)
	}
	noteBlank := strings.TrimSpace(note) == ""
	var rendered strings.Builder
	for _, answer := range sortedQuestionAnswers(answers) {
		choice := questionAnswerOtherText
		if answer.OptionLabel != nil {
			choice = cutQuestionAnswerLabel(*answer.OptionLabel)
		} else if noteBlank {
			return "", fmt.Errorf("question answer for decision %d names no option: %w",
				answer.DecisionIndex, ErrQuestionAnswerNoteRequired)
		}
		rendered.WriteString("Decision ")
		rendered.WriteString(strconv.Itoa(answer.DecisionIndex + 1))
		rendered.WriteString(": ")
		rendered.WriteString(choice)
		rendered.WriteString("\n")
	}
	if !noteBlank {
		rendered.WriteString("\n")
		rendered.WriteString(note)
	}
	return rendered.String(), nil
}

// cutQuestionAnswerLabel keeps a label within MaxQuestionAnswerLabelBytes,
// cutting on a UTF-8 boundary and marking the cut. The typed answer keeps the
// exact label; only this human-readable line is shortened.
func cutQuestionAnswerLabel(label string) string {
	if len(label) <= MaxQuestionAnswerLabelBytes {
		return label
	}
	cut := MaxQuestionAnswerLabelBytes - len(questionAnswerEllipsis)
	for cut > 0 && !utf8.RuneStart(label[cut]) {
		cut--
	}
	return label[:cut] + questionAnswerEllipsis
}

func compareQuestionAnswers(a, b QuestionAnswer) int { return a.DecisionIndex - b.DecisionIndex }

// sortedQuestionAnswers is a detached copy in decision order.
func sortedQuestionAnswers(in []QuestionAnswer) []QuestionAnswer {
	out := cloneQuestionAnswers(in)
	slices.SortFunc(out, compareQuestionAnswers)
	return out
}

func cloneQuestionAnswers(in []QuestionAnswer) []QuestionAnswer {
	if in == nil {
		return nil
	}
	out := make([]QuestionAnswer, len(in))
	for index, answer := range in {
		out[index] = QuestionAnswer{DecisionIndex: answer.DecisionIndex, OptionLabel: clonePtr(answer.OptionLabel)}
	}
	return out
}
