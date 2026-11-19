package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// seededQuestionRunID and seededQuestionItemID are the representative seed's
// agent_question run and item (seedfixture.seedAgentQuestion).
const seededQuestionRunID domain.RunID = "run-freeside-question"

var seededQuestionItemID = domain.ItemID("question-" + string(domain.SpecificationInvocationID(seededQuestionRunID, 1)))

// TestInspectItemGolden pins the read's shape over the seeded question. A
// change to these bytes is a change to attentionItemReadVersion.
func TestInspectItemGolden(t *testing.T) {
	t.Parallel()
	st, _ := openSeededStore(t)
	item, err := readAttentionItemFrom(t.Context(), st, seededQuestionItemID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Error != nil || item.AgentQuestion == nil || len(item.AgentQuestion.Decisions) != 1 {
		t.Fatalf("item = %+v, want the seeded question", item)
	}
	got, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "inspect_item", append(got, '\n'))

	// The task record lists the item and never carries the question.
	record := readRecord(t, st, taskOfRun(t, st, seededQuestionRunID))
	if recordItem(record, seededQuestionItemID) == nil {
		t.Fatalf("record does not list %s", seededQuestionItemID)
	}
	if out := mustJSON(t, record); bytes.Contains(out, []byte(item.AgentQuestion.Decisions[0].Question)) {
		t.Fatal("the task record carries the agent's question")
	}
}
