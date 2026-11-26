package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// settingsRoute carries every settings verb: the args it forwards name the
// verb, so the running daemon runs the same dispatch.
const settingsRoute = "/settings"

// settingsTakeEffect is when a section the installed daemon composes from at
// start applies: every instance section but the cordoned launch cap, and
// the project's agent_image and enabled.
const settingsTakeEffect = "at the daemon's next start"

// nextAdmissionTakeEffect is when a section the admitting transaction reads
// applies: the cordoned launch cap (#3471) and the project's writer class,
// which each admission resolves from the project's current record (#3209).
const nextAdmissionTakeEffect = "at the next admission"

// nextGateTakeEffect is when a section the path, publish, recipe, and
// hand-back gates read per task applies (#3209).
const nextGateTakeEffect = "at each task's next gate"

// repositoryTakeEffect is nextGateTakeEffect for the repository, whose tip
// watch and readiness still follow the composed project until a restart, so
// until then an edit holds new admissions with a repository mismatch.
const repositoryTakeEffect = nextGateTakeEffect + "; the tip watch and readiness follow at the daemon's next start"

// recipeTakeEffect is nextGateTakeEffect for the approved recipe. Readiness
// and the writer-image check still read the composed project's recipe until
// a restart, so until then new work still starts on the old recipe's image,
// and the recipe gate blocks it at publication.
const recipeTakeEffect = nextGateTakeEffect + "; work on an image built from the old recipe blocks at publication, " +
	"and readiness follows at the daemon's next start"

// settingsSetTakesEffect is when a set of the named section applies.
func settingsSetTakesEffect(project domain.ProjectID, section string) string {
	if project == "" {
		if section == string(domain.InstanceSectionCordonedLaunches) {
			return nextAdmissionTakeEffect
		}
		return settingsTakeEffect
	}
	return projectSectionTakesEffect(domain.ProjectSettingsSection(section))
}

// projectSectionTakesEffect is when a change to a project section applies.
func projectSectionTakesEffect(section domain.ProjectSettingsSection) string {
	switch section {
	case domain.ProjectSectionAllowedPaths, domain.ProjectSectionHandBackLabel:
		return nextGateTakeEffect
	case domain.ProjectSectionRepository:
		return repositoryTakeEffect
	case domain.ProjectSectionApprovedRecipe:
		return recipeTakeEffect
	case domain.ProjectSectionWriterClass:
		return nextAdmissionTakeEffect
	case domain.ProjectSectionAgentImage, domain.ProjectSectionEnabled:
		return settingsTakeEffect
	}
	// An unknown section is refused before its revision is written.
	return settingsTakeEffect
}

// runSettingsMain is the stored run settings verb (issue #2082): `settings
// show` prints a scope's record, `settings set` records a revision of one
// section, and `settings import` turns a deputy settings file into the
// records once.
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
		return errors.New("usage: freesided settings show|set|import [flags]")
	}
	switch args[0] {
	case "show":
		return runSettingsShowCommand(ctx, args, stdout, stderr)
	case "set":
		return runSettingsSetCommand(ctx, args, stdout, stderr)
	case "import":
		return runSettingsImportCommand(ctx, args, stdout, stderr)
	}
	return errors.New("unknown settings command (want show, set, or import)")
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
	result := settingsSetResult{Revision: revision, TakesEffect: settingsSetTakesEffect(domain.ProjectID(*project), *sectionName)}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
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
		if section == domain.InstanceSectionExecutionPause {
			// The pause is not run configuration: it is written only with
			// the card that closes admission, and cleared only with that
			// card's conclusion, so no settings edit can split them.
			return nil, errors.New("settings set does not write execution_pause; " +
				"use freesided execution pause and freesided execution resume")
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

// settingsImportResult reports the revisions an import wrote and the file's
// keys that have no stored section.
type settingsImportResult struct {
	InstanceRevision int                   `json:"instance_revision"`
	Project          domain.ProjectID      `json:"project"`
	ProjectRevision  int                   `json:"project_revision"`
	NotImported      []string              `json:"not_imported"`
	TakesEffect      settingsImportEffects `json:"takes_effect"`
}

// settingsImportEffects is when an import's sections apply: every instance
// section the file carries at the daemon's next start, and each project
// section as a set of it would.
type settingsImportEffects struct {
	Instance string                  `json:"instance"`
	Project  []settingsSectionEffect `json:"project"`
}

type settingsSectionEffect struct {
	Section     domain.ProjectSettingsSection `json:"section"`
	TakesEffect string                        `json:"takes_effect"`
}

// importEffects lists, in section order, when each project section project
// sets applies.
func importEffects(project domain.ProjectSettings) (settingsImportEffects, error) {
	effects := settingsImportEffects{Instance: settingsTakeEffect, Project: []settingsSectionEffect{}}
	for _, section := range domain.AllProjectSettingsSections {
		value, err := project.SectionValue(section)
		if err != nil {
			return settingsImportEffects{}, err
		}
		if value != nil {
			effects.Project = append(effects.Project, settingsSectionEffect{
				Section: section, TakesEffect: projectSectionTakesEffect(section),
			})
		}
	}
	return effects, nil
}

// runSettingsImportCommand turns a deputy settings file into the instance
// record and its project's record, one revision per set section, in one
// transaction. It refuses when either scope already has a revision, so an
// import never mixes with values set since. The file's container tool is a
// bare name, so the import needs it as an absolute path: -container-bin, or
// the PATH lookup of the name the deputy runs. The lookup runs where the
// operator runs the command, and a running daemon gets its result.
func runSettingsImportCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("freesided settings import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "existing SQLite database path (required)")
	settingsPath := flags.String("settings", "", "absolute path of the deputy settings.json to import (required)")
	containerBin := flags.String("container-bin", "",
		"absolute path of the container tool; empty looks up "+deputyContainerBin+" on this command's PATH")
	if err := parseSettingsFlags(flags, args); err != nil {
		return err
	}
	// A running daemon reads the file, from its own working directory.
	if !filepath.IsAbs(*settingsPath) {
		return errors.New("-settings must be an absolute path")
	}
	container, err := importContainerBin(*containerBin)
	if err != nil {
		return err
	}
	handle, client, err := openSettingsStore(ctx, *dbPath)
	if err != nil {
		return err
	}
	if client != nil {
		// The daemon's launchd PATH need not hold the tool, so it gets the
		// path this command resolved.
		if *containerBin == "" {
			args = append(slices.Clone(args), "-container-bin", container)
		}
		return callCommand(ctx, client, settingsRoute, args, stdout)
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	loaded, err := loadDeputySettings(*settingsPath)
	if err != nil {
		return err
	}
	run := loaded.runSettings(container)
	if err := daemoncompose.CheckInstanceSettings(run.Instance); err != nil {
		return fmt.Errorf("instance settings: %w", err)
	}
	if err := daemoncompose.CheckProjectSettings(run.Project); err != nil {
		return fmt.Errorf("project %q settings: %w", run.ProjectID, err)
	}
	setBy, err := settingsSetBy()
	if err != nil {
		return err
	}
	effects, err := importEffects(run.Project)
	if err != nil {
		return err
	}
	result := settingsImportResult{
		Project: run.ProjectID, NotImported: loaded.keysNotImported(), TakesEffect: effects,
	}
	if err := handle.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		result.InstanceRevision, result.ProjectRevision, err = importRunSettings(ctx, tx, run, setBy, time.Now().UTC())
		return err
	}); err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write settings import: %w", err)
	}
	return nil
}

// importContainerBin is the container tool path an import stores: the
// operator's, or the PATH lookup of the deputy's bare name. Either must be
// absolute, because the installed daemon's launchd PATH need not hold it.
func importContainerBin(flagValue string) (string, error) {
	path := flagValue
	if path == "" {
		found, err := exec.LookPath(deputyContainerBin)
		if err != nil {
			return "", fmt.Errorf("container tool %q is not on PATH; pass -container-bin <absolute path>", deputyContainerBin)
		}
		path = found
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("container tool %q is not an absolute path; pass -container-bin <absolute path>", path)
	}
	return path, nil
}

// importRunSettings appends one revision per set section of both scopes,
// refusing when either already has one.
func importRunSettings(
	ctx context.Context, tx *store.InternalTx, run daemoncompose.RunSettings, setBy string, at time.Time,
) (instanceRevision, projectRevision int, err error) {
	instanceRevisions, err := tx.InstanceSettingsRevisions(ctx)
	if err != nil {
		return 0, 0, err
	}
	// An execution pause, a cordoned launch cap, or the execution switch,
	// set or cleared, is not configuration the deputy's file carries, so a
	// store that only ever held them still takes an import. The file
	// carries no switch either, so an import never turns execution on.
	for _, revision := range instanceRevisions {
		if revision.Section != domain.InstanceSectionExecutionPause &&
			revision.Section != domain.InstanceSectionCordonedLaunches &&
			revision.Section != domain.InstanceSectionExecution {
			return 0, 0, fmt.Errorf("instance settings already have revision %d; set sections with settings set",
				instanceRevisions[len(instanceRevisions)-1].Revision)
		}
	}
	projectRevisions, err := tx.ProjectSettingsRevisions(ctx, run.ProjectID)
	if err != nil {
		return 0, 0, err
	}
	for _, revision := range projectRevisions {
		if revision.Section != domain.ProjectSectionEnabled {
			return 0, 0, fmt.Errorf("project %q settings already have revision %d; set sections with settings set",
				run.ProjectID, projectRevisions[len(projectRevisions)-1].Revision)
		}
	}
	for _, section := range domain.AllInstanceSettingsSections {
		value, err := run.Instance.SectionValue(section)
		if err != nil {
			return 0, 0, err
		}
		if value == nil {
			continue
		}
		revision, err := tx.AppendInstanceSettingsRevision(ctx, domain.InstanceSettingsInput{
			Section: section, Value: value, SetBy: setBy,
		}, at)
		if err != nil {
			return 0, 0, err
		}
		instanceRevision = revision.Revision
	}
	for _, section := range domain.AllProjectSettingsSections {
		value, err := run.Project.SectionValue(section)
		if err != nil {
			return 0, 0, err
		}
		if value == nil {
			continue
		}
		revision, err := tx.AppendProjectSettingsRevision(ctx, domain.ProjectSettingsInput{
			ProjectID: run.ProjectID, Section: section, Value: value, SetBy: setBy,
		}, at)
		if err != nil {
			return 0, 0, err
		}
		projectRevision = revision.Revision
	}
	return instanceRevision, projectRevision, nil
}
