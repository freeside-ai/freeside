// Command freeside-signet-dev composes the signet HTTP surface into a
// runnable process for the §5.14 real-daemon convergence pass (issue #72):
// store, service, and request authorizer behind the contract handler on one
// loopback listener, plus a dev-only control surface on a second loopback
// listener for test choreography the contract deliberately does not offer
// (minting pairing codes, checkpointing and restoring the store through the
// real §5.14 epoch-rotating restore path, rotating the sync epoch on its own,
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
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func main() {
	flags := flag.NewFlagSet("freeside-signet-dev", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dbPath := flags.String("db", "", "SQLite database path (required; created if absent)")
	listenAddr := flags.String("listen", "127.0.0.1:0", "contract listener address (loopback only)")
	controlAddr := flags.String("control", "127.0.0.1:0", "control listener address (loopback only)")
	ntfyURL := flags.String("ntfy-url", "", "ntfy server URL for delivery submission (optional; deliveries fail closed without it)")
	topicKeyFile := flags.String("topic-key-file", "", "path to the persisted ntfy topic key (optional; must be disjoint from -db and its .blobs/.checkpoints siblings). When set, device topics survive restarts; when unset, the key is per-process and reusing -db fails closed")
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
	// ".blobs" and ".checkpoints" siblings: those are the backup/workspace
	// surfaces this credential must stay out of.
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
func run(ctx context.Context, cfg config) (_ *harness, err error) {
	if cfg.DBPath == "" {
		return nil, errors.New("-db is required")
	}

	// One rollback for every partial-construction failure: each acquired
	// resource registers its closer, and the deferred guard unwinds them in
	// reverse order unless success flips true. On success the listeners and
	// store pass to the harness (Close owns them), so the guard must not fire
	// and double-close them.
	var cleanup []func()
	success := false
	defer func() {
		if !success {
			for i := len(cleanup) - 1; i >= 0; i-- {
				cleanup[i]()
			}
		}
	}()

	apiListener, err := listenLoopback(cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("contract listener: %w", err)
	}
	cleanup = append(cleanup, func() { _ = apiListener.Close() })

	controlListener, err := listenLoopback(cfg.ControlAddr)
	if err != nil {
		return nil, fmt.Errorf("control listener: %w", err)
	}
	cleanup = append(cleanup, func() { _ = controlListener.Close() })

	// Checkpoints are standalone SQLite snapshot files beside the store; the
	// control surface writes them here and restores from them through the real
	// §5.14 restore (epoch rotation included). A snapshot carries the whole
	// store (device credentials, pairing rows), and this path is predictable
	// and persists across runs, so a pre-existing loose or symlinked directory
	// must fail closed rather than silently receive them: MkdirAll leaves an
	// existing directory's mode untouched, so re-assert owner-only access.
	// Validated before store.Open so a rejected directory cannot strand a
	// just-created, never-paired store behind the issue #133 topic-key gate.
	checkpointDir := cfg.DBPath + ".checkpoints"
	if err := os.MkdirAll(checkpointDir, 0o700); err != nil {
		return nil, fmt.Errorf("create checkpoint dir: %w", err)
	}
	if err := assertPrivateDir(checkpointDir); err != nil {
		return nil, err
	}

	// storePreexisting must be sampled before store.Open, which creates the
	// database file when absent: a pre-existing store is the conservative
	// proxy for "may already hold paired devices" that gates topic-key
	// creation below (issue #133). The store exposes no device count, and
	// over-refusing an existing-but-empty store fails safe.
	_, statErr := os.Stat(cfg.DBPath)
	storePreexisting := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
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
			return nil, err
		}
	}

	st, err := store.Open(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { _ = st.Close() })

	// A random per-process pairing key: codes minted through the control
	// surface are redeemable only against this process, which is exactly the
	// harness's lifetime.
	pairingKey := make([]byte, 32)
	if _, err := rand.Read(pairingKey); err != nil {
		return nil, fmt.Errorf("generate pairing key: %w", err)
	}
	// Attachments live in a digest-addressed directory beside the store
	// (plan §5.14: text in SQLite, blobs in the artifact store by digest);
	// composing it here keeps PUT/GET /attachments serviceable rather than
	// failing closed on a nil blob store.
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
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
			Handler:           newControlHandler(service, st, checkpointDir),
			ReadHeaderTimeout: 5 * time.Second,
		},
		serveErrs: make(chan error, 2),
	}
	go func() { h.serveErrs <- h.apiServer.Serve(apiListener) }()
	go func() { h.serveErrs <- h.controlServer.Serve(controlListener) }()
	success = true
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

// assertPrivateDir fails closed unless path is a real directory (not a
// symlink) reachable only by its owner, mirroring the keystore's permission
// gate (internal/publish) for a credential-bearing path. Lstat, not Stat: a
// symlinked checkpoint directory would carry snapshots outside the validated
// location, so it must fail the kind check rather than be followed.
func assertPrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("checkpoint dir %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("checkpoint dir %s is not a plain directory", path)
	}
	// Require exactly 0700: reject group/other bits (a leak), and also a
	// missing owner write/execute bit (e.g. 0500), which would pass a
	// group/other-only check yet fail closed only later, at the first
	// /control/checkpoint write, instead of here at startup.
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("checkpoint dir %s is mode %04o, want owner-only 0700", path, info.Mode().Perm())
	}
	return nil
}

// newControlHandler serves the dev-only choreography surface. It lives in
// package main, never in internal/signet, so the contract handler cannot
// grow a control route by accident: what production composes is exactly what
// this binary's api listener serves.
func newControlHandler(service *signet.Service, st *store.Store, checkpointDir string) http.Handler {
	c := controlHandler{service: service, store: st, checkpointDir: checkpointDir}
	mux := http.NewServeMux()
	mux.Handle("POST /control/pairing-codes", http.HandlerFunc(c.mintPairingCode))
	mux.Handle("POST /control/checkpoint", http.HandlerFunc(c.checkpoint))
	mux.Handle("POST /control/restore", http.HandlerFunc(c.restore))
	mux.Handle("POST /control/epoch", http.HandlerFunc(c.rotateEpoch))
	mux.Handle("POST /control/items", http.HandlerFunc(c.putItem))
	mux.Handle("POST /control/runs", http.HandlerFunc(c.putRun))
	mux.Handle("POST /control/deliveries", http.HandlerFunc(c.submitDelivery))
	return mux
}

type controlHandler struct {
	service       *signet.Service
	store         *store.Store
	checkpointDir string
}

// checkpointName matches the filenames POST /control/checkpoint issues: 16
// random bytes as hex plus ".db". Restore accepts only these, so a control
// caller cannot make Store.Restore copy an arbitrary database (whose raw table
// copy bypasses the write-time domain gates) from outside the daemon-issued
// set.
var checkpointName = regexp.MustCompile(`^[0-9a-f]{32}\.db$`)

// resolveCheckpoint constrains a caller-supplied checkpoint reference to a
// file this handler issued: an issued name inside checkpointDir that is a
// regular file, not a symlink. The path is derived from the validated base
// name alone, so any directory the caller supplies (including traversal) is
// discarded.
func (c controlHandler) resolveCheckpoint(supplied string) (string, error) {
	name := filepath.Base(supplied)
	if !checkpointName.MatchString(name) {
		return "", fmt.Errorf("not an issued checkpoint: %q", supplied)
	}
	path := filepath.Join(c.checkpointDir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("checkpoint %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("checkpoint %s is not a regular file", name)
	}
	return path, nil
}

// checkpoint snapshots the live store to a fresh file and returns its path, so
// a later restore can roll the daemon back to exactly this state.
func (c controlHandler) checkpoint(w http.ResponseWriter, r *http.Request) {
	var name [16]byte
	if _, err := rand.Read(name[:]); err != nil {
		controlError(w, err)
		return
	}
	path := filepath.Join(c.checkpointDir, fmt.Sprintf("%x.db", name))
	if err := c.store.Checkpoint(r.Context(), path); err != nil {
		controlError(w, err)
		return
	}
	controlJSON(w, http.StatusCreated, map[string]string{"checkpoint": path})
}

// restoreRequest names a checkpoint produced by POST /control/checkpoint.
type restoreRequest struct {
	Checkpoint string `json:"checkpoint"`
}

// restore is the real §5.14 restore: it rolls the store back to the named
// checkpoint and rotates the sync epoch in the same operation, so a client
// cached against the intervening state must discard and bootstrap.
func (c controlHandler) restore(w http.ResponseWriter, r *http.Request) {
	var req restoreRequest
	if !decodeControlRequest(w, r, &req) {
		return
	}
	if req.Checkpoint == "" {
		controlJSON(w, http.StatusBadRequest, map[string]string{"message": "checkpoint path is required"})
		return
	}
	path, err := c.resolveCheckpoint(req.Checkpoint)
	if err != nil {
		controlJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	state, err := c.store.Restore(r.Context(), path)
	if err != nil {
		controlError(w, err)
		return
	}
	controlJSON(w, http.StatusOK, map[string]any{
		"sync_epoch": state.SyncEpoch,
		"revision":   state.Revision,
	})
}

func (c controlHandler) mintPairingCode(w http.ResponseWriter, r *http.Request) {
	plaintext, _, err := c.service.MintPairingCode(r.Context())
	if err != nil {
		controlError(w, err)
		return
	}
	controlJSON(w, http.StatusCreated, map[string]string{"pairing_code": plaintext})
}

// rotateEpoch rotates the sync epoch on its own, without a data restore: the
// minimal §5.14 test-8 stimulus for "a bare epoch change evicts the cache".
// The real restore (data rollback plus rotation) is POST /control/restore.
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

// putItemRequest seeds or advances one attention item. The item body is
// constructed here, mirroring signet's own test fixture, so the Swift suite
// never re-encodes the full domain shape and the domain gates (Validate,
// per-type action policy) still run on every put. Type and RequestedDecision
// are optional overrides: an omitted type seeds ready_for_final_review, and an
// omitted (nil, never merely empty) action set seeds that type's default offer.
// The policy-parity suite (#204) sets both to drive the whole
// attention-type/action matrix, including the invalid/unknown strings the typed
// Swift enums cannot represent, through the real per-type action policy.
type putItemRequest struct {
	ID                string   `json:"id"`
	ItemVersion       int      `json:"item_version"`
	Reason            string   `json:"reason"`
	Type              string   `json:"type"`
	RequestedDecision []string `json:"requested_decision"`
	// TextClaim, when non-empty, attaches one markdown text claim carrying
	// this content (#217), digest-bound server-side by ComputeDigest, so the
	// convergence suite can assert the inline carrier survives the real wire.
	TextClaim string `json:"text_claim"`
}

func (c controlHandler) putItem(w http.ResponseWriter, r *http.Request) {
	var req putItemRequest
	if !decodeControlRequest(w, r, &req) {
		return
	}
	if req.Reason == "" {
		req.Reason = "seeded by the convergence harness"
	}
	// Type and the offered action set default to the historical fixture
	// (ready_for_final_review offering open_pr/stop/dismiss), so the sync tests'
	// seedItem(id, version) calls are unchanged. An explicit empty action set is
	// deliberately distinct from an omitted one (nil): the parity suite sends
	// [] to drive the blocked accept and the non-blocked ErrNoActions rejection.
	itemType := domain.AttentionReadyForFinalReview
	if req.Type != "" {
		itemType = domain.AttentionType(req.Type)
	}
	requested := []domain.Action{domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss}
	if req.RequestedDecision != nil {
		requested = make([]domain.Action, len(req.RequestedDecision))
		for i, a := range req.RequestedDecision {
			requested[i] = domain.Action(a)
		}
	}
	var claims []domain.AgentClaim
	if req.TextClaim != "" {
		text := domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: req.TextClaim}
		claims = []domain.AgentClaim{{
			Label:    "summary",
			Artifact: domain.ArtifactID("art-sum-" + req.ID),
			Digest:   text.ComputeDigest(),
			Text:     &text,
			Provenance: domain.Provenance{
				ProducerClass:        domain.ProducerAgent,
				ProducerInvocationID: domain.InvocationID("inv-" + req.ID),
				HeadBinding:          domain.HeadBound,
				SourceHeadSHA:        "cafebabe",
				SensitivityClass:     domain.SensitivityNormal,
			},
		}}
	}
	runID := domain.RunID("run-" + req.ID)
	var reviewRecoveryBinding *domain.ReviewRecoveryBinding
	if itemType == domain.AttentionReviewContradiction {
		reviewRecoveryBinding = &domain.ReviewRecoveryBinding{
			RunID: runID, InvocationID: domain.InvocationID("review-" + req.ID), Round: 1,
			BaseSHA: "beefcafe", HeadSHA: "cafebabe",
			FailureDigest: domain.Digest("sha256:failure-" + req.ID),
		}
	}
	var reviewConfigurationRecovery *domain.ReviewConfigurationRecoveryBinding
	if itemType == domain.AttentionReviewConfiguration {
		reviewConfigurationRecovery = &domain.ReviewConfigurationRecoveryBinding{
			RunID: runID, InvocationID: domain.InvocationID("review-" + req.ID), Round: 2,
			BaseSHA: "beefcafe", HeadSHA: "cafebabe",
			FailureDigest: domain.Digest("sha256:failure-" + req.ID),
			Repo:          "freeside-ai/demo", RepositoryID: 123456789,
			SupersededProfileDigest: domain.Digest("sha256:profile-" + req.ID),
		}
	}
	var findingAdjudication *domain.FindingAdjudicationBinding
	if itemType == domain.AttentionFindingAdjudication {
		binding, err := c.seedFindingAdjudicationAuthority(r.Context(), runID, req.ID)
		if err != nil {
			controlError(w, err)
			return
		}
		findingAdjudication = &binding
	}
	createdInstant := time.Now().UTC()
	createdAt := &createdInstant
	if existing, err := c.service.GetAttentionItem(r.Context(), domain.ItemID(req.ID)); err == nil {
		createdAt = existing.Item.CreatedAt
	} else if !errors.Is(err, store.ErrNotFound) {
		controlJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	expires := createdInstant.Add(24 * time.Hour)
	var posture *domain.HealthPosture
	if itemType == domain.AttentionSystemHealth {
		blocking := domain.HealthPostureBlocking
		posture = &blocking
	}
	var codexReenrollmentRecoveryBinding *domain.CodexReenrollmentRecoveryBinding
	if itemType == domain.AttentionSystemHealth &&
		slices.Contains(requested, domain.ActionResolveReenrollment) {
		codexReenrollmentRecoveryBinding = &domain.CodexReenrollmentRecoveryBinding{
			AuthIdentityID: "codex-convergence", LeaseFence: 1,
			AuthStoreDigest:      domain.Digest("sha256:codex-reenrollment-" + req.ID),
			AccessTokenExpiresAt: expires.UTC(),
		}
	}
	var prReference *domain.PRReference
	if itemType == domain.AttentionReadyForFinalReview {
		prReference = &domain.PRReference{Repo: "freeside-ai/demo", Number: 123}
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:                               domain.ItemID(req.ID),
		ProjectID:                        "proj-convergence",
		Subject:                          domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:                             itemType,
		Priority:                         domain.PriorityNormal,
		Reason:                           req.Reason,
		RequestedDecision:                requested,
		AgentClaims:                      claims,
		PRHeadSHA:                        "cafebabe",
		PRReference:                      prReference,
		ReviewRecoveryBinding:            reviewRecoveryBinding,
		CodexReenrollmentRecoveryBinding: codexReenrollmentRecoveryBinding,
		ReviewConfigurationRecovery:      reviewConfigurationRecovery,
		FindingAdjudication:              findingAdjudication,
		ItemVersion:                      req.ItemVersion,
		InterruptionClass:                domain.InterruptionPlannedGate,
		CreatedAt:                        createdAt,
		ExpiresWhen:                      &expires,
		Posture:                          posture,
		Status:                           domain.StatusOpen,
	}, nil)
	if err != nil {
		// NewAttentionItem's Validate rejects an unknown type or a malformed
		// action string here, before policy runs: a client-visible 400, not a
		// harness 500. This is the invalid/unknown-input arm of the parity suite.
		controlJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	if err := c.service.PutItem(r.Context(), item); err != nil {
		// PutItem rejects policy violations before its Write; anything
		// else (store contention, I/O) is the harness's fault, not the
		// request's, and must not read as a scripted 400 in a test log.
		// ErrActionNotAllowedForType (a valid action wrong for the type) and
		// ErrNoActions (a non-blocked type offering nothing) are per-type policy
		// rejections. A valid run proposal reaches the later specialized-admission
		// rejection because this generic test route cannot create its authority
		// records atomically. All three are definitive client-visible 400s.
		if errors.Is(err, signet.ErrActionNotAllowedForType) || errors.Is(err, domain.ErrNoActions) ||
			errors.Is(err, signet.ErrProposalAdmissionRequired) {
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

func convergenceDigest(value string) domain.Digest {
	return domain.Digest(contentaddr.Sum([]byte(value)))
}

func (c controlHandler) seedFindingAdjudicationAuthority(
	ctx context.Context, runID domain.RunID, suffix string,
) (domain.FindingAdjudicationBinding, error) {
	createdAt := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	findingID := domain.FindingID("finding-" + suffix)
	specDigest := convergenceDigest("spec-" + suffix)
	policyDigest := convergenceDigest("policy-" + suffix)
	instructionDigest := convergenceDigest("instructions-" + suffix)
	finding := domain.Finding{
		ID: findingID, RunID: runID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/example.go", StartLine: 1, EndLine: 1},
		Message:  "the finding contradicts the approved work unit",
		RawText:  "the finding contradicts the approved work unit", CreatedAt: createdAt,
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: domain.InvocationID("review-" + suffix), RunID: runID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: convergenceDigest("configuration-" + suffix),
		InstructionDigest:   instructionDigest, CostOwner: "owner",
		BaseSHA: "beefcafe", HeadSHA: "cafebabe", CompletedAt: createdAt,
		CompletionEvidence: convergenceDigest("completion-" + suffix),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{findingID},
	})
	if err != nil {
		return domain.FindingAdjudicationBinding{}, err
	}
	entry, err := domain.NewModelAdjudicationEntry(
		findingID, domain.GoalContradictory, nil, domain.RouteDecline,
		domain.ConfidenceHigh, "the finding contradicts the approved work unit",
		nil, []string{"AGENTS.md"}, []string{"the work contract is current"}, nil, nil,
	)
	if err != nil {
		return domain.FindingAdjudicationBinding{}, err
	}
	artifact, err := domain.NewFindingAdjudication(
		runID, 1, specDigest, instructionDigest, policyDigest,
		[]domain.FindingAdjudicationEntry{entry}, createdAt,
	)
	if err != nil {
		return domain.FindingAdjudicationBinding{}, err
	}
	if err := c.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "proj-convergence", SpecDigest: specDigest, PolicyDigest: policyDigest,
		}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		return domain.FindingAdjudicationBinding{}, err
	}
	return domain.FindingAdjudicationBinding{
		RunID: runID, Round: 1, AdjudicationDigest: artifact.Digest,
		Proposals: []domain.FindingAdjudicationProposal{{
			FindingID: findingID, Producer: entry.Producer,
			GoalRelationship: entry.GoalRelationship, Compatibility: entry.Compatibility,
			Route: entry.Route, Rationale: entry.Rationale,
			CitedRules: entry.CitedRules, Assumptions: entry.Assumptions,
			OpenQuestions: entry.OpenQuestions, Confidence: entry.Confidence,
			OfferedAlternatives: []domain.OfferedAlternative{{
				Route: domain.RouteDispute, Consequence: "park for a human decision",
			}},
		}},
	}, nil
}

type putRunRequest struct {
	ID string `json:"id"`
}

// putRun seeds one complete run-list/timeline fixture through the real store
// APIs. The Swift convergence suite supplies only the identity; every
// operator-visible fact is daemon-constructed and domain-validated here.
func (c controlHandler) putRun(w http.ResponseWriter, r *http.Request) {
	var req putRunRequest
	if !decodeControlRequest(w, r, &req) {
		return
	}
	if req.ID == "" {
		controlJSON(w, http.StatusBadRequest, map[string]string{"message": "run id is required"})
		return
	}
	runID := domain.RunID(req.ID)
	invocationID := domain.InvocationID("inv-" + req.ID)
	stageID := domain.StageID("stage-" + req.ID)
	policyDigest := domain.Digest("sha256:policy-" + req.ID)
	run := domain.Run{
		ID: runID, ProjectID: "proj-convergence",
		SpecDigest: "sha256:spec-" + domain.Digest(req.ID), PolicyDigest: policyDigest,
		Stages: []domain.Stage{{
			ID: stageID, RunID: runID, Name: "implementation",
			Attempts: []domain.Attempt{{
				ID: domain.AttemptID("attempt-" + req.ID), StageID: stageID,
				Number: 1, InvocationID: invocationID,
			}},
		}},
	}
	// The control route is an idempotent convergence fixture: replaying the
	// same run must reconstruct the same immutable schedule even when calls
	// cross a wall-clock second.
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	fireAt := now.Add(time.Hour)
	itemID := domain.ItemID("item-" + req.ID)
	itemVersion := 1
	schedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID: domain.ScheduleID("schedule-" + req.ID), ProjectID: run.ProjectID,
		Kind: domain.ScheduleReviewWaitThreshold,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &itemVersion,
		},
		RunID: &runID, PolicyDigest: &policyDigest,
		CreatedAt: now, FireAt: &fireAt,
	})
	if err != nil {
		controlJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	if err := c.store.Write(r.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(r.Context(), run); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(r.Context(), string(invocationID),
			string(domain.ProductionInvocationRequestedKind), []byte(fmt.Sprintf(
				`{"invocation_id":%q,"run_id":%q,"stage_id":%q}`,
				invocationID, runID, stageID))); err != nil {
			return err
		}
		return tx.PutSchedule(r.Context(), schedule)
	}); err != nil {
		controlError(w, err)
		return
	}
	if err := c.store.Write(r.Context(), func(tx *store.WriteTx) error {
		if err := tx.AppendRunMilestone(r.Context(), domain.RunMilestone{
			RunID: runID, Kind: domain.MilestoneRunSubmitted,
			InvocationID: &invocationID, RecordedAt: now,
		}); err != nil {
			return err
		}
		if err := tx.RecordInvocationObservation(r.Context(), domain.InvocationObservation{
			InvocationID: invocationID, RunID: runID,
			Status: domain.ObservedStatusRunning, Live: true, ObservedAt: now,
		}); err != nil {
			return err
		}
		return tx.RecordRunHold(r.Context(), domain.RunHoldObservation{
			RunID: runID, InvocationID: &invocationID,
			Reason:          domain.HoldVerificationFindings,
			FirstObservedAt: now, LastObservedAt: now,
		})
	}); err != nil {
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
	var req submitDeliveryRequest
	if !decodeControlRequest(w, r, &req) {
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

// decodeControlRequest enforces the dev control boundary uniformly for every
// POST handler: the body cap (413 on overflow, including a valid prefix
// trailed by over-cap bytes), unknown-field rejection, and exactly one JSON
// value with no trailing non-whitespace (400). The control surface remains a
// separate, dev-only boundary while sharing the daemon's strict JSON gate.
// On an unacceptable body it writes the response and returns false, so the
// caller returns without touching the store, preserving decode-before-mutate.
func decodeControlRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBodyBytes)
	err := strictjson.DecodeReader(
		r.Body, dst, strictjson.TolerateInvalidUTF8, strictjson.Limit(maxControlBodyBytes),
	)
	if errors.Is(err, strictjson.ErrTrailingData) {
		err = errors.New("request body must contain exactly one JSON value")
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			controlJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"message": "request body is too large"})
		} else {
			controlJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		}
		return false
	}
	return true
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
