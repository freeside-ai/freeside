package fakepublication

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestTerminalDigestsMatchFrozenPreTaskEncodings(t *testing.T) {
	body, err := os.ReadFile("testdata/terminal_before_tasks.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Task              Task
		Item              json.RawMessage
		BeforeTasks       domain.Digest
		BeforePRReference domain.Digest
	}
	if err := json.Unmarshal(body, &vectors); err != nil {
		t.Fatal(err)
	}
	for i, vector := range vectors {
		var item domain.AttentionItem
		if err := json.Unmarshal(vector.Item, &item); err != nil {
			t.Fatal(err)
		}
		var legacy struct {
			Names *struct {
				Project  domain.DisplayName `json:"project"`
				WorkUnit domain.DisplayName `json:"work_unit"`
			} `json:"display_names"`
		}
		if err := json.Unmarshal(vector.Item, &legacy); err != nil {
			t.Fatal(err)
		}
		if legacy.Names != nil {
			item.DisplayNames = &domain.DisplayNames{Project: legacy.Names.Project, Task: legacy.Names.WorkUnit}
		}
		before, err := TerminalDigestBeforeTasks(vector.Task, item)
		if err != nil {
			t.Fatal(err)
		}
		old, err := TerminalDigestBeforePRReference(vector.Task, item)
		if err != nil {
			t.Fatal(err)
		}
		if before != vector.BeforeTasks || old != vector.BeforePRReference {
			t.Fatalf("vector %d: pre-task=%s want %s; v1=%s want %s", i, before, vector.BeforeTasks, old, vector.BeforePRReference)
		}
	}
}
