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
		writeCommandError(recorder, nil, fmt.Errorf("submit adopt: %w", sentinel))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%v -> %d, want %d", sentinel, recorder.Code, http.StatusBadRequest)
		}
		var body errorResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Message == "" {
			t.Fatalf("%v body = %q, %v; want an error message", sentinel, recorder.Body.String(), err)
		}
	}
	recorder := httptest.NewRecorder()
	writeCommandError(recorder, nil, errors.New("disk unplugged"))
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
	writeCommandError(recorder, nil, fmt.Errorf("action %q message is too long: %w",
		domain.ActionRequestChanges, domain.ErrClaimTextTooLarge))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("oversized request_changes -> %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	var body errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Message == "" {
		t.Fatalf("body = %q, %v; want an error message", recorder.Body.String(), err)
	}
}

func TestNoOpProposalRevisionHTTPMappingIsAuthoritative(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	writeCommandError(recorder, nil, fmt.Errorf("submit revision: %w", ErrInvalidProposalDecisionPayload))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("no-op proposal revision -> %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestCapabilityManifestDigestHTTPMapping(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"command_id":"command-capability","device_id":"device-1",
		"expected_entity_version":1,"expected_bindings":{},
		"payload":{"item_id":"item-1","action":"retry_with_capabilities",
		"item_version":1,"pr_head_sha":"","artifact_digests":[],
		"capability_manifest_digest":"sha256:capability"}}
	`)
	var request clientCommandRequest
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request.Payload.CapabilityManifestDigest == nil ||
		*request.Payload.CapabilityManifestDigest != "sha256:capability" {
		t.Fatalf("capability manifest digest = %v", request.Payload.CapabilityManifestDigest)
	}
}

func TestCapabilityManifestSelectionHTTPMappingsAreAuthoritative(t *testing.T) {
	t.Parallel()
	for _, sentinel := range []error{
		ErrInvalidCapabilityRetryDecisionPayload,
		ErrCapabilityManifestNotOffered,
	} {
		recorder := httptest.NewRecorder()
		writeCommandError(recorder, nil, fmt.Errorf("submit capability retry: %w", sentinel))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%v -> %d, want %d", sentinel, recorder.Code, http.StatusBadRequest)
		}
		var body errorResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Message == "" {
			t.Fatalf("%v body = %q, %v; want an error message", sentinel, recorder.Body.String(), err)
		}
	}
}

func TestSnoozedProposalHTTPMappingsAreAuthoritative(t *testing.T) {
	t.Parallel()
	command := httptest.NewRecorder()
	writeCommandError(command, nil, fmt.Errorf("submit: %w", ErrProposalSnoozed))
	if command.Code != http.StatusBadRequest {
		t.Fatalf("snoozed command -> %d, want %d", command.Code, http.StatusBadRequest)
	}
	read := httptest.NewRecorder()
	writeReadError(read, fmt.Errorf("get: %w", ErrProposalSnoozed))
	if read.Code != http.StatusNotFound {
		t.Fatalf("snoozed read -> %d, want %d", read.Code, http.StatusNotFound)
	}
}
