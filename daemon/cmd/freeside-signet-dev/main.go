// Command freeside-signet-dev composes the signet HTTP surface into a
// runnable process for the §5.14 real-daemon convergence pass (issue #72):
// store, service, and request authorizer behind the contract handler on one
// loopback listener, plus a dev-only control surface on a second loopback
// listener for test choreography the contract deliberately does not offer
// (minting pairing codes, rotating the sync epoch to simulate a restore,
// seeding attention items). It is a test harness, not the product daemon:
// `freesided` and its operational surface stay with plan §10. Both listeners
// refuse non-loopback addresses outright (plan §5.2), and the pairing key is
// random per process, so nothing this binary serves can outlive or leave the
// machine that ran it.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func main() {
	flags := flag.NewFlagSet("freeside-signet-dev", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dbPath := flags.String("db", "", "SQLite database path (required; created if absent)")
	listenAddr := flags.String("listen", "127.0.0.1:0", "contract listener address (loopback only)")
	controlAddr := flags.String("control", "127.0.0.1:0", "control listener address (loopback only)")
	ntfyURL := flags.String("ntfy-url", "", "ntfy server URL for delivery submission (optional; deliveries fail closed without it)")
	topicKeyFile := flags.String("topic-key-file", "", "path to the persisted ntfy topic key (optional; must be disjoint from -db and its .blobs sibling). When set, device topics survive restarts; when unset, the key is per-process and reusing -db fails closed")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	h, err := run(ctx, config{DBPath: *dbPath, ListenAddr: *listenAddr, ControlAddr: *controlAddr, NtfyURL: *ntfyURL, TopicKeyFile: *topicKeyFile})
	if err != nil {
		fmt.Fprintln(os.Stderr, "freeside-signet-dev:", err)
		os.Exit(1)
	}
	// One readiness line on stdout is the whole startup protocol: the
	// orchestration script reads it to learn both bound URLs.
	if err := json.NewEncoder(os.Stdout).Encode(h.readiness()); err != nil {
		fmt.Fprintln(os.Stderr, "freeside-signet-dev:", err)
		os.Exit(1)
	}

	<-ctx.Done()
	if err := h.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "freeside-signet-dev:", err)
		os.Exit(1)
	}
}

type config struct {
	DBPath      string
	ListenAddr  string
	ControlAddr string
	// NtfyURL points delivery submission at an ntfy server (the convergence
	// suite scripts a local fake). Empty means no channel is composed and
	// POST /control/deliveries reports the pipeline's fail-closed refusal.
	NtfyURL string
	// TopicKeyFile persists the ntfy topic key so device topics survive a
	// restart against the same store (issue #133). Empty keeps the historical
	// per-process key, which is safe only against a fresh store; reusing a
	// pre-existing store without it fails closed rather than silently
	// rekeying paired devices. It must be disjoint from DBPath and its
	// ".blobs" sibling: the store is the backup/workspace surface this
	// credential must stay out of.
	TopicKeyFile string
}

// readiness is the startup line: both bound URLs, so callers never guess
// ports (the defaults bind port 0).
type readiness struct {
	APIURL     string `json:"api_url"`
	ControlURL string `json:"control_url"`
}

type harness struct {
	store         *store.Store
	apiListener   net.Listener
	controlListen net.Listener
	apiServer     *http.Server
	controlServer *http.Server
	serveErrs     chan error
}

// run opens the store, composes the two servers, and starts serving. It is
// main's whole body behind a testable seam; Close releases everything run
// acquired.
func run(ctx context.Context, cfg config) (*harness, error) {
	if cfg.DBPath == "" {
		return nil, errors.New("-db is required")
	}

	apiListener, err := listenLoopback(cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("contract listener: %w", err)
	}
	controlListener, err := listenLoopback(cfg.ControlAddr)
	if err != nil {
		_ = apiListener.Close()
		return nil, fmt.Errorf("control listener: %w", err)
	}

	// storePreexisting must be sampled before store.Open, which creates the
	// database file when absent: a pre-existing store is the conservative
	// proxy for "may already hold paired devices" that gates topic-key
	// creation below (issue #133). The store exposes no device count, and
	// over-refusing an existing-but-empty store fails safe.
	_, statErr := os.Stat(cfg.DBPath)
	storePreexisting := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		_ = apiListener.Close()
		_ = controlListener.Close()
		return nil, fmt.Errorf("stat store path: %w", statErr)
	}

	// Resolve the ntfy topic key before opening the store: store.Open creates
	// and migrates the database when absent, so a bad -topic-key-file (rejected
	// path or unreadable key) must fail here rather than leave a fresh store
	// behind. A left-behind store would flip storePreexisting to true on the
	// operator's corrected retry and refuse the still-absent key as a possible
	// rekey (issue #133), stranding a never-paired setup.
	var topicKey []byte
	if cfg.NtfyURL != "" {
		topicKey, err = resolveTopicKey(cfg.TopicKeyFile, cfg.DBPath, storePreexisting)
		if err != nil {
			_ = apiListener.Close()
			_ = controlListener.Close()
			return nil, err
		}
	}

	st, err := store.Open(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		_ = apiListener.Close()
		_ = controlListener.Close()
		return nil, err
	}

	// A random per-process pairing key: codes minted through the control
	// surface are redeemable only against this process, which is exactly the
	// harness's lifetime.
	pairingKey := make([]byte, 32)
	if _, err := rand.Read(pairingKey); err != nil {
		_ = apiListener.Close()
		_ = controlListener.Close()
		_ = st.Close()
		return nil, fmt.Errorf("generate pairing key: %w", err)
	}
	// Attachments live in a digest-addressed directory beside the store
	// (plan §5.14: text in SQLite, blobs in the artifact store by digest);
	// composing it here keeps PUT/GET /attachments serviceable rather than
	// failing closed on a nil blob store.
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		_ = apiListener.Close()
		_ = controlListener.Close()
		_ = st.Close()
		return nil, fmt.Errorf("open blob store: %w", err)
	}
	options := []signet.Option{signet.WithPairingKey(pairingKey), signet.WithBlobStore(blobs)}
	if cfg.NtfyURL != "" {
		// topicKey was resolved before store.Open (above); the deep link points
		// at this process's own contract listener.
		options = append(options, signet.WithNtfy(signet.NtfyConfig{
			BaseURL:      cfg.NtfyURL,
			TopicKey:     topicKey,
			ClickBaseURL: "http://" + apiListener.Addr().String(),
		}))
	}
	service := signet.NewService(st, options...)

	h := &harness{
		store:         st,
		apiListener:   apiListener,
		controlListen: controlListener,
		apiServer: &http.Server{
			Handler:           signet.NewHTTPHandler(service, signet.NewRequestAuthorizer(st)),
			ReadHeaderTimeout: 5 * time.Second,
		},
		controlServer: &http.Server{
			Handler:           newControlHandler(service, st),
			ReadHeaderTimeout: 5 * time.Second,
		},
		serveErrs: make(chan error, 2),
	}
	go func() { h.serveErrs <- h.apiServer.Serve(apiListener) }()
	go func() { h.serveErrs <- h.controlServer.Serve(controlListener) }()
	return h, nil
}

func (h *harness) readiness() readiness {
	return readiness{
		APIURL:     "http://" + h.apiListener.Addr().String(),
		ControlURL: "http://" + h.controlListen.Addr().String(),
	}
}

// Close shuts both servers down gracefully and closes the store. Safe to
// call once; returns the first shutdown error.
func (h *harness) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var errs []error
	errs = append(errs, h.apiServer.Shutdown(ctx), h.controlServer.Shutdown(ctx))
	for range 2 {
		if err := <-h.serveErrs; !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	errs = append(errs, h.store.Close())
	return errors.Join(errs...)
}

// listenLoopback binds addr and fails closed unless the bound address is
// loopback: the §5.2 constraint NewHTTPHandler's contract delegates to the
// composition, and the control surface is unauthenticated by design so it
// must never be reachable off-host.
func listenLoopback(addr string) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !tcpAddr.IP.IsLoopback() {
		_ = listener.Close()
		return nil, fmt.Errorf("refusing non-loopback address %q", listener.Addr())
	}
	return listener, nil
}

const maxControlBodyBytes = 1 << 20

// newControlHandler serves the dev-only choreography surface. It lives in
// package main, never in internal/signet, so the contract handler cannot
// grow a control route by accident: what production composes is exactly what
// this binary's api listener serves.
func newControlHandler(service *signet.Service, st *store.Store) http.Handler {
	c := controlHandler{service: service, store: st}
	mux := http.NewServeMux()
	mux.Handle("POST /control/pairing-codes", http.HandlerFunc(c.mintPairingCode))
	mux.Handle("POST /control/epoch", http.HandlerFunc(c.rotateEpoch))
	mux.Handle("POST /control/items", http.HandlerFunc(c.putItem))
	mux.Handle("POST /control/deliveries", http.HandlerFunc(c.submitDelivery))
	return mux
}

type controlHandler struct {
	service *signet.Service
	store   *store.Store
}

func (c controlHandler) mintPairingCode(w http.ResponseWriter, r *http.Request) {
	plaintext, _, err := c.service.MintPairingCode(r.Context())
	if err != nil {
		controlError(w, err)
		return
	}
	controlJSON(w, http.StatusCreated, map[string]string{"pairing_code": plaintext})
}

func (c controlHandler) rotateEpoch(w http.ResponseWriter, r *http.Request) {
	state, err := c.store.NewEpoch(r.Context())
	if err != nil {
		controlError(w, err)
		return
	}
	controlJSON(w, http.StatusOK, map[string]any{
		"sync_epoch": state.SyncEpoch,
		"revision":   state.Revision,
	})
}

// putItemRequest seeds or advances one attention item. The shape stays
// minimal on purpose: the item body is constructed here, mirroring signet's
// own test fixture, so the Swift suite never re-encodes domain shapes and
// the domain gates (Validate, per-type action policy) still run on every
// put.
type putItemRequest struct {
	ID          string `json:"id"`
	ItemVersion int    `json:"item_version"`
	Reason      string `json:"reason"`
}

func (c controlHandler) putItem(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxControlBodyBytes))
	if err != nil {
		controlError(w, err)
		return
	}
	var req putItemRequest
	if err := json.Unmarshal(body, &req); err != nil {
		controlJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	if req.Reason == "" {
		req.Reason = "seeded by the convergence harness"
	}
	runID := domain.RunID("run-" + req.ID)
	expires := time.Now().Add(24 * time.Hour)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        domain.ItemID(req.ID),
		ProjectID: "proj-convergence",
		Subject:   domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:      domain.AttentionReadyForFinalReview,
		Priority:  domain.PriorityNormal,
		Reason:    req.Reason,
		RequestedDecision: []domain.Action{
			domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss,
		},
		PRHeadSHA:         "cafebabe",
		ItemVersion:       req.ItemVersion,
		InterruptionClass: domain.InterruptionPlannedGate,
		ExpiresWhen:       &expires,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		controlJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	if err := c.service.PutItem(r.Context(), item); err != nil {
		// PutItem rejects policy violations before its Write; anything
		// else (store contention, I/O) is the harness's fault, not the
		// request's, and must not read as a scripted 400 in a test log.
		if errors.Is(err, signet.ErrActionNotAllowedForType) {
			controlJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		controlError(w, err)
		return
	}
	state, err := c.store.ServerState(r.Context())
	if err != nil {
		controlError(w, err)
		return
	}
	controlJSON(w, http.StatusOK, map[string]any{"revision": state.Revision})
}

// submitDeliveryRequest drives one notification attempt through the real
// pipeline (delivery row, timing recompute, ntfy publish).
type submitDeliveryRequest struct {
	ItemID   string `json:"item_id"`
	DeviceID string `json:"device_id"`
}

func (c controlHandler) submitDelivery(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxControlBodyBytes))
	if err != nil {
		controlError(w, err)
		return
	}
	var req submitDeliveryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		controlJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	row, err := c.service.SubmitDelivery(r.Context(), domain.ItemID(req.ItemID), domain.DeviceID(req.DeviceID))
	switch {
	case err == nil:
	case errors.Is(err, signet.ErrNotifierUnavailable):
		controlJSON(w, http.StatusServiceUnavailable, map[string]string{"message": err.Error()})
		return
	case errors.Is(err, signet.ErrDeviceNotActive),
		errors.Is(err, signet.ErrItemNotOpenForDelivery),
		errors.Is(err, store.ErrNotFound):
		controlJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	case errors.Is(err, signet.ErrChannelRejected):
		// The submitted-only row is committed and honest; the provider said
		// no. 502 keeps a scripted channel failure distinct from a harness
		// fault.
		controlJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error(), "delivery": row})
		return
	default:
		controlError(w, err)
		return
	}
	controlJSON(w, http.StatusOK, map[string]any{"delivery": row})
}

func controlError(w http.ResponseWriter, err error) {
	controlJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
}

func controlJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is already written; an encode failure has no recovery.
	_ = json.NewEncoder(w).Encode(body)
}
