package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func runSnapshotMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runSnapshotCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided snapshot:", err)
		os.Exit(1)
	}
}

// runSnapshotCommand writes a consistent copy of the live database to a new
// owner-only file, so an operator can inspect it with sqlite3 while a
// supervised daemon holds the live file exclusively. With a running daemon
// the copy is made in-process over the control socket; otherwise the command
// takes the daemon lock and copies directly. It never creates or migrates the
// source database.
func runSnapshotCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("freesided snapshot", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return errors.New("-db is required")
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("want one output path, got %d arguments", flags.NArg())
	}
	// The daemon's working directory is not the caller's, so a relative path
	// is resolved here before it can cross the control socket.
	output, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		return err
	}
	handle, client, err := openCommandStore(ctx, *dbPath, store.Options{}, storeExisting)
	if err != nil {
		return err
	}
	if client != nil {
		// A typed payload, not the argument list: callCommand appends the
		// canonical -db to an argument list, which would land after the
		// positional output path and fail to parse.
		return callCommand(ctx, client, "/snapshot", snapshotRequest{Output: output}, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	if err := writeSnapshot(ctx, handle.store, output); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, output)
	return err
}

type snapshotRequest struct {
	Output string `json:"output"`
}

// runSnapshotControl serves /snapshot inside the daemon, on the daemon's own
// store handle: under exclusive locking a second handle on the live file
// would fail busy.
func runSnapshotControl(ctx context.Context, st *store.Store, body []byte) (commandOutput, error) {
	request, err := decodeControlPayload[snapshotRequest](body)
	if err != nil {
		return commandOutput{}, err
	}
	if !filepath.IsAbs(request.Output) {
		return commandOutput{}, fmt.Errorf("snapshot output %q is not absolute", request.Output)
	}
	if err := writeSnapshot(ctx, st, request.Output); err != nil {
		return commandOutput{}, err
	}
	return commandOutput{Output: request.Output + "\n"}, nil
}

// writeSnapshot creates path exclusively and owner-only before VACUUM INTO
// fills it: O_EXCL refuses an existing file, and the 0600 create closes the
// window where a loose umask would leave a copy of every credential
// group-readable. VACUUM INTO accepts the empty file. On failure the file
// this call created is removed, never a pre-existing one.
func writeSnapshot(ctx context.Context, st *store.Store, path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // operator-selected output
	if err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	if err := file.Close(); err != nil {
		return errors.Join(fmt.Errorf("create snapshot: %w", err), os.Remove(path))
	}
	if err := st.Checkpoint(ctx, path); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}
