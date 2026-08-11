package signet

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestWriteCommandErrorClassifiesAuthorityRejections pins the HTTP boundary
// for determinate recovery-decision rejections (issue #611, Codex round 6):
// each rolls its accepting transaction back with the item left open, so the
// client must receive an authoritative 400 (definitive no-effect, retry
// after repair) rather than a 500 it treats as possibly committed and
// replays. An unclassified error stays a 500 so a genuinely ambiguous
// failure keeps its pending slot.
func TestWriteCommandErrorClassifiesAuthorityRejections(t *testing.T) {
	t.Parallel()
	for _, sentinel := range []error{
		domain.ErrReviewConfigAdoptionIneffective,
		domain.ErrReviewConfigSupersessionInvalid,
		domain.ErrReviewConfigRecoveryBindingMissing,
		domain.ErrReviewConfigRecoveryBindingMismatch,
		domain.ErrReviewRecoveryBindingMissing,
		domain.ErrReviewRecoveryBindingMismatch,
		domain.ErrTransitionCommandMismatch,
		domain.ErrTransitionUnbacked,
	} {
		recorder := httptest.NewRecorder()
		writeCommandError(recorder, fmt.Errorf("submit adopt: %w", sentinel))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%v -> %d, want %d", sentinel, recorder.Code, http.StatusBadRequest)
		}
		var body errorResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Message == "" {
			t.Fatalf("%v body = %q, %v; want an error message", sentinel, recorder.Body.String(), err)
		}
	}
	recorder := httptest.NewRecorder()
	writeCommandError(recorder, errors.New("disk unplugged"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("ambiguous error -> %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

// TestWriteCommandErrorClassifiesOversizedRequestChanges guards that an
// over-limit request_changes message, rejected by validateCommandContent
// before the write, is reported as a 400 rather than an ambiguous 500.
func TestWriteCommandErrorClassifiesOversizedRequestChanges(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	writeCommandError(recorder, fmt.Errorf("action %q message is too long: %w",
		domain.ActionRequestChanges, domain.ErrClaimTextTooLarge))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("oversized request_changes -> %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	var body errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Message == "" {
		t.Fatalf("body = %q, %v; want an error message", recorder.Body.String(), err)
	}
}
