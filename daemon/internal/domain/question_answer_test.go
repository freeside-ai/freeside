package domain_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestRenderQuestionAnswersFitsTheAnswerBound pins the size argument the
// render rests on: the largest answer the question and the note allow (eight
// decisions, each choosing a 4 KiB label, and a 6 KiB note) still fits the
// 8 KiB bound every engine consumer of an answer accepts. The labels are
// multi-byte so the cut must land on a UTF-8 boundary.
func TestRenderQuestionAnswersFitsTheAnswerBound(t *testing.T) {
	t.Parallel()
	label := strings.Repeat("é", domain.MaxDecisionTextBytes/len("é"))
	answers := make([]domain.QuestionAnswer, domain.MaxDecisionsPerResult)
	for index := range answers {
		answers[index] = domain.QuestionAnswer{DecisionIndex: index, OptionLabel: &label}
	}
	note := strings.Repeat("n", domain.MaxQuestionAnswerNoteBytes)
	rendered, err := domain.RenderQuestionAnswers(answers, note)
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered) > domain.MaxSpecRevisionCommentBytes || !utf8.ValidString(rendered) {
		t.Fatalf("rendered answer is %d bytes, valid UTF-8 %t; want at most %d and valid",
			len(rendered), utf8.ValidString(rendered), domain.MaxSpecRevisionCommentBytes)
	}
	lines, _, _ := strings.Cut(rendered, "\n\n")
	if len(lines) > domain.MaxQuestionAnswerLinesBytes {
		t.Fatalf("rendered lines are %d bytes, want at most %d", len(lines), domain.MaxQuestionAnswerLinesBytes)
	}
	if !strings.HasSuffix(strings.SplitN(lines, "\n", 2)[0], "é…") {
		t.Fatalf("first line %q does not end in a cut label", strings.SplitN(lines, "\n", 2)[0])
	}
}

// TestRenderQuestionAnswersWithoutANote: answers that all choose an option
// may go without a note, and render as their lines alone in decision order;
// a whitespace-only note counts as none. The app renders the same bytes to
// trust the record (app/Sources/FreesideAPI/QuestionAnswers.swift).
func TestRenderQuestionAnswersWithoutANote(t *testing.T) {
	t.Parallel()
	second, first := "Postgres", "Current only"
	answers := []domain.QuestionAnswer{{DecisionIndex: 1, OptionLabel: &second}, {DecisionIndex: 0, OptionLabel: &first}}
	rendered, err := domain.RenderQuestionAnswers(answers, " \n")
	if want := "Decision 1: Current only\nDecision 2: Postgres\n"; err != nil || rendered != want {
		t.Fatalf("RenderQuestionAnswers = %q, %v; want %q", rendered, err, want)
	}
}
