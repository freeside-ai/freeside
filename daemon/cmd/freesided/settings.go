package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// settingsRoute carries every settings verb: the args it forwards name the
// verb, so the running daemon runs the same dispatch.
const settingsRoute = "/settings"

// settingsTakeEffect is what a set says about when its change
// applies. The installed daemon reads the records only at start until the
// project gates read them live (#3209).
const settingsTakeEffect = "at the daemon's next start"

// runSettingsMain is the stored run settings verb (issue #2082): `settings
// show` prints a scope's record and `settings set` records a revision of
// one section.
func runSettingsMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runSettingsCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided settings:", err)
		os.Exit(1)
	}
}

func runSettingsCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: freesided settings show|set [flags]")
	}
	switch args[0] {
	case "show":
		return runSettingsShowCommand(ctx, args, stdout, stderr)
	case "set":
		return runSettingsSetCommand(ctx, args, stdout, stderr)
	}
	return errors.New("unknown settings command (want show or set)")
}

// openSettingsStore opens the store named by -db, or returns the running
// daemon's control client, which the caller forwards its args to.
func openSettingsStore(ctx context.Context, dbPath string) (*commandStoreHandle, *controlClient, error) {
	if dbPath == "" {
		return nil, nil, errors.New("-db is required")
	}
	handle, client, err := openCommandStore(ctx, dbPath, store.Options{}, storeExisting)
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	return handle, client, nil
}

func parseSettingsFlags(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	return nil
}

// runSettingsShowCommand prints the instance record, or one project's with
// -project: the head revision and, for each set section, its value, who set
// it, and when. It prints what is stored, which is not a grant; the run
// settings loader re-checks it at start.
func runSettingsShowCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("freesided settings show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "existing SQLite database path (required)")
	project := flags.String("project", "", "project id whose settings to show; empty shows the instance settings")
	if err := parseSettingsFlags(flags, args); err != nil {
		return err
	}
	handle, client, err := openSettingsStore(ctx, *dbPath)
	if err != nil {
		return err
	}
	if client != nil {
		return callCommand(ctx, client, settingsRoute, args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	var output any
	if err := handle.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if *project == "" {
			output, err = tx.InstanceSettingsRecord(ctx)
		} else {
			output, err = tx.ProjectSettingsRecord(ctx, domain.ProjectID(*project))
		}
		return err
	}); err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}

// settingsSetResult is the revision a set recorded and when it applies.
type settingsSetResult struct {
	Revision    any    `json:"revision"`
	TakesEffect string `json:"takes_effect"`
}

// runSettingsSetCommand records one revision of one section: -value sets it
// to a JSON value, -clear clears an optional section. The value passes the
// section's rules and the settings gate against current policy before the
// store appends it; a project's repository must also agree with the
// project's projects binding.
func runSettingsSetCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("freesided settings set", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "existing SQLite database path (required)")
	project := flags.String("project", "", "project id whose settings to set; empty sets the instance settings")
	sectionName := flags.String("section", "", "section to set (required)")
	value := flags.String("value", "", "the section's new value as JSON")
	clear := flags.Bool("clear", false, "clear an optional section instead of setting it")
	if err := parseSettingsFlags(flags, args); err != nil {
		return err
	}
	if *sectionName == "" {
		return errors.New("-section is required")
	}
	if (*value == "") == !*clear {
		return errors.New("pass exactly one of -value and -clear")
	}
	var raw json.RawMessage
	if !*clear {
		raw = json.RawMessage(*value)
	}
	write, err := settingsSetWrite(domain.ProjectID(*project), *sectionName, raw)
	if err != nil {
		return err
	}
	handle, client, err := openSettingsStore(ctx, *dbPath)
	if err != nil {
		return err
	}
	if client != nil {
		return callCommand(ctx, client, settingsRoute, args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	setBy, err := settingsSetBy()
	if err != nil {
		return err
	}
	var revision any
	if err := handle.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		revision, err = write(ctx, tx, setBy, time.Now().UTC())
		return err
	}); err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(settingsSetResult{Revision: revision, TakesEffect: settingsTakeEffect}); err != nil {
		return fmt.Errorf("write settings revision: %w", err)
	}
	return nil
}

type settingsWrite func(ctx context.Context, tx *store.InternalTx, setBy string, at time.Time) (any, error)

// settingsSetWrite checks a set before any store opens, so a refused value
// is refused the same way whether or not the daemon runs: the section's
// name, its value against the section's rules, and the settings gate. A
// clear is checked by the store's append, which refuses a required section.
func settingsSetWrite(project domain.ProjectID, name string, value json.RawMessage) (settingsWrite, error) {
	cleared := value == nil
	if project == "" {
		section, err := domain.ParseInstanceSettingsSection(name)
		if err != nil {
			return nil, err
		}
		if !cleared {
			settings, _, err := domain.DecodeInstanceSection(section, value)
			if err != nil {
				return nil, err
			}
			if err := daemoncompose.CheckInstanceSettingsSection(settings, section); err != nil {
				return nil, err
			}
		}
		return func(ctx context.Context, tx *store.InternalTx, setBy string, at time.Time) (any, error) {
			return tx.AppendInstanceSettingsRevision(ctx, domain.InstanceSettingsInput{
				Section: section, Value: value, SetBy: setBy,
			}, at)
		}, nil
	}
	section, err := domain.ParseProjectSettingsSection(name)
	if err != nil {
		return nil, err
	}
	if !cleared {
		settings, _, err := domain.DecodeProjectSection(section, value)
		if err != nil {
			return nil, err
		}
		if err := daemoncompose.CheckProjectSettingsSection(settings, section); err != nil {
			return nil, err
		}
	}
	return func(ctx context.Context, tx *store.InternalTx, setBy string, at time.Time) (any, error) {
		return tx.AppendProjectSettingsRevision(ctx, domain.ProjectSettingsInput{
			ProjectID: project, Section: section, Value: value, SetBy: setBy,
		}, at)
	}, nil
}

// settingsSetBy is who an operator command records as setting a value.
func settingsSetBy() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("current account: %w", err)
	}
	return "cli:" + account.Username, nil
}
