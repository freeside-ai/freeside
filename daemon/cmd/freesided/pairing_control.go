package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const pairingControlFileName = "pairing-control.json"

type pairingControlAddress struct {
	SocketPath string `json:"socket_path"`
}

type pairingCodeRequest struct {
	StateDir string `json:"state_dir"`
}

type pairingCodeResult struct {
	APIURL      string    `json:"api_url"`
	PairingCode string    `json:"pairing_code"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// pairingControl exposes only host-authorized minting, separate from Signet's
// network listener and device authority. The state-directory lock prevents two
// daemons with different databases from advertising over each other's endpoint.
type pairingControl struct {
	stateDir   string
	lock       *daemonlock.Lock
	listener   *net.UnixListener
	server     *http.Server
	peerUID    func(*net.UnixConn) (uint32, error)
	owned      []pairingControlFile
	closeOnce  sync.Once
	closeError error
}

type pairingControlFile struct {
	path string
	info os.FileInfo
}

func newPairingControl(stateDir string) (_ *pairingControl, err error) {
	if stateDir == "" {
		return nil, errors.New("pairing control state directory is required")
	}
	// Production composition already supports a fresh state root. Reserve it
	// before those consumers run; command-side discovery never creates it.
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create pairing control state directory: %w", err)
	}
	stateDir, err = canonicalPairingStateDir(stateDir)
	if err != nil {
		return nil, err
	}
	lock, err := daemonlock.Acquire(filepath.Join(stateDir, pairingControlFileName))
	if err != nil {
		return nil, fmt.Errorf("acquire pairing control: %w", err)
	}
	p := &pairingControl{
		stateDir: stateDir, lock: lock, peerUID: unixPeerUID,
		server: &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second},
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, p.Close())
		}
	}()
	// macOS limits Unix socket addresses to 104 bytes. State roots and even
	// os.TempDir() can exceed that; advertise a short, private runtime path.
	dir, err := os.MkdirTemp("/tmp", "freeside-pairing-")
	if err != nil {
		return nil, fmt.Errorf("create pairing socket directory: %w", err)
	}
	if err := p.remember(dir); err != nil {
		return nil, err
	}
	socketPath := filepath.Join(dir, "control.sock")
	p.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen for host pairing: %w", err)
	}
	// net's default unlink-on-close would remove a replacement socket too.
	p.listener.SetUnlinkOnClose(false)
	if err := p.remember(socketPath); err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return nil, fmt.Errorf("protect pairing socket: %w", err)
	}
	return p, nil
}

func (p *pairingControl) configure(apiURL string, mint func(context.Context) (string, domain.PairingCode, error)) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /pairing-code", func(w http.ResponseWriter, r *http.Request) {
		var request pairingCodeRequest
		if err := strictjson.DecodeReader(r.Body, &request, strictjson.RejectInvalidUTF8, 16<<10); err != nil ||
			request.StateDir != p.stateDir {
			http.Error(w, "pairing control state directory mismatch or invalid request", http.StatusBadRequest)
			return
		}
		code, record, err := mint(r.Context())
		if err != nil {
			// The response and ordinary logs must never echo a code or a
			// credential-bearing service error.
			http.Error(w, "could not mint pairing code", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(pairingCodeResult{
			APIURL: apiURL, PairingCode: code, ExpiresAt: record.ExpiresAt,
		})
	})
	p.server.Handler = mux
}

func canonicalPairingStateDir(path string) (string, error) {
	if path == "" {
		return "", errors.New("-state-dir is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve pairing state directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve existing pairing state directory: %w", err)
	}
	return resolved, nil
}

func (p *pairingControl) remember(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect pairing control resource: %w", err)
	}
	p.owned = append(p.owned, pairingControlFile{path: path, info: info})
	return nil
}

func (p *pairingControl) publish() error {
	body, err := json.Marshal(pairingControlAddress{SocketPath: p.listener.Addr().String()})
	if err != nil {
		return err
	}
	path := filepath.Join(p.stateDir, pairingControlFileName)
	if err := atomicfile.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("publish pairing control address: %w", err)
	}
	return p.remember(path)
}

func (p *pairingControl) Serve() error {
	return p.server.Serve(pairingControlListener{UnixListener: p.listener, peerUID: p.peerUID})
}

func (p *pairingControl) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		var errs []error
		if p.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), serverShutdownBudget)
			defer cancel()
			if err := p.server.Shutdown(ctx); err != nil {
				errs = append(errs, err, p.server.Close())
			}
		}
		if p.listener != nil {
			if err := p.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
		for i := len(p.owned) - 1; i >= 0; i-- {
			owned := p.owned[i]
			current, err := os.Lstat(owned.path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if os.SameFile(owned.info, current) {
				errs = append(errs, os.Remove(owned.path))
			}
		}
		p.closeError = errors.Join(append(errs, p.lock.Close())...)
	})
	return p.closeError
}

type pairingControlListener struct {
	*net.UnixListener
	peerUID func(*net.UnixConn) (uint32, error)
}

func (l pairingControlListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		if err := authenticatePairingPeer(conn, l.peerUID); err != nil {
			_ = conn.Close()
			continue
		}
		return conn, nil
	}
}

func authenticatePairingPeer(conn *net.UnixConn, peerUID func(*net.UnixConn) (uint32, error)) error {
	uid, err := peerUID(conn)
	if err != nil || int64(uid) != int64(os.Geteuid()) {
		return errors.New("pairing control requires the daemon's operating-system user")
	}
	return nil
}

func unixPeerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var credErr error
	if err := raw.Control(func(fd uintptr) { uid, credErr = socketPeerUID(int(fd)) }); err != nil {
		return 0, err
	}
	return uid, credErr
}
