package engine

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func TestScopeConflictRejectsAmbiguousOrForeignSource(t *testing.T) {
	t.Parallel()
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "ambiguous", true: "foreign"}[foreign], func(t *testing.T) {
			f := newSpecificationFixture(t, false, 4)
			workflow := productionPublicationWorkflow{store: f.store}
			task := productionPublicationTask{ProducingInvocationID: "implementation"}
			entry := export.EvidenceEntry{
				Label:      export.ScopeConflictEvidenceLabel,
				Provenance: export.EvidenceProvenance{ProducerInvocationID: "implementation"},
			}
			task.Replay.Evidence.Entries = []export.EvidenceEntry{entry, entry}
			if foreign {
				entry.Provenance.ProducerInvocationID = "another-invocation"
				task.Replay.Evidence.Entries = []export.EvidenceEntry{entry}
			}
			if _, err := workflow.recoverScopeConflictTask(t.Context(), &task, productionBinding{}); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("untrusted scope source = %v", err)
			}
		})
	}
}
