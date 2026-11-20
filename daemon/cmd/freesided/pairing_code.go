package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func runPairingCodeMain(args []string) {
	if err := runPairingCodeCommand(context.Background(), args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided pairing-code:", err)
		os.Exit(1)
	}
}

func runPairingCodeCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("freesided pairing-code", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateDir := flags.String("state-dir", "", "existing state directory of the running daemon (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	canonical, client, closeClient, err := pairingControlClient(*stateDir, 5*time.Second)
	if err != nil {
		return err
	}
	defer closeClient()
	body, err := json.Marshal(pairingCodeRequest{StateDir: canonical})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pairing-control/pairing-code", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("contact running daemon for pairing code: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("pairing control refused renewal (HTTP %d)", response.StatusCode)
	}
	var result pairingCodeResult
	if err := strictjson.DecodeReader(response.Body, &result, strictjson.RejectInvalidUTF8, 16<<10); err != nil ||
		result.APIURL == "" || result.PairingCode == "" || result.ExpiresAt.IsZero() {
		return errors.New("invalid pairing control response")
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write pairing code: %w", err)
	}
	return nil
}

// pairingControlClient connects a host command to the running daemon that
// owns stateDir. It returns the canonical state directory, which every
// request names so a daemon refuses a command meant for another, and a client
// whose every connection is a peer-authenticated Unix socket. Discovery is
// read-only: only the running daemon may write in its store.
func pairingControlClient(stateDir string, timeout time.Duration) (string, *http.Client, func(), error) {
	canonical, err := canonicalPairingStateDir(stateDir)
	if err != nil {
		return "", nil, nil, err
	}
	body, err := os.ReadFile(filepath.Join(canonical, pairingControlFileName)) //nolint:gosec // host-selected state directory
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil, pairingControlAbsent(canonical)
	}
	if err != nil {
		return "", nil, nil, fmt.Errorf("read pairing control address (is this daemon running?): %w", err)
	}
	var address pairingControlAddress
	if err := strictjson.Decode(body, &address, strictjson.RejectInvalidUTF8, 16<<10); err != nil ||
		!filepath.IsAbs(address.SocketPath) {
		return "", nil, nil, errors.New("invalid pairing control address")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, "unix", address.SocketPath)
			if err != nil {
				return nil, err
			}
			unixConn, ok := conn.(*net.UnixConn)
			if !ok {
				_ = conn.Close()
				return nil, errors.New("pairing control requires a Unix socket")
			}
			if err := authenticatePairingPeer(unixConn, unixPeerUID); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return conn, nil
		},
	}
	client := &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return canonical, client, transport.CloseIdleConnections, nil
}

// pairingControlAbsent explains a state directory with no pairing control
// address. A daemon takes the pairing control lock early in start-up but
// publishes the address only once it serves, after ward conformance, which
// can take minutes; a held lock tells that window apart from no daemon.
//
// The probe opens the lock file only if it exists and never creates it, so
// discovery stays read-only. Its flock can make a daemon starting in the
// same instant fail with ErrAlreadyRunning, the window openStoreOrControl
// already accepts; the lock is released at once.
func pairingControlAbsent(stateDir string) error {
	lockPath := filepath.Join(stateDir, pairingControlFileName) + ".daemon.lock"
	held, err := pairingControlLockHeld(lockPath)
	if err != nil {
		return fmt.Errorf("no pairing control address in %s, and its lock could not be checked: %w", stateDir, err)
	}
	if held {
		return fmt.Errorf("the daemon for %s is still starting and serves pairing once start-up finishes "+
			"(ward conformance can take minutes); rerun this once it prints its api_url", stateDir)
	}
	return fmt.Errorf("no daemon serves %s yet; if one is starting, rerun this once it prints its api_url", stateDir)
}

func pairingControlLockHeld(lockPath string) (bool, error) {
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0) //nolint:gosec // lock beside the host-selected state directory
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil
		}
		return false, err
	}
	return false, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
