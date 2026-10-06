package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestLinkPublicationSuccessors: a run's sealed successors are its chain only
// when they form one unbranched line from the root ready item. The order they
// are listed in decides nothing.
func TestLinkPublicationSuccessors(t *testing.T) {
	t.Parallel()
	const runID = domain.RunID("run-1")
	root := domain.ProductionReadyItemID(runID)
	feedback := func(predecessor domain.ItemID, command string) domain.PublicationSuccessor {
		return domain.PublicationSuccessor{
			Origin: domain.PublicationSuccessorFeedback, RunID: runID, CommandID: command, PredecessorItemID: predecessor,
		}
	}
	first := feedback(root, "return-1")
	second := feedback(first.ReadyItemID(), "return-2")
	reentry := domain.PublicationSuccessor{
		Origin: domain.PublicationSuccessorExternalReview, RunID: runID, PredecessorItemID: second.ReadyItemID(),
	}

	for name, tc := range map[string]struct {
		sealed []domain.PublicationSuccessor
		want   []domain.PublicationSuccessor
		err    error
	}{
		"no successor": {},
		"listed out of order": {
			sealed: []domain.PublicationSuccessor{reentry, first, second},
			want:   []domain.PublicationSuccessor{first, second, reentry},
		},
		"a second successor of the root": {
			sealed: []domain.PublicationSuccessor{first, second, feedback(root, "return-3")},
			err:    domain.ErrParentKeyMismatch,
		},
		// The off-chain authority: an external_review row beside the feedback
		// return that already superseded its ready item.
		"a second successor of a superseded item": {
			sealed: []domain.PublicationSuccessor{first, second, {
				Origin: domain.PublicationSuccessorExternalReview, RunID: runID, PredecessorItemID: first.ReadyItemID(),
			}},
			err: domain.ErrParentKeyMismatch,
		},
		"a successor the root does not reach": {
			sealed: []domain.PublicationSuccessor{first, reentry},
			err:    domain.ErrParentKeyMismatch,
		},
		"a successor of another run's root": {
			sealed: []domain.PublicationSuccessor{feedback(domain.ProductionReadyItemID("run-2"), "return-1")},
			err:    domain.ErrParentKeyMismatch,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := linkPublicationSuccessors(runID, tc.sealed)
			if !errors.Is(err, tc.err) || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("link = %#v, %v; want %#v, %v", got, err, tc.want, tc.err)
			}
		})
	}
}
