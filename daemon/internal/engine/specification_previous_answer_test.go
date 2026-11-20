package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// previousAnswerRun asks and answers specification questions on one run.
type previousAnswerRun struct {
	f      specificationFixture
	driver *execfake.StageDriver
	engine *Engine
	run    domain.RunID
}

func newPreviousAnswerRun(t *testing.T, f specificationFixture) previousAnswerRun {
	t.Helper()
	driver := f.newDriver(t)
	return previousAnswerRun{f: f, driver: driver, engine: f.newEngine(t, driver), run: "specification-run"}
}

// ask scripts iteration n to ask decisions, runs it, and returns the
// question item it created. The clock moves first, so each ask is newer.
func (r previousAnswerRun) ask(t *testing.T, n int, decisions []domain.Decision) domain.AttentionItem {
	t.Helper()
	*r.f.now = r.f.now.Add(time.Minute)
	id := specificationInvocationID(r.run, n)
	if err := specifyfake.Script(r.driver, id, 0, 0, specify.Output{Decisions: decisions}); err != nil {
		t.Fatal(err)
	}
	if result, err := r.engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("ask %d: reconcile = %+v, %v", n, result, err)
	}
	item, _ := r.f.item(t, domain.ItemID("question-"+string(id)))
	return item
}

// answer submits answer_and_retry on iteration n's question with the chosen
// options and the note, then consumes it.
func (r previousAnswerRun) answer(t *testing.T, n int, answers []domain.QuestionAnswer, note string) {
	t.Helper()
	snapshot, err := r.f.signet.GetAttentionItem(t.Context(),
		domain.ItemID("question-"+string(specificationInvocationID(r.run, n))))
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Item
	if _, err := r.f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: fmt.Sprintf("answer-%s-%d", r.run, n), DeviceID: "device-1",
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionAnswerAndRetry,
			ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests,
			QuestionAnswers: answers, Message: note,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if created, err := r.engine.reconcileOperatorFeedback(t.Context()); err != nil || created != 1 {
		t.Fatalf("answer %d: reconcileOperatorFeedback = %d, %v", n, created, err)
	}
}

func choose(index int, label string) domain.QuestionAnswer {
	return domain.QuestionAnswer{DecisionIndex: index, OptionLabel: &label}
}

func inNote(index int) domain.QuestionAnswer {
	return domain.QuestionAnswer{DecisionIndex: index}
}

func decision(question string, labels ...string) domain.Decision {
	options := make([]domain.DecisionOption, len(labels))
	for i, label := range labels {
		options[i] = domain.DecisionOption{Label: label, Tradeoffs: "Tradeoffs of " + label + "."}
	}
	return domain.Decision{
		Question: question, WhyBlocking: "The specification depends on it.",
		Options: options, Recommendation: labels[0],
	}
}

func requirePreviousAnswers(t *testing.T, item domain.AttentionItem, want []domain.PreviousAnswer) {
	t.Helper()
	if item.AgentQuestion == nil {
		t.Fatalf("item %s carries no question", item.ID)
	}
	if got := item.AgentQuestion.PreviousAnswers; !reflect.DeepEqual(got, want) {
		t.Fatalf("previous answers on %s = %+v, want %+v", item.ID, got, want)
	}
}

// TestSpecificationReaskCarriesEarlierAnswers pins #3443's matching on one
// task: a decision whose options came back reordered keeps its chosen label
// (AC 6), one matched by its question text keeps its typed note, a chosen
// label the new decision no longer offers carries nothing, and the newest
// earlier answer wins.
func TestSpecificationReaskCarriesEarlierAnswers(t *testing.T) {
	t.Parallel()
	f := newSpecificationFixture(t, false, 6)
	f.submit(t)
	r := newPreviousAnswerRun(t, f)
	// A label may hold a newline; the note is still found after it.
	const both = "Keep both\nversions"
	first := r.ask(t, 1, []domain.Decision{
		decision("Which compatibility target applies?", both, "Current only"),
		decision("How long are logs kept?", "30 days", "1 year"),
		decision("Which region hosts it?", "EU", "US"),
	})
	requirePreviousAnswers(t, first, nil)
	const note = "Ninety days; legal asked."
	r.answer(t, 1, []domain.QuestionAnswer{choose(0, both), inNote(1), choose(2, "EU")}, note)

	second := r.ask(t, 2, []domain.Decision{
		decision("Which API versions do we support?", "Current only", both),
		decision("How long are logs kept?", "7 days", "90 days"),
		decision("Which region hosts it?", "APAC", "US"),
		decision("Who owns the rollout?", "Platform", "Product"),
	})
	requirePreviousAnswers(t, second, []domain.PreviousAnswer{
		{DecisionIndex: 0, OptionLabel: new(both), ItemID: first.ID},
		{DecisionIndex: 1, Note: new(note), ItemID: first.ID},
	})
	// The previous answers are a default outside the question's digest.
	digest, err := second.AgentQuestion.ComputeDigest()
	if err != nil || second.AgentClaims[0].Digest != digest {
		t.Fatalf("question claim digest = %s, want %s (%v)", second.AgentClaims[0].Digest, digest, err)
	}
	// A replayed pass writes no second version of the item.
	_, before := f.item(t, second.ID)
	if result, err := r.engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
		t.Fatalf("replayed reconcile = %+v, %v", result, err)
	}
	if _, after := f.item(t, second.ID); after.EntityVersion != before.EntityVersion {
		t.Fatalf("replay advanced the question to version %d", after.EntityVersion)
	}

	r.answer(t, 2, []domain.QuestionAnswer{
		choose(0, "Current only"), choose(1, "90 days"),
		choose(2, "US"), choose(3, "Platform"),
	}, "")
	third := r.ask(t, 3, []domain.Decision{
		decision("Which compatibility target applies?", both, "Current only"),
		decision("How long are logs kept?", "30 days", "1 year"),
	})
	requirePreviousAnswers(t, third, []domain.PreviousAnswer{
		{DecisionIndex: 0, OptionLabel: new("Current only"), ItemID: second.ID},
		// The second ask's decision 1 has other options, so this one
		// matches the first ask's by its labels, ahead of the newer
		// question-text match.
		{DecisionIndex: 1, Note: new(note), ItemID: first.ID},
	})
}

// TestSpecificationReaskMatchesSharedLabelsByText pins that two decisions
// offering the same labels, such as two Yes/No questions, each keep their
// own answer when they come back in another order.
func TestSpecificationReaskMatchesSharedLabelsByText(t *testing.T) {
	t.Parallel()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	r := newPreviousAnswerRun(t, f)
	first := r.ask(t, 1, []domain.Decision{
		decision("Should the export keep deleted rows?", "Yes", "No"),
		decision("Should the export run nightly?", "Yes", "No"),
	})
	r.answer(t, 1, []domain.QuestionAnswer{choose(0, "Yes"), choose(1, "No")}, "")
	requirePreviousAnswers(t, r.ask(t, 2, []domain.Decision{
		decision("Should the export run nightly?", "No", "Yes"),
		decision("Should the export keep deleted rows?", "No", "Yes"),
	}), []domain.PreviousAnswer{
		{DecisionIndex: 0, OptionLabel: new("No"), ItemID: first.ID},
		{DecisionIndex: 1, OptionLabel: new("Yes"), ItemID: first.ID},
	})
}

// TestSpecificationTypedAnswerCarriesOnlyForOneDecision pins how an answer
// from before #3444's picker carries: it is all typed text, which belongs to
// a question's only decision and can't be split across several, and which
// carries only within the bound of the note the card offers it again as.
func TestSpecificationTypedAnswerCarriesOnlyForOneDecision(t *testing.T) {
	t.Parallel()
	const typed = "Target the current and immediately previous API versions."
	oneDecision := []domain.Decision{decision("Which compatibility target applies?", "Current and previous", "Current only")}
	for _, tc := range []struct {
		name      string
		typed     string
		decisions []domain.Decision
		want      []domain.PreviousAnswer
	}{
		{
			name:      "one decision",
			typed:     typed,
			decisions: oneDecision,
			want:      []domain.PreviousAnswer{{DecisionIndex: 0, Note: new(typed)}},
		},
		{
			name:  "two decisions",
			typed: typed,
			decisions: []domain.Decision{
				decision("Which compatibility target applies?", "Current and previous", "Current only"),
				decision("How long are logs kept?", "30 days", "1 year"),
			},
		},
		{
			name:      "over the note bound",
			typed:     strings.Repeat("x", domain.MaxQuestionAnswerNoteBytes+1),
			decisions: oneDecision,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSpecificationFixture(t, false, 4)
			f.submit(t)
			r := newPreviousAnswerRun(t, f)
			first := r.ask(t, 1, tc.decisions)
			r.answer(t, 1, nil, tc.typed)
			want := tc.want
			for i := range want {
				want[i].ItemID = first.ID
			}
			requirePreviousAnswers(t, r.ask(t, 2, tc.decisions), want)
		})
	}
}

// TestSpecificationResubmissionCarriesEarlierAnswers pins that answers carry
// from another task on the same project and source, as after submit
// --replace, and from no task on another project or source.
func TestSpecificationResubmissionCarriesEarlierAnswers(t *testing.T) {
	t.Parallel()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	r := newPreviousAnswerRun(t, f)
	decisions := []domain.Decision{decision("Which compatibility target applies?", "Current and previous", "Current only")}
	first := r.ask(t, 1, decisions)
	r.answer(t, 1, []domain.QuestionAnswer{choose(0, "Current only")}, "")
	// The answer queues the first task's next iteration; it asks again, so
	// nothing else is pending when the other runs start.
	carried := []domain.PreviousAnswer{{DecisionIndex: 0, OptionLabel: new("Current only"), ItemID: first.ID}}
	requirePreviousAnswers(t, r.ask(t, 2, decisions), carried)

	otherBody := []byte("Specify a different work item.")
	other := testSpecificationArtifact(t, "other-work-item", domain.ArtifactKindSpecification,
		domain.Digest(contentaddr.Sum(otherBody)), domain.ProducerAgent, "work-item-importer")
	if _, err := f.blobs.Put(other.Digest, strings.NewReader(string(otherBody))); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutArtifact(t.Context(), other)
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		run     domain.RunID
		project domain.ProjectID
		source  domain.ArtifactID
		want    []domain.PreviousAnswer
	}{
		// The open re-ask on the first task has no answer yet, so the
		// answered first ask still decides.
		{run: "resubmitted-run", project: "project-1", source: f.source.ID, want: carried},
		{run: "other-project-run", project: "project-2", source: f.source.ID},
		{run: "other-source-run", project: "project-1", source: other.ID},
	} {
		// A resolved policy names its run, so each run gets its own.
		policy, err := domain.NewResolvedPolicy(tc.run, f.policy.Keys)
		if err != nil {
			t.Fatal(err)
		}
		policyArtifact := testSpecificationArtifact(t, domain.ArtifactID("policy-"+tc.run), domain.ArtifactKindPolicy,
			policy.Digest, domain.ProducerDaemon, "policy-resolver")
		policyBody, err := json.Marshal(policy.Keys)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.blobs.Put(policyArtifact.Digest, strings.NewReader(string(policyBody))); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
			return tx.PutArtifact(t.Context(), policyArtifact)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := SubmitSpecificationRun(t.Context(), f.store, SpecificationRunSpec{
			SpecificationRunID: tc.run, ImplementationRunID: tc.run + "-implementation",
			ProjectID: tc.project, SourceArtifactID: tc.source, PolicyArtifactID: policyArtifact.ID,
			ResolvedPolicy: policy,
			Publication: ProductionPublication{
				Title: "Implement approved work item", Body: "Implements the operator-approved specification.",
				CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
			},
		}); err != nil {
			t.Fatalf("%s: submit: %v", tc.run, err)
		}
		resubmitted := r
		resubmitted.run = tc.run
		requirePreviousAnswers(t, resubmitted.ask(t, 1, decisions), tc.want)
	}
}
