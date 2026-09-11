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
	canonical, err := canonicalPairingStateDir(*stateDir)
	if err != nil {
		return err
	}
	// Discovery is read-only. Only the running daemon may mint in its store.
	body, err := os.ReadFile(filepath.Join(canonical, pairingControlFileName)) //nolint:gosec // host-selected state directory
	if err != nil {
		return fmt.Errorf("read pairing control address (is this daemon running?): %w", err)
	}
	var address pairingControlAddress
	if err := strictjson.Decode(body, &address, strictjson.RejectInvalidUTF8, 16<<10); err != nil ||
		!filepath.IsAbs(address.SocketPath) {
		return errors.New("invalid pairing control address")
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
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	body, err = json.Marshal(pairingCodeRequest{StateDir: canonical})
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
