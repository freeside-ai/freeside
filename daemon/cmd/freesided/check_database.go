package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// checkDatabaseDirName is the one fixed directory, under the tier's state
// directory, that holds the copy. Every run removes it before it starts, so a
// copy a SIGKILLed run left behind never outlives the next check.
const checkDatabaseDirName = "check-database"

func runCheckDatabaseMain(args []string) {
	// SIGHUP too: closing the installer's terminal should not leave the
	// credential-bearing copy until the next run.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// The first signal cancels the check, which then cleans up; a second one
	// kills the process the default way, and the next run sweeps the copy.
	context.AfterFunc(ctx, stop)
	err := runCheckDatabaseCommand(ctx, args, os.Stdout, os.Stderr, checkDatabaseSeams{})
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
	case errors.Is(err, errCheckDatabaseUsage):
		fmt.Fprintln(os.Stderr, "freesided check-database:", err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "freesided check-database:", err)
		os.Exit(1)
	}
}

var errCheckDatabaseUsage = errors.New("usage")

// checkDatabaseSeams lets a test replace the store open; the zero value is
// production.
type checkDatabaseSeams struct {
	open func(context.Context, string, store.Options) (*store.Store, error)
}

// runCheckDatabaseCommand checks that this build can open, migrate, and
// decode the operator's data before an update replaces the running daemon.
// It copies the live database into an owner-only directory under the tier's
// state directory, migrates the copy, decodes every stored row through the
// store's own reads, and runs the store-only startup steps, all under the
// options the LaunchAgent daemon (driver disabled) opens its store with.
//
// It is offline: its only connection is the daemon's local control socket.
// The copy carries every credential the database holds, so it is mode 0600,
// is removed on every exit path a process can catch, and a leftover from a
// killed run is removed before the next copy is made.
//
// Errors from making the copy print in full; they carry no row content.
// Errors from reading the copy never print their underlying text, which can
// quote stored values: a failure names the migration file, the kinds of row,
// or the startup step, and nothing else.
func runCheckDatabaseCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer, seams checkDatabaseSeams,
) (err error) {
	// Registered first, so it runs after the copy is closed and removed. A
	// panic while migrating or reading the copy (a data migration over an
	// unexpected row, say) would otherwise print its value, which can quote
	// a stored row, and its stack.
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.Join(err, errors.New("the check panicked while reading the copy; the panic is not printed, since it can quote stored rows"))
		}
	}()
	flags := flag.NewFlagSet("freesided check-database", flag.ContinueOnError)
	flags.SetOutput(stderr)
	rawEnvironment := flags.String("environment", "", "tier the LaunchAgent daemon runs as: prod, dev, or ephemeral (required)")
	dbPath := flags.String("db", "", "the tier's live SQLite database (required)")
	stateDir := flags.String("state-dir", "", "the tier's state directory; the copy is made under it (required)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %w", errCheckDatabaseUsage, err)
	}
	if *rawEnvironment == "" || *dbPath == "" || *stateDir == "" || flags.NArg() != 0 {
		return fmt.Errorf("%w: -environment, -db, and -state-dir are required, and nothing else", errCheckDatabaseUsage)
	}
	env, err := parseEnvironment(*rawEnvironment)
	if err != nil {
		return fmt.Errorf("%w: %w", errCheckDatabaseUsage, err)
	}
	// Absolute, as freesided snapshot makes its output: the daemon refuses a
	// relative copy path, and the stopped path must name the same files.
	if *dbPath, err = filepath.Abs(*dbPath); err != nil {
		return fmt.Errorf("%w: -db: %w", errCheckDatabaseUsage, err)
	}
	if *stateDir, err = filepath.Abs(*stateDir); err != nil {
		return fmt.Errorf("%w: -state-dir: %w", errCheckDatabaseUsage, err)
	}
	if err := checkDatabaseEnvironment(env, *dbPath, *stateDir); err != nil {
		return fmt.Errorf("%w: %w", errCheckDatabaseUsage, err)
	}
	dir := filepath.Join(*stateDir, checkDatabaseDirName)
	// Every run removes the copy's directory first, so a -db inside it would
	// be deleted as a stale copy.
	if *dbPath == dir || strings.HasPrefix(*dbPath, dir+string(filepath.Separator)) {
		return fmt.Errorf("%w: -db %s is inside the check's own copy directory %s", errCheckDatabaseUsage, *dbPath, dir)
	}
	opts, err := config{Environment: env}.storeOptions()
	if err != nil {
		return err
	}
	if seams.open == nil {
		// The copy is checked, never served: nothing derives a device topic
		// from it, and openStoreWithTopicKey would refuse a store with no
		// key file beside it, which the copy deliberately lacks.
		seams.open = store.Open //nolint:forbidigo // see above
	}
	if _, err := os.Stat(*dbPath); err != nil {
		return fmt.Errorf("live database: %w", err)
	}

	if err := removeCheckDatabaseDir(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	defer func() { err = errors.Join(err, removeCheckDatabaseDir(dir)) }()
	copyPath := filepath.Join(dir, "freeside.db")
	if err := copyLiveDatabase(ctx, *dbPath, copyPath); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(copyPath)
	if err != nil {
		return fmt.Errorf("inspect copy: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("copy %s is %v, want a regular file with mode 0600", copyPath, info.Mode())
	}

	before, err := store.ReadSchemaVersion(ctx, copyPath)
	if err != nil {
		return errors.New("the copy's schema version could not be read")
	}
	st, err := seams.open(ctx, copyPath, opts)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var migration *store.MigrationError
		if errors.As(err, &migration) {
			return fmt.Errorf("migration %s failed on the copy; the live database is unchanged", migration.Name)
		}
		return errors.New("this build could not open the copy; its migration history does not match this build's")
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	after, err := st.SchemaVersion(ctx)
	if err != nil {
		return errors.New("the migrated copy's schema version could not be read")
	}

	report, err := st.DecodeStoredRows(ctx)
	if err != nil {
		return err
	}
	if len(report.Failed) > 0 {
		return fmt.Errorf("these kinds of stored row failed to decode on the copy: %s", strings.Join(report.Failed, ", "))
	}

	discard := slog.New(slog.DiscardHandler)
	if err := daemoncompose.ConvergeStoredState(ctx, st, false, time.Now, discard); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		step := "an unnamed step"
		var stepErr *daemoncompose.StoredStateError
		if errors.As(err, &stepErr) {
			step = stepErr.Step
		}
		return fmt.Errorf("startup step %q failed on the copy", step)
	}

	_, err = fmt.Fprintf(stdout,
		"database check passed: schema %d to %d, %d kinds of stored row decoded, %d tables not walked\n",
		before, after, report.Walked, report.NotWalked)
	return err
}

// checkDatabaseEnvironment refuses a path from the wrong tier, as the daemon
// does at start, so a check can never copy another tier's database.
func checkDatabaseEnvironment(env environment, dbPath, stateDir string) error {
	paths := environmentPaths{DB: dbPath, StateDir: stateDir}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := checkEnvironment(env, home, paths, ""); err != nil {
		return err
	}
	if env == environmentEphemeral {
		accountHome, err := currentAccountHome()
		if err != nil {
			return err
		}
		return checkEnvironment(env, accountHome, paths, "")
	}
	return nil
}

// copyLiveDatabase writes the copy. With a daemon running, the daemon writes
// it over the control socket, on its own handle, as freesided snapshot does.
// The call runs to completion even after a signal, so cleanup never races a
// copy the daemon is still writing. With none running, the check takes the
// daemon lock and copies the file without reading its schema, then releases
// the lock at once.
func copyLiveDatabase(ctx context.Context, dbPath, copyPath string) error {
	access, err := openStoreOrControl(dbPath)
	if err != nil {
		return err
	}
	if access.client != nil {
		return callCommand(context.WithoutCancel(ctx), access.client, "/snapshot",
			snapshotRequest{Output: copyPath}, io.Discard)
	}
	return errors.Join(store.CopyDatabaseFile(ctx, dbPath, copyPath), access.Close())
}

// removeCheckDatabaseDir removes the copy's directory, retrying once. A copy
// that cannot be removed is reported by path, never left silently.
func removeCheckDatabaseDir(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("could not remove the database copy under %s; remove it by hand: %w", dir, err)
	}
	return nil
}
