package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const (
	// prelaunchRecordName sits beside the composition manifest. Nothing reads
	// it; it is the operator's evidence that the root was ready before spend.
	prelaunchRecordName    = "prelaunch.json"
	prelaunchRecordVersion = "freeside-prelaunch-v1"
	prelaunchOwnerNote     = "freesided prelaunch"
	// prelaunchAcquisitionName holds this command's rig token while it runs.
	// It is not rig-acquisition.json, so a later submit or prod-authority
	// check never reads a token this command released.
	prelaunchAcquisitionName = "prelaunch-rig-acquisition.json"
	// prelaunchSubmissionID stands in for the identity a submission gets.
	// Validation needs one to apply a new submission's rules; nothing is
	// saved under it.
	prelaunchSubmissionID = "prelaunch"
	prelaunchFirstStart   = "start the deputy once on this root to adopt the agent tree, then review and commit " +
		"the patch it emits and set agent_tree.commit to that commit; see docs/production-walkthrough.md, The First Start"
)

var (
	errPrelaunchUsage  = errors.New("usage")
	errPrelaunchFailed = errors.New("a prelaunch check failed")
)

func runPrelaunchMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runPrelaunchCommand(ctx, args, os.Stdout, os.Stderr, productionPrelaunchHost()); err != nil {
		fmt.Fprintln(os.Stderr, "freesided prelaunch:", err)
		if errors.Is(err, errPrelaunchUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// prelaunchGroup is what a check covers. The zero value "" is invalid by
// design.
type prelaunchGroup string

const (
	prelaunchGroupSettings    prelaunchGroup = "settings"
	prelaunchGroupImages      prelaunchGroup = "images"
	prelaunchGroupInputs      prelaunchGroup = "inputs"
	prelaunchGroupAgentTree   prelaunchGroup = "agent_tree"
	prelaunchGroupState       prelaunchGroup = "state"
	prelaunchGroupRig         prelaunchGroup = "rig"
	prelaunchGroupComposition prelaunchGroup = "composition"
	prelaunchGroupLive        prelaunchGroup = "live"
)

// AllPrelaunchGroups is the single registration point for prelaunchGroup,
// in the order the checks run.
var AllPrelaunchGroups = []prelaunchGroup{
	prelaunchGroupSettings, prelaunchGroupImages, prelaunchGroupAgentTree, prelaunchGroupInputs, prelaunchGroupState,
	prelaunchGroupRig, prelaunchGroupComposition, prelaunchGroupLive,
}

func (g prelaunchGroup) valid() bool {
	switch g {
	case prelaunchGroupSettings, prelaunchGroupImages, prelaunchGroupInputs, prelaunchGroupAgentTree, prelaunchGroupState,
		prelaunchGroupRig, prelaunchGroupComposition, prelaunchGroupLive:
		return true
	default:
		return false
	}
}

// prelaunchPrerequisite is one onboarding step a launch needs, in the order
// the operator takes them. The zero value "" is invalid by design.
type prelaunchPrerequisite string

const (
	prelaunchNeedsAgentTree      prelaunchPrerequisite = "agent_tree"
	prelaunchNeedsTrustProfile   prelaunchPrerequisite = "trust_profile"
	prelaunchNeedsProjectImage   prelaunchPrerequisite = "project_image"
	prelaunchNeedsApprovedRecipe prelaunchPrerequisite = "approved_recipe"
)

// AllPrelaunchPrerequisites is the single registration point for
// prelaunchPrerequisite, in onboarding order.
var AllPrelaunchPrerequisites = []prelaunchPrerequisite{
	prelaunchNeedsAgentTree, prelaunchNeedsTrustProfile, prelaunchNeedsProjectImage, prelaunchNeedsApprovedRecipe,
}

func (p prelaunchPrerequisite) valid() bool {
	switch p {
	case prelaunchNeedsAgentTree, prelaunchNeedsTrustProfile, prelaunchNeedsProjectImage, prelaunchNeedsApprovedRecipe:
		return true
	default:
		return false
	}
}

// prelaunchRecord is prelaunch.json. ManifestDigest is null when the
// composition did not run; LineupProbe is null when the probe did not run.
type prelaunchRecord struct {
	Version        string                `json:"version"`
	SettingsDigest *domain.Digest        `json:"settings_digest"`
	DaemonBuild    string                `json:"daemon_build"`
	CheckedAt      time.Time             `json:"checked_at"`
	Passed         bool                  `json:"passed"`
	ManifestDigest *domain.Digest        `json:"manifest_digest"`
	FirstMissing   *prelaunchMissing     `json:"first_missing"`
	Checks         []prelaunchCheck      `json:"checks"`
	HealthItems    []prelaunchHealthItem `json:"health_items"`
	LineupProbe    *lineupProbeRecord    `json:"lineup_probe"`
}

// prelaunchCheck is one check. Evidence says what was seen, or why the
// check did not run; Fix is what the operator runs when it failed.
type prelaunchCheck struct {
	Group    prelaunchGroup    `json:"group"`
	Name     string            `json:"name"`
	Status   compositionStatus `json:"status"`
	Evidence string            `json:"evidence"`
	Fix      string            `json:"fix,omitempty"`
}

type prelaunchMissing struct {
	Prerequisite prelaunchPrerequisite `json:"prerequisite"`
	Evidence     string                `json:"evidence"`
	Fix          string                `json:"fix"`
}

// prelaunchHealthItem is an open system_health item that holds unattended
// admission. OperatorStop is the read's: whether an operator stop is in
// force as well.
type prelaunchHealthItem struct {
	ID                domain.ItemID   `json:"id"`
	RequestedDecision []domain.Action `json:"requested_decision"`
	OperatorStop      bool            `json:"operator_stop"`
}

func (r *prelaunchRecord) add(group prelaunchGroup, name string, status compositionStatus, evidence, fix string) {
	r.Checks = append(r.Checks, prelaunchCheck{Group: group, Name: name, Status: status, Evidence: evidence, Fix: fix})
}

func (r *prelaunchRecord) pass(group prelaunchGroup, name, evidence string) {
	r.add(group, name, compositionPassed, evidence, "")
}

func (r *prelaunchRecord) fail(group prelaunchGroup, name, evidence, fix string) {
	r.add(group, name, compositionFailed, evidence, fix)
}

func (r *prelaunchRecord) skip(group prelaunchGroup, name, why string) {
	r.add(group, name, compositionNotRun, why, "")
}

func (r *prelaunchRecord) status(name string) compositionStatus {
	for _, check := range r.Checks {
		if check.Name == name && check.Group != prelaunchGroupComposition {
			return check.Status
		}
	}
	return ""
}

// prelaunchHost is what the command touches outside its root. Tests replace
// each part; production uses the deputy's own.
type prelaunchHost struct {
	rig       rigHost
	preflight preflightEnvironment
	leaseRoot string
	now       func() time.Time
	// build is the daemon build the record and the composition name.
	build string
	// preLease runs the deputy's checks before its lease.
	preLease func(loadedDeputySettings, deputyPaths) (deputyHomes, error)
	// prodAuthority checks prod's App authority under the held lease.
	prodAuthority func(ctx context.Context, homes deputyHomes, settings loadedDeputySettings, tokenFile, leaseRoot string) error
	prober        func(settings *loadedDeputySettings, stateRoot, tokenFile string) (lineupProber, error)
	// inspectImage checks that the local container store holds a pinned
	// reference, without the lease.
	inspectImage func(ctx context.Context, ref string) error
}

func productionPrelaunchHost() prelaunchHost {
	return prelaunchHost{
		rig:       newProductionRigHost(),
		preflight: newProductionPreflightEnvironment(),
		now:       func() time.Time { return time.Now().UTC() },
		build:     buildVersion(),
		preLease: func(settings loadedDeputySettings, paths deputyPaths) (deputyHomes, error) {
			return deputyPreLeaseChecks(settings, paths, readTailscaleIPs)
		},
		prodAuthority: func(
			ctx context.Context, homes deputyHomes, settings loadedDeputySettings, tokenFile, leaseRoot string,
		) error {
			return checkProdAppAuthority(ctx, deputyEnvironment, homes.home, homes.account, tokenFile, leaseRoot,
				settings.Prod.InstalledDaemon)
		},
		prober: func(settings *loadedDeputySettings, stateRoot, tokenFile string) (lineupProber, error) {
			return newProductionLineupProber(settings, stateRoot, tokenFile, deputyContainerBin)
		},
		inspectImage: func(ctx context.Context, ref string) error {
			return checkImageInStore(ctx, deputyContainerBin, ref)
		},
	}
}

type prelaunchConfig struct {
	Root, TaskPath, PolicyPath, PublicationPath, WorkUnitPath string
	TaskLines                                                 taskLineFlag
	NoLive                                                    bool
}

func parsePrelaunchConfig(args []string, stderr io.Writer) (prelaunchConfig, error) {
	flags := flag.NewFlagSet("freesided prelaunch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var cfg prelaunchConfig
	flags.StringVar(&cfg.Root, "root", "", "standing deputy root; settings are read from <root>/daemon/settings.json (required)")
	flags.StringVar(&cfg.TaskPath, "task", "", "the task file the run will submit (required)")
	flags.StringVar(&cfg.PolicyPath, "policy", "", "the resolved policy file the run will submit (required)")
	flags.StringVar(&cfg.PublicationPath, "publication", "", "the publication file the run will submit (required)")
	flags.StringVar(&cfg.WorkUnitPath, "work-unit", "", "the work-unit declaration the run will submit")
	flags.Var(&cfg.TaskLines, "task-line", "a task line the run will submit, as role=agent (repeatable)")
	flags.BoolVar(&cfg.NoLive, "no-live", false, "skip the lineup probe")
	if err := flags.Parse(args); err != nil {
		return prelaunchConfig{}, fmt.Errorf("%w: %w", errPrelaunchUsage, err)
	}
	switch {
	case flags.NArg() != 0:
		return prelaunchConfig{}, fmt.Errorf("%w: unexpected arguments: %v", errPrelaunchUsage, flags.Args())
	case cfg.Root == "" || !filepath.IsAbs(cfg.Root):
		return prelaunchConfig{}, fmt.Errorf("%w: -root must name an absolute directory", errPrelaunchUsage)
	case cfg.TaskPath == "" || cfg.PolicyPath == "" || cfg.PublicationPath == "":
		return prelaunchConfig{}, fmt.Errorf("%w: -task, -policy, and -publication are required", errPrelaunchUsage)
	}
	return cfg, nil
}

// runPrelaunchCommand runs every check a deputy start and the run's submit
// would make that spends nothing: settings, the pinned images in the local
// container store, submit inputs, the agent tree, the state database, the
// rig lease, prod's authority, and the launch composition, and with live on
// the lineup probe. It writes prelaunch.json beside the composition manifest
// and prints the same bytes. The only store write is the mint audit of the
// token the composition mints; the composition manifest is neither written
// nor removed.
func runPrelaunchCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer, host prelaunchHost,
) error {
	cfg, err := parsePrelaunchConfig(args, stderr)
	if err != nil {
		return err
	}
	stateRoot, err := filepath.EvalSymlinks(filepath.Join(cfg.Root, "daemon"))
	if err != nil {
		return fmt.Errorf("%w: resolve state root: %w", errPrelaunchUsage, err)
	}
	paths := newDeputyPaths(stateRoot)
	record := evaluatePrelaunch(ctx, cfg, paths, host)
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := atomicfile.WriteFile(filepath.Join(stateRoot, prelaunchRecordName), body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", prelaunchRecordName, err)
	}
	if _, err := stdout.Write(body); err != nil {
		return err
	}
	if record.Passed {
		return nil
	}
	for _, check := range record.Checks {
		if check.Status == compositionFailed {
			_, _ = fmt.Fprintf(stderr, "%s %s: %s; fix: %s\n", check.Group, check.Name, check.Evidence, check.Fix)
		}
	}
	if missing := record.FirstMissing; missing != nil {
		_, _ = fmt.Fprintf(stderr, "first missing: %s: %s; fix: %s\n", missing.Prerequisite, missing.Evidence, missing.Fix)
	}
	return fmt.Errorf("%w; see %s", errPrelaunchFailed, filepath.Join(stateRoot, prelaunchRecordName))
}

// prelaunchInputs is what the submission checks bind for the checks that
// compare the inputs with the store and with the composition.
type prelaunchInputs struct {
	identity compositionIdentity
	policy   domain.ResolvedPolicy
	// source is the specification artifact submit would register; a task
	// already holding it refuses the submission.
	source domain.ArtifactID
}

// prelaunchState is what the store reads found, for the walk. trustRead and
// projectImageRead say the read reached them, so a failed read is never
// reported as a missing prerequisite.
type prelaunchState struct {
	present          bool
	read             bool
	trustRead        bool
	trustProfile     bool
	projectImage     *domain.ProjectImage
	projectImageRead bool
}

func evaluatePrelaunch(ctx context.Context, cfg prelaunchConfig, paths deputyPaths, host prelaunchHost) prelaunchRecord {
	now := host.now()
	record := prelaunchRecord{
		Version: prelaunchRecordVersion, DaemonBuild: host.build, CheckedAt: now,
		Checks: []prelaunchCheck{}, HealthItems: []prelaunchHealthItem{},
	}
	settings, err := loadDeputySettings(paths.Settings)
	if err != nil {
		record.fail(prelaunchGroupSettings, "settings", err.Error(), "correct "+paths.Settings)
		for _, image := range prelaunchImages(loadedDeputySettings{}) {
			record.skip(prelaunchGroupImages, image.check, "the settings must load first")
		}
		record.skip(prelaunchGroupAgentTree, "agent_tree", "the settings must load first")
		for _, name := range []string{"submission", "task_lines", "duplicate_source"} {
			record.skip(prelaunchGroupInputs, name, "the settings must load first")
		}
		for _, name := range []string{"state_database", "system_health", "identities"} {
			record.skip(prelaunchGroupState, name, "the settings must load first")
		}
		skipLeaseBound(&record, "the settings must load first", cfg.NoLive)
		return record
	}
	digest := settings.Digest
	record.SettingsDigest = &digest
	homes, preLeaseErr := host.preLease(settings, paths)
	if preLeaseErr != nil {
		record.fail(prelaunchGroupSettings, "settings", preLeaseErr.Error(), "correct "+paths.Settings+
			" or the files and paths it names")
	} else {
		record.pass(prelaunchGroupSettings, "settings", "settings "+string(settings.Digest)+
			" pass the deputy's checks before its lease")
	}
	checkPrelaunchImages(ctx, &record, settings, host)

	tree, treeMissing, treeErr := checkPrelaunchAgentTree(ctx, &record, settings, paths)
	inputs := checkPrelaunchInputs(ctx, &record, cfg, settings)
	checkPrelaunchTaskLines(&record, cfg.TaskLines, tree, treeErr)
	state := checkPrelaunchState(ctx, &record, settings, paths, inputs)

	var manifest *compositionManifest
	if preLeaseErr != nil {
		skipLeaseBound(&record, "the settings must pass the deputy's checks first", cfg.NoLive)
	} else {
		manifest = runPrelaunchUnderLease(ctx, &record, cfg, settings, paths, homes, inputs, host, now)
	}
	record.FirstMissing = firstPrelaunchMissing(record, treeMissing, settings, paths, state, manifest)
	record.Passed = record.FirstMissing == nil && !slices.ContainsFunc(record.Checks, func(c prelaunchCheck) bool {
		return c.Status == compositionFailed
	})
	return record
}

// prelaunchImage is one pinned image a run creates containers from. Check
// names match the composition's image checks.
type prelaunchImage struct{ check, key, ref string }

func prelaunchImages(settings loadedDeputySettings) []prelaunchImage {
	return []prelaunchImage{
		{"exporter_image", "images.exporter", settings.Images.Exporter},
		{"implementer_image", "images.agent", settings.Images.Agent},
		{"reviewer_image", "images.review", settings.Images.Review},
	}
}

// checkPrelaunchImages checks that the local container store holds each
// pinned image under its digest. It runs before the lease and with -no-live,
// because auth adopt creates the first container from the exporter image
// before the composition inspects any of them.
func checkPrelaunchImages(ctx context.Context, record *prelaunchRecord, settings loadedDeputySettings, host prelaunchHost) {
	for _, image := range prelaunchImages(settings) {
		if err := host.inspectImage(ctx, image.ref); err != nil {
			evidence, fix := imageStoreFailure(image.ref, err)
			record.fail(prelaunchGroupImages, image.check, image.key+" "+evidence+": "+err.Error(), fix)
			continue
		}
		record.pass(prelaunchGroupImages, image.check, image.key+" "+image.ref+" is in the local container store")
	}
}

func skipLeaseBound(record *prelaunchRecord, why string, noLive bool) {
	record.skip(prelaunchGroupRig, "rig_lease", why)
	skipUnderLease(record, why, noLive)
}

// skipUnderLease records the checks that run only under a held lease as not
// run.
func skipUnderLease(record *prelaunchRecord, why string, noLive bool) {
	record.skip(prelaunchGroupRig, "installed_daemon", why)
	record.skip(prelaunchGroupComposition, "composition", why)
	record.skip(prelaunchGroupInputs, "launch_binding", why)
	if noLive {
		why = "-no-live"
	}
	record.skip(prelaunchGroupLive, "lineup_probe", why)
}

// checkPrelaunchInputs applies the refusals submit makes to a new submission
// before it opens the store, and returns what the later checks bind, or nil
// when the inputs fail. Validation runs as preflight's per-submission identity check does, under a
// placeholder identity and with no database, so no saved journal can exempt
// the inputs from a rule.
func checkPrelaunchInputs(
	ctx context.Context, record *prelaunchRecord, cfg prelaunchConfig, settings loadedDeputySettings,
) *prelaunchInputs {
	fix := "correct the task, policy, publication, or work-unit file before submission"
	if _, err := domain.CanonicalTaskLineChoices(cfg.TaskLines); err != nil {
		record.fail(prelaunchGroupInputs, "submission", "--task-line: "+err.Error(), "name each role once, as role=agent")
		return nil
	}
	identity, err := inspectCompositionIdentity(ctx, preflightConfig{
		SubmissionID: prelaunchSubmissionID,
		TaskPath:     cfg.TaskPath, PolicyPath: cfg.PolicyPath,
		PublicationPath: cfg.PublicationPath, WorkUnitPath: cfg.WorkUnitPath,
		ProjectID: settings.Repository.Project, AllowedPaths: settings.Repository.AllowedPaths,
	})
	var policy domain.ResolvedPolicy
	if err == nil {
		policy, err = checkSubmittedPolicy(cfg.PolicyPath, identity.ImplementationRunID)
	}
	var source domain.Artifact
	if err == nil {
		source, err = prelaunchSourceArtifact(cfg.TaskPath, identity.SourceDigest)
	}
	if err != nil {
		record.fail(prelaunchGroupInputs, "submission", err.Error(), fix)
		return nil
	}
	record.pass(prelaunchGroupInputs, "submission", "the inputs pass submit's checks for a new submission to project "+
		string(settings.Repository.Project))
	return &prelaunchInputs{identity: identity, policy: policy, source: source.ID}
}

// prelaunchSourceArtifact is the specification artifact submit registers for
// the task file.
func prelaunchSourceArtifact(taskPath string, digest domain.Digest) (domain.Artifact, error) {
	task, err := readSubmissionFile(taskPath)
	if err != nil {
		return domain.Artifact{}, err
	}
	if task.digest != digest {
		return domain.Artifact{}, errors.New("the task file changed while it was checked")
	}
	return engine.SubmissionArtifact(
		domain.ArtifactKindSpecification, task.digest, domain.EvidenceMediaTextMarkdown, int64(len(task.body)))
}

// checkSubmittedPolicy is the rest of what submit checks of the policy before
// it saves anything: the launch sizes and the specification policy. It
// returns the policy as submit resolves it.
func checkSubmittedPolicy(policyPath string, runID domain.RunID) (domain.ResolvedPolicy, error) {
	policyFile, err := readSubmissionFile(policyPath)
	if err != nil {
		return domain.ResolvedPolicy{}, err
	}
	var keys []domain.PolicyKey
	if err := json.Unmarshal(policyFile.body, &keys); err != nil {
		return domain.ResolvedPolicy{}, err
	}
	specificationRunID, err := engine.SpecificationRunIDForImplementation(runID)
	if err != nil {
		return domain.ResolvedPolicy{}, err
	}
	policy, err := domain.NewResolvedPolicy(specificationRunID, keys)
	if err != nil {
		return domain.ResolvedPolicy{}, err
	}
	if _, err := ward.ResolveLaunchSizes(policy.Keys); err != nil {
		return domain.ResolvedPolicy{}, err
	}
	if _, err := specify.ParsePolicy(policy); err != nil {
		return domain.ResolvedPolicy{}, fmt.Errorf("validate specification policy: %w", err)
	}
	return policy, nil
}

// checkPrelaunchAgentTree reads the agent tree at agent_tree.commit, and when
// an earlier deputy start left its tree patch on the root, compares the two
// as the deputy's start does. missing reports a tree the commit doesn't hold
// or that differs from the patch, the walk's first step; a local file error
// fails the check without naming the tree missing.
func checkPrelaunchAgentTree(
	ctx context.Context, record *prelaunchRecord, settings loadedDeputySettings, paths deputyPaths,
) (agenttree.Tree, bool, error) {
	checkout, commit := settings.AgentTree.Checkout, settings.AgentTree.Commit
	failed := func(err error, evidence string) (agenttree.Tree, bool, error) {
		record.fail(prelaunchGroupAgentTree, "agent_tree", evidence, prelaunchFirstStart)
		return agenttree.Tree{}, true, err
	}
	unreadable := func(err error, evidence string) (agenttree.Tree, bool, error) {
		record.fail(prelaunchGroupAgentTree, "agent_tree", evidence,
			"make agent_tree.checkout and "+paths.StateRoot+" readable, then rerun prelaunch")
		return agenttree.Tree{}, false, err
	}
	scratch, err := os.MkdirTemp("", "freesided-prelaunch-tree-")
	if err != nil {
		return unreadable(err, "could not create a scratch directory: "+err.Error())
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	files, err := agenttree.ReadCommit(ctx, checkout, filepath.Join(scratch, "read"), commit)
	if err != nil {
		return failed(err, fmt.Sprintf("agent_tree.commit %s does not hold a readable agent tree in %s: %v", commit, checkout, err))
	}
	tree, err := agenttree.Parse(files)
	if err != nil {
		return failed(err, fmt.Sprintf("the agent tree at %s does not parse: %v", commit, err))
	}
	revision, err := agenttree.Revision(files)
	if err != nil {
		return failed(err, fmt.Sprintf("the agent tree at %s has no revision: %v", commit, err))
	}
	if info, err := os.Stat(paths.Patch); err == nil {
		matches, err := agentTreeMatches(ctx, paths.Patch, checkout, commit, filepath.Join(scratch, "compare"))
		switch {
		case err != nil:
			return unreadable(err, fmt.Sprintf("could not compare %s with %s: %v", paths.Patch, commit, err))
		case !matches:
			err := errors.New("agent tree differs from the emitted patch")
			return failed(err, fmt.Sprintf("the agent tree at %s is not the tree in %s, which a deputy start wrote at %s; "+
				"if the settings changed since, the next start emits a new one", commit, paths.Patch,
				info.ModTime().UTC().Format(time.RFC3339)))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return unreadable(err, "could not read "+paths.Patch+": "+err.Error())
	}
	record.pass(prelaunchGroupAgentTree, "agent_tree", fmt.Sprintf("agent_tree.commit %s holds lineup revision %s with %d agents",
		commit, revision, len(tree.Agents)))
	return tree, false, nil
}

// checkPrelaunchTaskLines checks that each task line names an agent the
// tree holds; admission resolves the line against that tree.
func checkPrelaunchTaskLines(record *prelaunchRecord, lines []domain.TaskLineChoice, tree agenttree.Tree, treeErr error) {
	switch {
	case len(lines) == 0:
		record.pass(prelaunchGroupInputs, "task_lines", "no task lines")
		return
	case treeErr != nil:
		record.skip(prelaunchGroupInputs, "task_lines", "the agent tree must be readable first")
		return
	}
	var unknown []string
	for _, line := range lines {
		if !slices.ContainsFunc(tree.Agents, func(agent domain.AgentSource) bool { return agent.Name == line.Agent }) {
			unknown = append(unknown, string(line.Role)+"="+line.Agent)
		}
	}
	if len(unknown) != 0 {
		record.fail(prelaunchGroupInputs, "task_lines", "the agent tree has no agent for "+strings.Join(unknown, ", "),
			"name an agent the tree at agent_tree.commit holds, or adopt and commit one that does")
		return
	}
	record.pass(prelaunchGroupInputs, "task_lines", fmt.Sprintf("the tree holds the agent of each of %d task lines", len(lines)))
}

// checkPrelaunchState reads the root's database read-only, never migrating
// it, under the daemon lock so no daemon writes it meanwhile.
func checkPrelaunchState(
	ctx context.Context, record *prelaunchRecord, settings loadedDeputySettings, paths deputyPaths,
	inputs *prelaunchInputs,
) prelaunchState {
	skipRest := func(why string) {
		record.skip(prelaunchGroupState, "system_health", why)
		record.skip(prelaunchGroupState, "identities", why)
		if inputs == nil {
			why = "the inputs must pass first"
		}
		record.skip(prelaunchGroupInputs, "duplicate_source", why)
	}
	if _, err := os.Stat(paths.DB); errors.Is(err, fs.ErrNotExist) {
		record.skip(prelaunchGroupState, "state_database", "fresh root; the deputy's first start creates the database")
		skipRest("fresh root; no task holds the source")
		return prelaunchState{}
	}
	state := prelaunchState{present: true}
	lock, err := daemonlock.Acquire(paths.DB)
	if err != nil {
		record.fail(prelaunchGroupState, "state_database", "a daemon holds this root, or its lock is unreadable: "+err.Error(),
			"stop the daemon on this root; prelaunch runs before the deputy starts")
		skipRest("the database must open first")
		return state
	}
	defer func() { _ = lock.Close() }() // Only reads ran under it.
	st, err := store.OpenReadOnly(ctx, paths.DB, store.Options{
		ApprovedRecipes: map[domain.Digest]bool{settings.ApprovedRecipe: true},
	})
	if mismatch := (*store.SchemaMismatchError)(nil); errors.As(err, &mismatch) {
		record.fail(prelaunchGroupState, "state_database", err.Error(),
			"the deputy's start on this build migrates it, one-way; to recover a lease on it first, run "+
				"`freesided rig recover -state-root "+paths.StateRoot+" -confirm`: "+olderSchemaRecovery(mismatch))
		skipRest("the database must open first")
		return state
	}
	if err != nil {
		record.fail(prelaunchGroupState, "state_database", "the database could not be opened read-only: "+err.Error(),
			"restore the database, or use a fresh root")
		skipRest("the database must open first")
		return state
	}
	defer func() { _ = st.Close() }() // Read-only: nothing to flush.
	record.pass(prelaunchGroupState, "state_database", "opened read-only at this build's schema")
	state.read = true
	checkPrelaunchHealth(ctx, record, st)
	checkPrelaunchIdentities(ctx, record, st, settings, &state)
	checkPrelaunchDuplicateSource(ctx, record, st, settings.Repository.Project, inputs)
	return state
}

// checkPrelaunchDuplicateSource makes submit's refusal of new work whose
// source an unfinished task in the project already holds.
func checkPrelaunchDuplicateSource(
	ctx context.Context, record *prelaunchRecord, st *store.Store, project domain.ProjectID, inputs *prelaunchInputs,
) {
	const name = "duplicate_source"
	if inputs == nil {
		record.skip(prelaunchGroupInputs, name, "the inputs must pass first")
		return
	}
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		return refuseUnfinishedDuplicate(ctx, tx, project, inputs.source)
	})
	switch {
	case errors.Is(err, errDuplicateSource):
		record.fail(prelaunchGroupInputs, name, err.Error(), "finish or abandon that task, or submit other work")
	case err != nil:
		record.fail(prelaunchGroupInputs, name, "the store read failed: "+err.Error(), "restore the database, or use a fresh root")
	default:
		record.pass(prelaunchGroupInputs, name, "no unfinished task in project "+string(project)+" holds the source")
	}
}

func checkPrelaunchHealth(ctx context.Context, record *prelaunchRecord, st *store.Store) {
	health, err := readSystemHealthFrom(ctx, st)
	if err != nil {
		record.fail(prelaunchGroupState, "system_health", "the system-health read failed: "+err.Error(),
			"run `freesided inspect health` on this root and resolve what it names")
		return
	}
	unknown := len(health.RefusedRows)
	for _, item := range health.Items {
		switch {
		case item.Admission == nil:
			unknown++
		case *item.Admission == systemHealthHolds:
			record.HealthItems = append(record.HealthItems, prelaunchHealthItem{
				ID: item.ID, RequestedDecision: item.RequestedDecision, OperatorStop: health.OperatorStop != nil,
			})
		}
	}
	fix := "resolve it in a paired client at the pairing checkpoint"
	switch {
	case health.AdmissionError != nil:
		record.fail(prelaunchGroupState, "system_health", "admission could not be evaluated: "+health.AdmissionError.Message,
			"run `freesided inspect health` on this root")
	case len(record.HealthItems) != 0:
		ids := make([]string, 0, len(record.HealthItems))
		for _, item := range record.HealthItems {
			ids = append(ids, string(item.ID))
		}
		record.fail(prelaunchGroupState, "system_health", "open items hold unattended admission: "+strings.Join(ids, ", "), fix)
	case unknown != 0:
		record.fail(prelaunchGroupState, "system_health",
			fmt.Sprintf("%d open items or rows could not be read for admission", unknown),
			"run `freesided inspect health` on this root")
	case health.OperatorStop != nil:
		record.fail(prelaunchGroupState, "system_health", "an operator stop is in force: "+health.OperatorStop.Reason, fix)
	default:
		record.pass(prelaunchGroupState, "system_health", fmt.Sprintf(
			"no open item holds unattended admission at revision %d", health.AsOfRevision))
	}
}

// checkPrelaunchIdentities compares the recorded identities with the
// settings, and reads what the walk needs: the repository's trust profile
// and the agent image's provenance.
func checkPrelaunchIdentities(
	ctx context.Context, record *prelaunchRecord, st *store.Store, settings loadedDeputySettings, state *prelaunchState,
) {
	identities, err := deputyIdentities(settings)
	if err != nil {
		record.fail(prelaunchGroupState, "identities", err.Error(), "correct "+settings.Review.AuthSnapshot)
	}
	var differs, absent []string
	readErr := st.Read(ctx, func(tx *store.ReadTx) error {
		for _, identity := range identities {
			stored, err := tx.GetAuthIdentity(ctx, identity.ID)
			switch {
			case errors.Is(err, store.ErrNotFound):
				absent = append(absent, string(identity.ID))
			case err != nil:
				return err
			case !stored.SameFixedBindings(identity):
				differs = append(differs, string(identity.ID))
			}
		}
		profiles, err := tx.InspectLatestTrustProfiles(ctx)
		if err != nil {
			return err
		}
		state.trustRead = true
		for _, current := range profiles {
			if current.Repo == settings.Repository.Repo && current.ReconstructionError == nil &&
				current.Profile.RepositoryID == settings.Repository.RepositoryID {
				state.trustProfile = true
			}
		}
		image, found, err := tx.GetProjectImageByRef(ctx, domain.ImageRef(settings.Images.Agent))
		if err != nil {
			return err
		}
		state.projectImageRead = true
		if found {
			state.projectImage = &image
		}
		return nil
	})
	switch {
	case err != nil:
	case readErr != nil:
		record.fail(prelaunchGroupState, "identities", "the store read failed: "+readErr.Error(),
			"restore the database, or use a fresh root")
	case len(differs) != 0:
		record.fail(prelaunchGroupState, "identities", "recorded identities differ from the settings: "+strings.Join(differs, ", "),
			"set the settings' agent_tree identities back to what this root recorded, or use a fresh root")
	case len(absent) != 0:
		record.skip(prelaunchGroupState, "identities", "not yet recorded: "+strings.Join(absent, ", ")+
			"; the deputy's first start records them")
	default:
		record.pass(prelaunchGroupState, "identities", "recorded identities carry the settings' fixed bindings")
	}
}

// runPrelaunchUnderLease takes the rig lease as the deputy does, runs the
// checks bound to it, and releases it cleanly. It never recovers a stale
// lease. The App token the composition mints is minted here, so a deputy,
// the App authority's one writer during a run, can't start meanwhile.
func runPrelaunchUnderLease(
	ctx context.Context, record *prelaunchRecord, cfg prelaunchConfig, settings loadedDeputySettings,
	paths deputyPaths, homes deputyHomes, inputs *prelaunchInputs, host prelaunchHost, now time.Time,
) *compositionManifest {
	lease, err := acquirePrelaunchLease(ctx, settings, paths, host)
	if err != nil {
		record.fail(prelaunchGroupRig, "rig_lease", err.Error(), prelaunchLeaseFix(err, paths))
		skipUnderLease(record, "the rig lease could not be taken", cfg.NoLive)
		return nil
	}
	tokenFile := filepath.Join(paths.StateRoot, prelaunchAcquisitionName)
	leaseRoot := lease.Manifest().Resources.LeaseRoot
	defer func() {
		releaseErr := errors.Join(removeIfPresent(tokenFile), releasePrelaunchLease(ctx, host.rig, paths, lease))
		if releaseErr == nil {
			return
		}
		for i := range record.Checks {
			check := &record.Checks[i]
			if check.Group != prelaunchGroupRig || check.Name != "rig_lease" {
				continue
			}
			// Keep a token-write failure's evidence beside the release's.
			evidence := "the lease could not be released cleanly: " + releaseErr.Error()
			if check.Status == compositionFailed {
				evidence = check.Evidence + "; " + evidence
			}
			*check = prelaunchCheck{
				Group: prelaunchGroupRig, Name: "rig_lease", Status: compositionFailed, Evidence: evidence,
				Fix: "the deputy's start on this root recovers it and names the run as interrupted",
			}
		}
	}()
	if err := writeDeputyAcquisition(tokenFile, lease); err != nil {
		record.fail(prelaunchGroupRig, "rig_lease", "could not write the lease token: "+err.Error(), "check "+paths.StateRoot)
		skipUnderLease(record, "the lease token could not be written", cfg.NoLive)
		return nil
	}
	record.pass(prelaunchGroupRig, "rig_lease", "acquired and idle; released when the checks end")

	if err := host.prodAuthority(ctx, homes, settings, tokenFile, leaseRoot); err != nil {
		record.fail(prelaunchGroupRig, "installed_daemon", err.Error(),
			"install the current Freeside app, so prod accepts this build's App authority formats, and keep $HOME the account's home")
	} else {
		record.pass(prelaunchGroupRig, "installed_daemon", "prod's App authority is usable under the lease")
	}

	manifest := composePrelaunchManifest(ctx, record, settings, paths, lease, tokenFile, host, now)
	checkPrelaunchLaunchBinding(record, manifest, inputs, settings.Repository.Project)
	if cfg.NoLive {
		record.skip(prelaunchGroupLive, "lineup_probe", "-no-live")
	} else {
		probePrelaunchLineup(ctx, record, settings, paths, tokenFile, host, now)
	}
	return manifest
}

func acquirePrelaunchLease(
	ctx context.Context, settings loadedDeputySettings, paths deputyPaths, host prelaunchHost,
) (*daemonlock.RigLease, error) {
	acquire, err := deputyRigAcquireConfig(paths, settings, host.leaseRoot, prelaunchOwnerNote)
	if err != nil {
		return nil, err
	}
	lease, err := daemonlock.AcquireRig(acquire)
	if err != nil {
		return nil, err
	}
	if err := proveRigIdle(ctx, host.rig, lease.Manifest().Resources); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	return lease, nil
}

// prelaunchLeaseFix names the way past a lease that could not be taken. A
// stale lease is recovered by its own root's deputy, or by rig recover.
func prelaunchLeaseFix(err error, paths deputyPaths) string {
	var stale *daemonlock.RigStaleError
	var conflict *daemonlock.RigConflictError
	switch {
	case errors.As(err, &stale) && stale.Manifest.Resources.StateRoot == paths.StateRoot:
		return "this root's deputy start recovers its own stale lease; run the deputy, or `freesided rig recover -state-root " +
			paths.StateRoot + " -confirm`, then rerun prelaunch"
	case errors.As(err, &stale):
		return "recover it with `freesided rig recover -state-root " + stale.Manifest.Resources.StateRoot + " -confirm`"
	case errors.As(err, &conflict):
		return "stop the holder named here; nothing may hold the rig before a launch"
	}
	return "free the listener, seed root, and database the settings name"
}

// releasePrelaunchLease deletes what the checks bound into the rig, proves
// the rig clear, and releases the lease. A failed proof abandons it, so the
// next deputy start recovers what was left.
func releasePrelaunchLease(ctx context.Context, rig rigHost, paths deputyPaths, lease *daemonlock.RigLease) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deputyReleaseTimeout)
	defer cancel()
	if err := proveDeputyRigClear(cleanupCtx, rig, paths.StateRoot); err != nil {
		return errors.Join(err, lease.Abandon())
	}
	return lease.Close()
}

// composePrelaunchManifest runs the deputy's launch composition in memory
// and copies its checks verbatim. The manifest is not written: a manifest
// beside the database binds submissions, and only a deputy start writes one.
func composePrelaunchManifest(
	ctx context.Context, record *prelaunchRecord, settings loadedDeputySettings, paths deputyPaths,
	lease *daemonlock.RigLease, tokenFile string, host prelaunchHost, now time.Time,
) *compositionManifest {
	cfg := deputyPreflightConfig(settings, paths, lease)
	cfg.RigTokenFile = tokenFile
	manifest, err := composePreflight(ctx, cfg, host.preflight, now, host.build)
	if err != nil {
		record.fail(prelaunchGroupComposition, "composition", "the composition could not be evaluated: "+err.Error(),
			"rerun once the error's cause is fixed")
		return nil
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		record.fail(prelaunchGroupComposition, "composition", "the manifest could not be encoded: "+err.Error(), "report it")
		return nil
	}
	digest := submissionBytes(append(body, '\n')).digest
	record.ManifestDigest = &digest
	for _, check := range manifest.Checks {
		record.add(prelaunchGroupComposition, check.Name, check.Status, check.Evidence, check.Remediation)
	}
	return &manifest
}

// checkPrelaunchLaunchBinding makes the comparison submit makes between the
// inputs and the deputy's launch manifest: the project, the publication's
// commit author against the App bot the composition resolved, and the
// policy's paths against the run's allowlist.
func checkPrelaunchLaunchBinding(
	record *prelaunchRecord, manifest *compositionManifest, inputs *prelaunchInputs, project domain.ProjectID,
) {
	const name = "launch_binding"
	switch {
	case inputs == nil:
		record.skip(prelaunchGroupInputs, name, "the inputs must pass first")
		return
	case manifest == nil:
		record.skip(prelaunchGroupInputs, name, "the composition must run first")
		return
	case manifest.PublicationAuthor == nil:
		record.skip(prelaunchGroupInputs, name, "the composition resolved no App bot; see publication_authority")
		return
	}
	if err := validateLaunchComposition(*manifest, inputs.identity, project, inputs.policy); err != nil {
		record.fail(prelaunchGroupInputs, name, err.Error(), fmt.Sprintf(
			"make the publication's commit_author the run's App bot (app_slug %q, bot_user_id %d) "+
				"and keep the policy's paths inside repository.allowed_paths",
			manifest.PublicationAuthor.AppSlug, manifest.PublicationAuthor.BotUserID))
		return
	}
	record.pass(prelaunchGroupInputs, name, "the inputs bind the launch manifest as submit requires")
}

// probePrelaunchLineup runs the lineup probe under the held lease and writes
// lineup-probe.json as probe-lineup does. Its result is evidence beside the
// manifest, never an input to it.
func probePrelaunchLineup(
	ctx context.Context, record *prelaunchRecord, settings loadedDeputySettings, paths deputyPaths,
	tokenFile string, host prelaunchHost, now time.Time,
) {
	calls, err := lineupCalls(&settings)
	if err != nil {
		record.fail(prelaunchGroupLive, "lineup_probe", err.Error(), "correct the efforts in "+paths.Settings)
		return
	}
	prober, err := host.prober(&settings, paths.StateRoot, tokenFile)
	if err != nil {
		record.fail(prelaunchGroupLive, "lineup_probe", err.Error(), "install the Apple container CLI")
		return
	}
	probe := probeLineup(ctx, calls, prober, settings.Digest, host.build, now)
	record.LineupProbe = &probe
	if _, err := writeLineupProbe(paths.StateRoot, probe); err != nil {
		record.fail(prelaunchGroupLive, "lineup_probe", err.Error(), "check "+paths.StateRoot)
		return
	}
	if failed := failedLineupCalls(probe); len(failed) != 0 {
		record.fail(prelaunchGroupLive, "lineup_probe", strings.Join(failed, "; "),
			"choose a model and effort the pinned CLI accepts for each failed role, or rebuild the image at the current pin")
		return
	}
	record.pass(prelaunchGroupLive, "lineup_probe", fmt.Sprintf("all %d lineup calls answered", len(probe.Calls)))
}

// firstPrelaunchMissing names the first onboarding step the root lacks, in
// onboarding order. Each later step is checked only once the earlier ones
// are present; the composition verifies every present one.
func firstPrelaunchMissing(
	record prelaunchRecord, missingTree bool, settings loadedDeputySettings, paths deputyPaths, state prelaunchState,
	manifest *compositionManifest,
) *prelaunchMissing {
	treeMissing := func(evidence string) *prelaunchMissing {
		return &prelaunchMissing{Prerequisite: prelaunchNeedsAgentTree, Evidence: evidence, Fix: prelaunchFirstStart}
	}
	switch {
	case missingTree:
		return treeMissing("agent_tree.commit holds no agent tree for these settings; see the agent_tree check")
	case record.status("agent_tree") == compositionFailed:
		// A local file error: whether the tree is present is unknown.
		return nil
	case !state.present:
		return treeMissing("the root has no database, so no agent tree has been adopted on it")
	case !state.read:
		return nil
	}
	if manifest != nil {
		for _, check := range manifest.Checks {
			if check.Name == "agent_selection" && check.Status == compositionFailed &&
				check.Remediation == agentSelectionUnresolvedFix {
				return treeMissing("the agent tree at agent_tree.commit does not bind the settings' lineup: " + check.Evidence)
			}
		}
	}
	if !state.trustRead {
		return nil
	}
	if !state.trustProfile {
		digest := "<review_configuration_digest from this record's composition>"
		if manifest != nil && manifest.ReviewConfigurationDigest != "" {
			digest = string(manifest.ReviewConfigurationDigest)
		}
		return &prelaunchMissing{
			Prerequisite: prelaunchNeedsTrustProfile,
			Evidence: fmt.Sprintf("no active trust profile binds %s (repository %d)",
				settings.Repository.Repo, settings.Repository.RepositoryID),
			Fix: fmt.Sprintf("run `freesided onboard %s -db %s ... -review-config-digest %s` once without -approve and "+
				"once with the digest it returns; see docs/production-walkthrough.md", settings.Repository.Repo, paths.DB, digest),
		}
	}
	if !state.projectImageRead {
		return nil
	}
	if state.projectImage == nil {
		return &prelaunchMissing{
			Prerequisite: prelaunchNeedsProjectImage,
			Evidence:     "no project-image provenance is recorded for images.agent " + settings.Images.Agent,
			Fix:          "set images.agent to the approving onboarding's project_image.image_ref",
		}
	}
	if state.projectImage.RecipeDigest != settings.ApprovedRecipe {
		return &prelaunchMissing{
			Prerequisite: prelaunchNeedsApprovedRecipe,
			Evidence: fmt.Sprintf("images.agent was built for recipe %s, not approved_recipe %s",
				state.projectImage.RecipeDigest, settings.ApprovedRecipe),
			Fix: "set approved_recipe to the onboarding's project_image.recipe_digest, " + string(state.projectImage.RecipeDigest),
		}
	}
	return nil
}
