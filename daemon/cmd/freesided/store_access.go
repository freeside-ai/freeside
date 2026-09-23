package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// storeAccess selects the direct store only while holding the daemon's lock.
// A live daemon always owns its store; clients never open that database.
type storeAccess struct {
	lock   *daemonlock.Lock
	client *controlClient
}

func canonicalDatabasePath(path string) (string, error) {
	return canonicalDatabasePathDepth(path, 0)
}

func canonicalDatabasePathDepth(path string, depth int) (string, error) {
	if depth >= 40 {
		return "", errors.New("canonicalize database path: too many symlinks")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("canonicalize database path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("canonicalize database parent: %w", err)
	}
	candidate := filepath.Join(parent, filepath.Base(abs))
	info, err := os.Lstat(candidate)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(candidate)
		if err != nil {
			return "", fmt.Errorf("read database symlink: %w", err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(parent, target)
		}
		return canonicalDatabasePathDepth(target, depth+1)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat database path: %w", err)
	}
	return candidate, nil
}

func openStoreOrControl(dbPath string) (_ storeAccess, err error) {
	canonical, err := canonicalDatabasePath(dbPath)
	if err != nil {
		return storeAccess{}, err
	}
	lock, err := daemonlock.Acquire(dbPath)
	if err == nil {
		return storeAccess{lock: lock}, nil
	}
	if !errors.Is(err, daemonlock.ErrAlreadyRunning) {
		return storeAccess{}, err
	}
	client, err := newControlClient(canonical)
	if err != nil {
		return storeAccess{}, fmt.Errorf("%s.daemon.lock is held, but the daemon control socket is unavailable (stop or upgrade the daemon): %w", canonical, err)
	}
	return storeAccess{client: client}, nil
}

func requireNoDaemon(command, dbPath string) (*daemonlock.Lock, error) {
	lock, err := daemonlock.Acquire(dbPath)
	if errors.Is(err, daemonlock.ErrAlreadyRunning) {
		return nil, fmt.Errorf("%s: stop the daemon first (%s.daemon.lock is held)", command, dbPath)
	}
	return lock, err
}

func (a storeAccess) Close() error {
	if a.client != nil {
		a.client.Close()
	}
	return a.lock.Close()
}

type (
	daemonStoreContextKey struct{}
	daemonStoreContext    struct {
		store           *store.Store
		blobs           *signet.BlobStore
		backupFiles     *store.LocalBackupFiles
		dbPath          string
		approvedRecipes map[domain.Digest]bool
	}
)

type commandStoreHandle struct {
	store    *store.Store
	access   storeAccess
	borrowed bool
}

func (h *commandStoreHandle) Close() error {
	if h == nil || h.borrowed {
		return nil
	}
	return errors.Join(h.store.Close(), h.access.Close())
}

type storeOpenMode uint8

const (
	storeMigrating storeOpenMode = iota
	storeExisting
)

type (
	directStoreOpener    func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error)
	directStoreOpenerKey struct{}
)

func openCommandStore(ctx context.Context, dbPath string, opts store.Options, mode storeOpenMode) (*commandStoreHandle, *controlClient, error) {
	if daemon, ok := ctx.Value(daemonStoreContextKey{}).(daemonStoreContext); ok {
		canonical, err := canonicalDatabasePath(dbPath)
		if err != nil {
			return nil, nil, err
		}
		if canonical != daemon.dbPath {
			return nil, nil, errors.New("database path mismatch")
		}
		for digest := range opts.ApprovedRecipes {
			if !daemon.approvedRecipes[digest] {
				return nil, nil, fmt.Errorf("recipe %s is not approved by the running daemon", digest)
			}
		}
		return &commandStoreHandle{store: daemon.store, borrowed: true}, nil, nil
	}
	access, err := openStoreOrControl(dbPath)
	if err != nil {
		return nil, nil, err
	}
	if access.client != nil {
		return nil, access.client, nil
	}
	var st *store.Store
	if open, ok := ctx.Value(directStoreOpenerKey{}).(directStoreOpener); ok {
		st, err = open(ctx, dbPath, opts, mode)
	} else if mode == storeExisting {
		st, err = store.OpenExisting(ctx, dbPath, opts)
	} else {
		st, _, err = openStoreWithTopicKey(ctx, dbPath, opts)
	}
	if err != nil {
		_ = access.Close()
		return nil, nil, err
	}
	return &commandStoreHandle{store: st, access: access}, nil, nil
}

type commandOutput struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
	Kind   string `json:"kind,omitempty"`
}

type controlError struct {
	Message string `json:"message"`
	Kind    string `json:"kind,omitempty"`
}

type remoteControlError struct {
	message string
	cause   error
}

func (e remoteControlError) Error() string { return e.message }
func (e remoteControlError) Unwrap() error { return e.cause }

func controlErrorKind(err error) string {
	switch {
	case errors.Is(err, observe.ErrUsage):
		return "usage"
	case errors.Is(err, store.ErrNotFound):
		return "not_found"
	case errors.Is(err, store.ErrImmutableConflict):
		return "immutable_conflict"
	case errors.Is(err, domain.ErrParentKeyMismatch):
		return "parent_key_mismatch"
	case errors.Is(err, domain.ErrUnapprovedRecipe):
		return "unapproved_recipe"
	default:
		return ""
	}
}

func controlErrorCause(kind string) error {
	switch kind {
	case "usage":
		return observe.ErrUsage
	case "not_found":
		return store.ErrNotFound
	case "immutable_conflict":
		return store.ErrImmutableConflict
	case "parent_key_mismatch":
		return domain.ErrParentKeyMismatch
	case "unapproved_recipe":
		return domain.ErrUnapprovedRecipe
	default:
		return nil
	}
}

func writeControlError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(controlError{Message: err.Error(), Kind: controlErrorKind(err)})
}

func callCommand(ctx context.Context, client *controlClient, route string, payload any, stdout io.Writer) error {
	defer client.Close()
	var result commandOutput
	if args, ok := payload.([]string); ok {
		payload = canonicalControlArgs(args, client.dbPath)
	}
	if err := client.call(ctx, route, payload, &result); err != nil {
		return err
	}
	_, err := io.WriteString(stdout, result.Output)
	if err == nil && result.Error != "" {
		return remoteControlError{message: result.Error, cause: controlErrorCause(result.Kind)}
	}
	return err
}

func canonicalControlArgs(args []string, dbPath string) []string {
	// Each command has already parsed successfully, so its final -db wins.
	// Appending avoids changing a user value that happens to look like a flag.
	canonical := append([]string(nil), args...)
	if len(canonical) > 0 && canonical[len(canonical)-1] == "--" {
		canonical = canonical[:len(canonical)-1]
	}
	return append(canonical, "-db", dbPath)
}

func getCommand(ctx context.Context, client *controlClient, route string, stdout io.Writer) error {
	defer client.Close()
	var result commandOutput
	if err := client.get(ctx, route, nil, &result); err != nil {
		return err
	}
	_, err := io.WriteString(stdout, result.Output)
	if err == nil && result.Error != "" {
		return remoteControlError{message: result.Error, cause: controlErrorCause(result.Kind)}
	}
	return err
}

type controlClient struct {
	dbPath    string
	http      *http.Client
	transport *http.Transport
}

type controlEnvelope struct {
	DBPath  string          `json:"db_path"`
	Payload json.RawMessage `json:"payload"`
}

// maxControlRequestBytes accommodates the aggregate prepared submission, not
// just its input files. Two 4 MiB byte slices need 11 MiB after base64 encoding;
// Keys and ResolvedPolicy repeat up to 8 MiB of canonical policy JSON. Derived
// paths can add 8 MiB (splitting a comma adds two quotes), and the numeric
// work-unit declaration adds 4 MiB. Publication's bounded strings fit within
// 1 MiB even with six-byte JSON escapes. Round that 32 MiB subtotal up by two
// input-file budgets for CLI identities, database path, and envelope fields.
const maxControlRequestBytes = 10 * maxSubmissionFileBytes

func newControlClient(dbPath string) (*controlClient, error) {
	body, err := os.ReadFile(dbPath + ".control.json") //nolint:gosec // operator-selected database
	if err != nil {
		return nil, err
	}
	var address pairingControlAddress
	if err := strictjson.Decode(body, &address, strictjson.RejectInvalidUTF8, 16<<10); err != nil || !filepath.IsAbs(address.SocketPath) {
		return nil, errors.New("invalid daemon control address")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", address.SocketPath)
		if err != nil {
			return nil, err
		}
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			_ = conn.Close()
			return nil, errors.New("daemon control requires a Unix socket")
		}
		if err := authenticatePairingPeer(unixConn, unixPeerUID); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}}
	return &controlClient{dbPath: dbPath, transport: transport, http: &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *controlClient) Close() {
	if c != nil {
		c.transport.CloseIdleConnections()
	}
}

func (c *controlClient) call(ctx context.Context, route string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(controlEnvelope{DBPath: c.dbPath, Payload: body})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://daemon-control"+route, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return c.do(request, result)
}

func (c *controlClient) get(ctx context.Context, route string, query url.Values, result any) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("db_path", c.dbPath)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://daemon-control"+route+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	return c.do(request, result)
}

func (c *controlClient) do(request *http.Request, result any) error {
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("contact running daemon: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // response is complete before return
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		var remote controlError
		if err := strictjson.Decode(body, &remote, strictjson.RejectInvalidUTF8, 4096); err == nil && remote.Message != "" {
			return remoteControlError{message: remote.Message, cause: controlErrorCause(remote.Kind)}
		}
		return fmt.Errorf("daemon control refused request (HTTP %d): %s", response.StatusCode, bytes.TrimSpace(body))
	}
	if result == nil {
		return nil
	}
	return strictjson.DecodeReader(response.Body, result, strictjson.RejectInvalidUTF8, 20<<20)
}

func (p *pairingControl) handleGet(mux *http.ServeMux, route string, operation func(context.Context, *http.Request) (any, error)) {
	mux.HandleFunc("GET "+route, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("db_path") != p.dbPath {
			writeControlError(w, errors.New("database path mismatch"))
			return
		}
		result, err := operation(r.Context(), r)
		if err != nil {
			writeControlError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(result)
	})
}

func (p *pairingControl) handle(mux *http.ServeMux, route string, operation func(context.Context, json.RawMessage) (any, error)) {
	mux.HandleFunc("POST "+route, func(w http.ResponseWriter, r *http.Request) {
		var envelope controlEnvelope
		if err := strictjson.DecodeReader(r.Body, &envelope, strictjson.RejectInvalidUTF8, maxControlRequestBytes); err != nil || envelope.DBPath != p.dbPath || len(envelope.Payload) == 0 {
			writeControlError(w, errors.New("database path mismatch or invalid request"))
			return
		}
		result, err := operation(r.Context(), envelope.Payload)
		if err != nil {
			writeControlError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(result)
	})
}
