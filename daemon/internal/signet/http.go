package signet

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const maxCommandBodyBytes = 1 << 20

// RequestAuthorizer validates the paired-device credential on an HTTP
// request and returns the credential's device identity. The device unit (#67)
// supplies the real implementation; requiring this dependency now keeps the
// first HTTP surface fail-closed and prevents one device from naming another
// device_id in a command body.
type RequestAuthorizer func(*http.Request) (domain.DeviceID, bool)

// NewHTTPHandler serves the currently implemented OpenAPI surface. Listener
// placement stays with daemon composition, which must bind only to loopback
// or the configured Tailscale address (plan §5.2). A nil authorizer denies
// every request; there is no unauthenticated fallback.
func NewHTTPHandler(service *Service, authorize RequestAuthorizer) http.Handler {
	h := httpHandler{service: service, authorize: authorize}
	mux := http.NewServeMux()
	mux.Handle("GET /sync/bootstrap", h.authenticated(h.getBootstrap))
	mux.Handle("GET /sync/revision", h.authenticated(h.getRevision))
	mux.Handle("GET /attention/items", h.authenticated(h.listAttentionItems))
	mux.Handle("GET /attention/items/{item_id}", h.authenticated(h.getAttentionItem))
	mux.Handle("GET /attention/items/{item_id}/deliveries", h.authenticated(h.listAttentionItemDeliveries))
	mux.Handle("GET /runs", h.authenticated(h.listRuns))
	mux.Handle("GET /runs/{run_id}", h.authenticated(h.getRun))
	mux.Handle("GET /conversations/{conversation_id}", h.authenticated(h.getConversation))
	mux.Handle("POST /commands", h.authenticated(h.submitCommand))
	return mux
}

type httpHandler struct {
	service   *Service
	authorize RequestAuthorizer
}

type authenticatedHandler func(http.ResponseWriter, *http.Request, domain.DeviceID)

func (h httpHandler) authenticated(next authenticatedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.authorize == nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, errorResponse{Message: "unauthorized"})
			return
		}
		deviceID, ok := h.authorize(r)
		if !ok || deviceID == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, errorResponse{Message: "unauthorized"})
			return
		}
		next(w, r, deviceID)
	})
}

func (h httpHandler) getBootstrap(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	snapshot, err := h.service.Bootstrap(r.Context())
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (h httpHandler) getRevision(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	revision, err := h.service.Revision(r.Context())
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, revision)
}

func (h httpHandler) listAttentionItems(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	items, err := h.service.ListAttentionItems(r.Context())
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h httpHandler) getAttentionItem(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	item, err := h.service.GetAttentionItem(r.Context(), domain.ItemID(r.PathValue("item_id")))
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h httpHandler) listAttentionItemDeliveries(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	deliveries, err := h.service.ListAttentionItemDeliveries(r.Context(), domain.ItemID(r.PathValue("item_id")))
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deliveries)
}

func (h httpHandler) listRuns(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	runs, err := h.service.ListRuns(r.Context())
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (h httpHandler) getRun(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	run, err := h.service.GetRun(r.Context(), domain.RunID(r.PathValue("run_id")))
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h httpHandler) getConversation(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	conversation, err := h.service.GetConversation(r.Context(), domain.ConversationID(r.PathValue("conversation_id")))
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conversation)
}

type clientCommandRequest struct {
	CommandID             string                   `json:"command_id"`
	DeviceID              domain.DeviceID          `json:"device_id"`
	ExpectedEntityVersion int64                    `json:"expected_entity_version"`
	ExpectedBindings      map[string]domain.Digest `json:"expected_bindings"`
	Payload               decisionPayloadRequest   `json:"payload"`
}

type decisionPayloadRequest struct {
	ItemID          domain.ItemID    `json:"item_id"`
	Action          domain.Action    `json:"action"`
	ItemVersion     int              `json:"item_version"`
	PRHeadSHA       *string          `json:"pr_head_sha"`
	ArtifactDigests *[]domain.Digest `json:"artifact_digests"`
}

func (h httpHandler) submitCommand(w http.ResponseWriter, r *http.Request, authenticatedDevice domain.DeviceID) {
	var request clientCommandRequest
	if err := decodeCommandRequest(w, r, &request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Message: "request body is too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{Message: err.Error()})
		return
	}
	if request.ExpectedBindings == nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Message: "expected_bindings is required"})
		return
	}
	for _, digest := range request.ExpectedBindings {
		if digest == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Message: "expected_bindings values must be non-empty digests"})
			return
		}
	}
	if request.Payload.PRHeadSHA == nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Message: "payload.pr_head_sha is required"})
		return
	}
	if request.Payload.ArtifactDigests == nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Message: "payload.artifact_digests is required"})
		return
	}
	if request.DeviceID != authenticatedDevice {
		writeJSON(w, http.StatusForbidden, errorResponse{Message: "device_id does not match the authenticated device"})
		return
	}
	result, err := h.service.Submit(r.Context(), ClientCommand{
		CommandID: request.CommandID, DeviceID: request.DeviceID,
		ExpectedEntityVersion: request.ExpectedEntityVersion,
		Payload: DecisionPayload{
			ItemID: request.Payload.ItemID, Action: request.Payload.Action,
			ItemVersion: request.Payload.ItemVersion, PRHeadSHA: *request.Payload.PRHeadSHA,
			ArtifactDigests: *request.Payload.ArtifactDigests,
		},
	})
	if err != nil {
		writeCommandError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, normalizeCommandResult(result))
}

func decodeCommandRequest(w http.ResponseWriter, r *http.Request, dst *clientCommandRequest) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxCommandBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain exactly one JSON object")
		}
		return err
	}
	return nil
}

type staleVersionResponse struct {
	Message         string                `json:"message"`
	ReplacementItem AttentionItemSnapshot `json:"replacement_item"`
}

type errorResponse struct {
	Message string `json:"message"`
}

func writeCommandError(w http.ResponseWriter, err error) {
	var stale *StaleVersionError
	if errors.As(err, &stale) {
		writeJSON(w, http.StatusConflict, staleVersionResponse{
			Message: err.Error(), ReplacementItem: itemSnapshot(stale.Replacement, stale.Snapshot),
		})
		return
	}
	var closed *ClosedItemError
	if errors.As(err, &closed) {
		writeJSON(w, http.StatusConflict, staleVersionResponse{
			Message: err.Error(), ReplacementItem: itemSnapshot(closed.Item, closed.Snapshot),
		})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Message: "not found"})
		return
	}
	if isCommandRequestError(err) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, errorResponse{Message: "internal server error"})
}

func isCommandRequestError(err error) bool {
	for _, target := range []error{
		ErrActionNotAllowedForType, ErrUnsupportedAction,
		store.ErrActionNotOffered, store.ErrImmutableConflict,
		domain.ErrEmptyID, domain.ErrEmptyField, domain.ErrInvalidAction,
		domain.ErrNonPositive, domain.ErrDigestsNotCanonical, domain.ErrDuplicate,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func writeReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Message: "not found"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, errorResponse{Message: "internal server error"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
