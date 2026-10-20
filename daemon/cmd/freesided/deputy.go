package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The bindings the real-work script hard-coded. A deputy is always an
// ephemeral instance of the current build that borrows prod's App authority
// under the production rig lease (#1583) and runs unattended with the full
// conformance suite; none of these is a setting.
const (
	deputyEnvironment           = environmentEphemeral
	deputyOwnerNote             = "freesided deputy"
	deputyContainerBin          = "container"
	deputyProviderEndpoints     = "api.anthropic.com:443"
	deputyReviewWorkspaceSizeMB = 8192
	deputyPreflightItemPrefix   = "system-health-deputy-preflight-"
	deputyInterruptedItemPrefix = "system-health-deputy-interrupted-"
	// deputyCompositionManifestName sits beside the database, where submit
	// looks for it under --require-composition.
	deputyCompositionManifestName = "composition-manifest.json"
)

var errDeputyUsage = errors.New("usage")

// deputyPaths is the fixed layout under -root. Every path is derived, so the
// operator names one directory and the settings file inside it.
type deputyPaths struct {
	StateRoot   string
	Settings    string
	DB          string
	Manifest    string
	Acquisition string
	Patch       string
	AdoptReport string
	AdoptLog    string
	TreeScratch string
	EvidenceDir string
}

func newDeputyPaths(stateRoot string) deputyPaths {
	return deputyPaths{
		StateRoot:   stateRoot,
		Settings:    filepath.Join(stateRoot, "settings.json"),
		DB:          filepath.Join(stateRoot, "freeside.db"),
		Manifest:    filepath.Join(stateRoot, deputyCompositionManifestName),
		Acquisition: filepath.Join(stateRoot, "rig-acquisition.json"),
		Patch:       filepath.Join(stateRoot, "agent-tree.patch"),
		AdoptReport: filepath.Join(stateRoot, "auth-adopt.json"),
		AdoptLog:    filepath.Join(stateRoot, "auth-adopt.log"),
		TreeScratch: filepath.Join(stateRoot, "agent-tree-scratch"),
		// Evidence sits at the standing root, beside daemon/, where the
		// script keeps it and the README names it.
		EvidenceDir: filepath.Join(filepath.Dir(stateRoot), "production-evidence", "composition"),
	}
}

// deputyHost is what a deputy start touches outside its root. Tests replace
// the rig host and the lease root; production uses the real ones.
type deputyHost struct {
	rig              rigHost
	preflight        preflightEnvironment
	adopt            authAdoptDeps
	leaseRoot        string
	launchAgentLabel string
	now              func() time.Time
	tailscaleIPs     func() ([]netip.Addr, error)
}

func productionDeputyHost() deputyHost {
	return deputyHost{
		rig:              newProductionRigHost(),
		preflight:        newProductionPreflightEnvironment(),
		adopt:            productionAuthAdoptDeps(),
		launchAgentLabel: defaultRigLaunchAgentLabel,
		now:              func() time.Time { return time.Now().UTC() },
		tailscaleIPs:     readTailscaleIPs,
	}
}

func runDeputyMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runDeputy(ctx, stop, args, os.Stdout, os.Stderr, productionDeputyHost()); err != nil {
		fmt.Fprintln(os.Stderr, "freesided deputy:", err)
		if errors.Is(err, errDeputyUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// runDeputy starts one deputy: it loads the root's settings, takes the
// production rig lease in-process, checks the composition before any spend,
// and serves until its context ends. A failed check leaves execution
// disabled and says why on a system_health card; the daemon still serves
// pairing and state so the card reaches the operator's clients.
func runDeputy(
	ctx context.Context, stop func(), args []string, stdout, stderr io.Writer, host deputyHost,
) (err error) {
	flags := flag.NewFlagSet("freesided deputy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "", "standing deputy root; settings are read from <root>/daemon/settings.json (required)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errDeputyUsage, err)
	}
	if flags.NArg() != 0 || *root == "" || !filepath.IsAbs(*root) {
		return fmt.Errorf("%w: -root must name an absolute directory", errDeputyUsage)
	}
	_, _ = fmt.Fprintf(stderr, "freesided deputy: build %s\n", buildVersion())
	settings, err := loadDeputySettings(filepath.Join(*root, "daemon", "settings.json"))
	if err != nil {
		return fmt.Errorf("%w: %w", errDeputyUsage, err)
	}
	// The settings file exists, so the state root does; the lease records
	// the canonical path, and the stale-lease match compares against it.
	stateRoot, err := filepath.EvalSymlinks(filepath.Join(*root, "daemon"))
	if err != nil {
		return err
	}
	paths := newDeputyPaths(stateRoot)
	_, _ = fmt.Fprintf(stderr, "freesided deputy: settings %s; publishes to %s (repository %d), base %s\n",
		settings.Digest, settings.Repository.Repo, settings.Repository.RepositoryID, settings.Repository.BaseRef)
	if err := checkDeputyDaemonSettings(settings, paths, host.tailscaleIPs); err != nil {
		return fmt.Errorf("%w: %w", errDeputyUsage, err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	accountHome, err := currentAccountHome()
	if err != nil {
		return err
	}
	// Path checks only, before the lease creates the state and seed roots:
	// the App directories are exempt only as prod's own, and the
	// authority itself is checked under the lease below.
	envPaths := environmentPaths{
		DB: paths.DB, StateDir: paths.StateRoot,
		PublicationStateDir: settings.Prod.AppStateDir, PublicationCredentialsDir: settings.Prod.AppCredentialsDir,
		ReviewInputRoot: settings.Review.InputRoot, ProdAppAuthority: true,
	}
	for _, h := range []string{home, accountHome} {
		if err := checkEnvironment(deputyEnvironment, h, envPaths, settings.Listen); err != nil {
			return err
		}
	}

	lease, interrupted, err := acquireDeputyLease(ctx, host, paths, settings, stderr)
	if err != nil {
		return err
	}
	daemonRan, cleanEnd := false, false
	defer func() {
		// A daemon that ran and did not end cleanly may have left recorded
		// resources: abandon the lease so the next start recovers them.
		err = errors.Join(err, releaseDeputyLease(ctx, host.rig, paths, lease, !daemonRan || cleanEnd))
	}()
	if err := writeDeputyAcquisition(paths.Acquisition, lease); err != nil {
		return err
	}
	if err := checkProdAppAuthority(ctx, deputyEnvironment, home, accountHome, paths.Acquisition,
		lease.Manifest().Resources.LeaseRoot, settings.Prod.InstalledDaemon); err != nil {
		return err
	}
	// A manifest left by an earlier start must never bind this run's
	// submissions; only this start's preflight writes a new one.
	if err := removeIfPresent(paths.Manifest); err != nil {
		return err
	}

	if err := withDeputyStore(ctx, paths.DB, settings, func(st *store.Store) error {
		if interrupted != nil {
			if err := fileDeputyInterruptedCard(ctx, st, *interrupted, host.now); err != nil {
				return err
			}
		}
		return recordDeputyIdentities(ctx, st, settings, host.now)
	}); err != nil {
		return err
	}

	check, launch, err := deputySelfCheck(ctx, host, paths, settings, lease, stderr)
	if err != nil {
		return err
	}
	if check.passed() {
		_, _ = fmt.Fprintf(stderr, "freesided deputy: composition passed; manifest %s\n", paths.Manifest)
	} else {
		_, _ = fmt.Fprintf(stderr, "freesided deputy: %s failed; execution stays disabled (%s)\n",
			strings.Join(check.Failed, ", "), check.Remediation)
	}
	if err := withDeputyStore(ctx, paths.DB, settings, func(st *store.Store) error {
		return convergeDeputyPreflightCard(ctx, st, check, host.now)
	}); err != nil {
		return err
	}

	prodLock, err := holdProdDatabaseLock(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, prodLock.Close()) }()
	logger, err := newLogger(stderr, defaultLogLevel)
	if err != nil {
		return err
	}
	daemonRan = true
	h, err := run(ctx, stop, deputyDaemonConfig(settings, paths, launch, logger))
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(h.readiness()); err != nil {
		stop()
		return errors.Join(err, h.Close())
	}
	if err := serve(ctx, stop, h); err != nil {
		return err
	}
	cleanEnd = true
	return nil
}

// deputyReleaseTimeout bounds the container cleanup at the end of a run. The
// run's own context is already cancelled by then.
const deputyReleaseTimeout = 2 * time.Minute

// releaseDeputyLease ends the run's lease. A clean release is what rig hold
// and the script's `rig cleanup` did together: delete the recorded
// containers, prove every recorded resource absent, then release. The
// resources are read from the lease's own manifest on disk, where the driver
// records them as it binds them. A failed proof, or an end the caller does
// not call clean, abandons the lease instead, so the next start recovers
// what was left and files the interrupted card. Either way the composition
// manifest is removed, so it binds nothing once this run is gone.
func releaseDeputyLease(
	ctx context.Context, host rigHost, paths deputyPaths, lease *daemonlock.RigLease, clean bool,
) error {
	files := errors.Join(removeIfPresent(paths.Manifest), removeIfPresent(paths.Acquisition))
	if !clean {
		return errors.Join(files, lease.Abandon())
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deputyReleaseTimeout)
	defer cancel()
	if err := proveDeputyRigClear(cleanupCtx, host, paths.StateRoot); err != nil {
		return errors.Join(files, fmt.Errorf("left the rig lease for the next start to recover: %w", err), lease.Abandon())
	}
	return errors.Join(files, lease.Close())
}

func proveDeputyRigClear(ctx context.Context, host rigHost, stateRoot string) error {
	manifest, err := daemonlock.ReadRigManifest(stateRoot)
	if err != nil {
		return err
	}
	if err := cleanupRigContainers(ctx, host, deputyContainerBin, manifest.Resources.Containers); err != nil {
		return err
	}
	return requireRigResourcesAbsent(ctx, host, deputyContainerBin, manifest.Resources)
}

// acquireDeputyLease takes the production rig lease for this root. A stale
// lease recorded for this same root is the previous deputy's: it is
// recovered (its recorded containers, volumes, and networks removed) and
// returned so the run is recorded as interrupted. A stale lease for another
// root is not this deputy's to recover, and a live holder refuses.
func acquireDeputyLease(
	ctx context.Context, host deputyHost, paths deputyPaths, settings loadedDeputySettings, stderr io.Writer,
) (*daemonlock.RigLease, *daemonlock.RigManifest, error) {
	currentUser, err := user.Current()
	if err != nil {
		return nil, nil, fmt.Errorf("identify rig owner user: %w", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, nil, fmt.Errorf("identify rig owner host: %w", err)
	}
	acquire := daemonlock.RigAcquireConfig{
		Owner: daemonlock.RigOwner{
			User: currentUser.Username, Host: hostname, PID: os.Getpid(), Note: deputyOwnerNote,
		},
		StateRoot: paths.StateRoot, DatabasePath: paths.DB,
		ListenAddress: settings.Listen, SeedRoot: settings.SeedRoot, LeaseRoot: host.leaseRoot,
	}
	lease, err := daemonlock.AcquireRig(acquire)
	var interrupted *daemonlock.RigManifest
	if stale := (*daemonlock.RigStaleError)(nil); errors.As(err, &stale) {
		if stale.Manifest.Resources.StateRoot != paths.StateRoot {
			return nil, nil, fmt.Errorf("%w; it belongs to %s, not this root: recover it with "+
				"`freesided rig recover -state-root %s`", err,
				stale.Manifest.Resources.StateRoot, stale.Manifest.Resources.StateRoot)
		}
		_, _ = fmt.Fprintf(stderr, "freesided deputy: recovering the interrupted run's lease from %s@%s pid=%d acquired %s\n",
			stale.Manifest.Owner.User, stale.Manifest.Owner.Host, stale.Manifest.Owner.PID,
			stale.Manifest.AcquiredAt.Format(time.RFC3339))
		if err := recoverStaleRig(ctx, stderr, host.rig, paths.StateRoot, deputyContainerBin,
			host.launchAgentLabel, true); err != nil {
			return nil, nil, fmt.Errorf("recover the interrupted run's lease: %w", err)
		}
		manifest := stale.Manifest
		interrupted = &manifest
		lease, err = daemonlock.AcquireRig(acquire)
	}
	if err != nil {
		return nil, nil, err
	}
	if err := proveRigIdle(ctx, host.rig, lease.Manifest().Resources, host.launchAgentLabel); err != nil {
		return nil, nil, errors.Join(err, lease.Close())
	}
	return lease, interrupted, nil
}

// writeDeputyAcquisition writes the lease token where the driver and the
// prod-authority check read it, as rig hold's stdout did for the script.
func writeDeputyAcquisition(path string, lease *daemonlock.RigLease) error {
	body, err := json.Marshal(rigHoldOutput{Token: lease.Token(), Manifest: lease.Manifest()})
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, append(body, '\n'), 0o600)
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func withDeputyStore(
	ctx context.Context, dbPath string, settings loadedDeputySettings, fn func(*store.Store) error,
) (err error) {
	st, _, err := openStoreWithTopicKey(ctx, dbPath, store.Options{
		ApprovedRecipes: map[domain.Digest]bool{settings.ApprovedRecipe: true},
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return fn(st)
}

// recordDeputyIdentities records the writer and reviewer identities a fresh
// root lacks, so auth adopt can enroll them. A recorded identity is checked,
// never rewritten: settings that disagree with the store refuse the launch.
func recordDeputyIdentities(
	ctx context.Context, st *store.Store, settings loadedDeputySettings, now func() time.Time,
) error {
	reviewSnapshot, err := filepath.EvalSymlinks(settings.Review.AuthSnapshot)
	if err != nil {
		return fmt.Errorf("review.auth_snapshot: %w", err)
	}
	identities := []domain.AuthIdentity{{
		ID: domain.AuthIdentityID(settings.AgentTree.AuthIdentity), Provider: "claude",
		AuthStoreMutationLease: true, MaxParallelExecutions: 1, Enabled: true,
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: settings.AgentTree.AuthVolume, RefreshStrategy: domain.RefreshOnDemand,
		},
	}, {
		ID: domain.AuthIdentityID(settings.AgentTree.ReviewAuthIdentity), Provider: "openai",
		AuthStoreMutationLease: true, MaxParallelExecutions: 1, Enabled: true,
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: reviewSnapshot, RefreshStrategy: domain.RefreshOnDemand,
			SupportsReadOnlyAuthSnapshot: true,
		},
	}}
	return st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, identity := range identities {
			stored, err := tx.GetAuthIdentity(ctx, identity.ID)
			switch {
			case err == nil:
				if !stored.SameFixedBindings(identity) {
					return fmt.Errorf("recorded auth identity %s differs from the settings", identity.ID)
				}
				continue
			case !errors.Is(err, store.ErrNotFound):
				return err
			}
			if err := tx.RecordAuthIdentity(ctx, identity, now()); err != nil {
				return err
			}
		}
		return nil
	})
}

// deputySelfCheckResult is the start-up verdict: the failed check names, in
// order, and the first one's remediation. No failed check means execution
// may be enabled.
type deputySelfCheckResult struct {
	Failed      []string
	Remediation string
}

func (r deputySelfCheckResult) passed() bool { return len(r.Failed) == 0 }

// deputySelfCheck adopts the agent tree and runs the composition preflight,
// in that order, as the script did. Both need the database idle and the
// listener free, so they run before the daemon opens either. An error is a
// failure to evaluate; a failed check is the result. The launch carries the
// manifest's digest only when the check passed.
func deputySelfCheck(
	ctx context.Context, host deputyHost, paths deputyPaths, settings loadedDeputySettings,
	lease *daemonlock.RigLease, stderr io.Writer,
) (deputySelfCheckResult, deputyLaunch, error) {
	if result, ok := adoptDeputyAgentTree(ctx, host, paths, settings); !ok {
		return result, deputyLaunch{}, nil
	}
	manifest, err := composePreflight(ctx, deputyPreflightConfig(settings, paths, lease), host.preflight,
		host.now(), buildVersion())
	if err != nil {
		return deputySelfCheckResult{}, deputyLaunch{}, err
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return deputySelfCheckResult{}, deputyLaunch{}, err
	}
	body = append(body, '\n')
	if err := atomicfile.WriteFile(paths.Manifest, body, 0o600); err != nil {
		return deputySelfCheckResult{}, deputyLaunch{}, err
	}
	var result deputySelfCheckResult
	for _, check := range manifest.Checks {
		if check.Status != compositionFailed {
			continue
		}
		if result.Remediation == "" {
			result.Remediation = check.Remediation
		}
		result.Failed = append(result.Failed, check.Name)
	}
	if !result.passed() {
		result.Remediation += "; the composition manifest is " + paths.Manifest
		return result, deputyLaunch{Manifest: manifest}, nil
	}
	evidence, err := publishCompositionEvidence(paths.EvidenceDir, body)
	if err != nil {
		return deputySelfCheckResult{}, deputyLaunch{}, err
	}
	_, _ = fmt.Fprintf(stderr, "freesided deputy: production composition manifest: %s\n", evidence)
	return result, deputyLaunch{ManifestDigest: submissionBytes(body).digest, Manifest: manifest}, nil
}

// adoptDeputyAgentTree runs auth adopt in-process and compares the tree it
// emits with the agent tree commit. The operator reviews and commits the
// patch; a deputy never edits the checkout.
func adoptDeputyAgentTree(
	ctx context.Context, host deputyHost, paths deputyPaths, settings loadedDeputySettings,
) (deputySelfCheckResult, bool) {
	failed := func(remediation string) (deputySelfCheckResult, bool) {
		return deputySelfCheckResult{Failed: []string{"agent_tree"}, Remediation: remediation}, false
	}
	if err := removeIfPresent(paths.Patch); err != nil {
		return failed("could not clear " + paths.Patch)
	}
	report, err := os.OpenFile(paths.AdoptReport, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return failed("could not write " + paths.AdoptReport)
	}
	defer func() { _ = report.Close() }()
	log, err := os.OpenFile(paths.AdoptLog, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return failed("could not write " + paths.AdoptLog)
	}
	defer func() { _ = log.Close() }()
	adoptErr := runAuthAdoptCommand(ctx, deputyAdoptArgs(settings, paths), report, log, host.adopt)
	if adoptErr != nil {
		_, _ = fmt.Fprintln(log, "freesided auth adopt:", adoptErr)
	}
	if info, err := os.Stat(paths.Patch); adoptErr != nil || err != nil || info.Size() == 0 {
		return failed(fmt.Sprintf("auth adopt failed or emitted no tree patch; its report is %s and its log is %s",
			paths.AdoptReport, paths.AdoptLog))
	}
	matches, err := agentTreeMatches(ctx, paths.Patch, settings.AgentTree.Checkout, settings.AgentTree.Commit,
		paths.TreeScratch)
	switch {
	case err != nil:
		_, _ = fmt.Fprintln(log, "compare agent tree:", err)
		return failed(fmt.Sprintf("could not compare %s with the agent tree checkout %s; see %s",
			paths.Patch, settings.AgentTree.Checkout, paths.AdoptLog))
	case !matches:
		return failed(fmt.Sprintf("the agent tree at %s in %s is not the tree auth adopt emitted: "+
			"review and commit %s in that checkout, set agent_tree.commit to the new commit, and restart",
			settings.AgentTree.Commit, settings.AgentTree.Checkout, paths.Patch))
	}
	return deputySelfCheckResult{}, true
}

func deputyAdoptArgs(settings loadedDeputySettings, paths deputyPaths) []string {
	args := []string{
		"-db", paths.DB,
		"-approved-recipe", string(settings.ApprovedRecipe),
		"-auth-identity", settings.AgentTree.AuthIdentity,
		"-review-auth-identity", settings.AgentTree.ReviewAuthIdentity,
		"-cost-owner", settings.AgentTree.CostOwner,
		"-review-cost-owner", settings.AgentTree.ReviewCostOwner,
		"-claude-account", settings.AgentTree.ClaudeAccount,
		"-exporter-image", settings.Images.Exporter,
		"-auth-store-root", settings.Review.InputRoot,
		"-review-model", settings.Review.Model,
		"-review-effort", string(settings.Review.Effort),
		"-terms-basis-date", settings.AgentTree.TermsBasisDate,
		"-pricing-revision", settings.AgentTree.PricingRevision,
		"-offer-not-after", settings.AgentTree.OfferNotAfter,
		"-prompt-package", settings.Prompts.PromptPackage,
		"-specification-prompt-package", settings.Prompts.SpecificationPromptPackage,
		"-remediation-prompt-package", settings.Prompts.RemediationPromptPackage,
	}
	if settings.Judgment.Model != "" {
		args = append(args,
			"-judgment-model", settings.Judgment.Model,
			"-judgment-publication-author-prompt", settings.Judgment.PublicationAuthorPrompt)
	}
	return append(args, "-patch", paths.Patch)
}

// deputyAgentTreePaths are the policy paths auth adopt writes.
var deputyAgentTreePaths = []string{"policy/agents", "policy/fragments", "policy/lineup", "policy/agents.lock"}

// agentTreeMatches applies patch to an empty scratch repository and compares
// the resulting tree entries under the policy paths with commit's. Git runs
// with no operator configuration, so a whitespace or line-ending setting
// cannot change the bytes compared. A commit the checkout lacks compares as
// a mismatch, not an error, because the remedy is the same: commit the tree.
func agentTreeMatches(ctx context.Context, patch, checkout, commit, scratch string) (bool, error) {
	const limit = 1 << 20
	format, err := runPreflightAuthorGit(ctx, checkout, limit, "rev-parse", "--show-object-format")
	if err != nil {
		return false, fmt.Errorf("read the checkout's object format: %w", err)
	}
	if err := os.RemoveAll(scratch); err != nil {
		return false, err
	}
	if _, err := runPreflightAuthorGit(ctx, filepath.Dir(scratch), limit,
		"init", "-q", "--object-format="+strings.TrimSpace(string(format)), scratch); err != nil {
		return false, fmt.Errorf("create the scratch repository: %w", err)
	}
	if _, err := runPreflightAuthorGit(ctx, scratch, limit, "apply", "--index", patch); err != nil {
		return false, fmt.Errorf("apply the emitted patch: %w", err)
	}
	emitted, err := runPreflightAuthorGit(ctx, scratch, limit, "write-tree")
	if err != nil {
		return false, fmt.Errorf("write the emitted tree: %w", err)
	}
	want, err := runPreflightAuthorGit(ctx, scratch, limit,
		append([]string{"ls-tree", strings.TrimSpace(string(emitted)), "--"}, deputyAgentTreePaths...)...)
	if err != nil {
		return false, fmt.Errorf("list the emitted tree: %w", err)
	}
	have, err := runPreflightAuthorGit(ctx, checkout, limit,
		append([]string{"ls-tree", commit, "--"}, deputyAgentTreePaths...)...)
	if err != nil {
		return false, nil
	}
	return bytes.Equal(want, have), nil
}

// deputyPreflightConfig composes the run, not one submission: submit binds
// each submission to this manifest.
func deputyPreflightConfig(
	settings loadedDeputySettings, paths deputyPaths, lease *daemonlock.RigLease,
) preflightConfig {
	cfg := preflightConfig{
		RigTokenFile: paths.Acquisition,
		ServerURL:    "http://" + lease.Manifest().Resources.ListenAddress,
		ContainerBin: deputyContainerBin,
		AgentImage:   settings.Images.Agent, ExporterImage: settings.Images.Exporter,
		ReviewImage: settings.Images.Review,
		Repo:        settings.Repository.Repo, RepositoryCheckout: settings.Repository.Checkout,
		RepositoryID: settings.Repository.RepositoryID,
		BaseRef:      settings.Repository.BaseRef, BaseSHA: settings.Repository.BaseSHA,
		ApprovedRecipe:    settings.ApprovedRecipe,
		AgentTreeCheckout: settings.AgentTree.Checkout, AgentTreeCommit: settings.AgentTree.Commit,
		AuthVolume:      settings.AgentTree.AuthVolume,
		ReviewInputRoot: settings.Review.InputRoot, ReviewAuthMode: settings.Review.AuthMode,
		ReviewAuthSnapshot: settings.Review.AuthSnapshot, ReviewInstructions: settings.Review.Instructions,
		PublicationStateDir:       settings.Prod.AppStateDir,
		PublicationCredentialsDir: settings.Prod.AppCredentialsDir,
		ReviewWorkspaceSizeMB:     deputyReviewWorkspaceSizeMB,
		ProjectID:                 settings.Repository.Project,
		BuildProxy:                settings.Build.Proxy,
		LaunchAgentLabel:          defaultRigLaunchAgentLabel,
		AllowedPaths:              settings.Repository.AllowedPaths,
		Launch:                    true,
		SettingsDigest:            settings.Digest,
	}
	cfg.Judgments = deputyJudgments(settings, "")
	return cfg
}

func deputyJudgments(settings loadedDeputySettings, expectedDigest string) judgmentConfig {
	if settings.Judgment.ClaudeBin == "" {
		return judgmentConfig{}
	}
	return judgmentConfig{
		CLI:                     claudeinference.Config{Binary: settings.Judgment.ClaudeBin, SHA256: settings.Judgment.ClaudeSHA256},
		PublicationAuthorPrompt: settings.Judgment.PublicationAuthorPrompt,
		ExpectedDigest:          expectedDigest,
	}
}

// publishCompositionEvidence keeps a content-addressed, no-clobber copy of a
// passing manifest under the state root, as the script did: an exact replay
// converges on the same bytes, and different inputs can never replace
// earlier acceptance evidence.
func publishCompositionEvidence(dir string, body []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	digest := contentaddr.Hex(contentaddr.Sum(body))
	path := filepath.Join(dir, digest+".json")
	if err := atomicfile.WriteFileNoReplace(path, body, 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("publish composition evidence: %w", err)
	}
	existing, err := os.ReadFile(path) //nolint:gosec // G304: the deputy's own content-addressed evidence path
	if err != nil {
		return "", fmt.Errorf("read composition evidence: %w", err)
	}
	if !bytes.Equal(existing, body) {
		return "", fmt.Errorf("composition evidence collision at %s", path)
	}
	return path, nil
}

// deputyDaemonConfig maps the settings onto the daemon configuration the
// script's flags built. A failed self-check, a launch with no manifest
// digest, leaves the driver unset, so the daemon serves pairing and state
// and runs no agent. Either way the daemon holds the launch, so it binds
// new work to it.
func deputyDaemonConfig(
	settings loadedDeputySettings, paths deputyPaths, launch deputyLaunch, logger *slog.Logger,
) config {
	cfg := config{
		Environment:                deputyEnvironment,
		ManualSubmissionConfigPath: settings.ManualSubmissionConfig,
		DBPath:                     paths.DB, StateDir: paths.StateRoot,
		ListenAddr: settings.Listen, NtfyURL: defaultNtfyURL, ReconcileInterval: defaultReconcileInterval,
		DoctorInterval:            defaultDoctorInterval,
		SchedulerInterval:         defaultSchedulerInterval,
		AttentionDeliveryInterval: defaultAttentionDeliveryInterval,
		ApprovedRecipes:           map[domain.Digest]bool{settings.ApprovedRecipe: true},
		Logger:                    logger,
		DeputyLaunch:              &launch,
	}
	if launch.ManifestDigest == "" {
		return cfg
	}
	claudeConfig := deputyClaudeDriverConfig(settings, paths, launch.Manifest.JudgmentConfigurationDigest)
	cfg.Claude = &claudeConfig
	return cfg
}

func deputyClaudeDriverConfig(
	settings loadedDeputySettings, paths deputyPaths, judgmentDigest string,
) claudeDriverConfig {
	return claudeDriverConfig{
		AgentImage: domain.ImageRef(settings.Images.Agent), ExporterImage: settings.Images.Exporter,
		ContainerBin: deputyContainerBin, SeedRoot: settings.SeedRoot,
		WriterStopTimeout: settings.stopTime, WriterStallInterval: settings.stallFor,
		Rebuild:  settings.rebuildInputs(),
		StateDir: paths.StateRoot, RigTokenFile: paths.Acquisition,
		Judgments:                      deputyJudgments(settings, judgmentDigest),
		ProviderEndpoints:              []string{deputyProviderEndpoints},
		PromptPackageFile:              settings.Prompts.PromptPackage,
		SpecificationPromptPackageFile: settings.Prompts.SpecificationPromptPackage,
		RemediationPromptPackageFile:   settings.Prompts.RemediationPromptPackage,
		VendorInstructions:             settings.Prompts.VendorInstructions,
		Repo:                           settings.Repository.Repo, RepositoryID: settings.Repository.RepositoryID,
		BaseRef: settings.Repository.BaseRef, BaseSHA: settings.Repository.BaseSHA,
		AgentTreeCheckout: settings.AgentTree.Checkout, AgentTreeCommit: settings.AgentTree.Commit,
		AllowedPaths:   settings.Repository.AllowedPaths,
		RunConformance: true,
		StateRoot:      settings.Prod.AppStateDir, CredentialsDir: settings.Prod.AppCredentialsDir,
		OperatingMode: domain.ModeUnattended,
		ReviewImage:   settings.Images.Review, ReviewInputRoot: settings.Review.InputRoot,
		ReviewAuthMode:     settings.Review.AuthMode,
		ReviewAuthSnapshot: settings.Review.AuthSnapshot, ReviewInstructions: settings.Review.Instructions,
		ReviewWorkspaceSizeMB: deputyReviewWorkspaceSizeMB,
	}
}

// checkDeputyDaemonSettings refuses known startup failures before any lease,
// credential adoption, or passing evidence. Startup still revalidates mutable
// files and addresses when it uses them.
func checkDeputyDaemonSettings(
	settings loadedDeputySettings, paths deputyPaths, tailscaleIPs func() ([]netip.Addr, error),
) error {
	if _, err := loadManualSubmissionConfig(settings.ManualSubmissionConfig); err != nil {
		return fmt.Errorf("settings: manual_submission_config: %w", err)
	}
	if _, err := resolveListenAddress(settings.Listen, tailscaleIPs); err != nil {
		return fmt.Errorf("settings: listen: %w", err)
	}
	cfg := deputyClaudeDriverConfig(settings, paths, "")
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("settings: Claude configuration: %w", err)
	}
	if _, err := readPromptPackages(cfg); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	return nil
}

// convergeDeputyPreflightCard keeps one open card for a failed start-up
// check: earlier deputy preflight cards are resolved, and a failure files a
// new one naming what failed now.
func convergeDeputyPreflightCard(
	ctx context.Context, st *store.Store, check deputySelfCheckResult, now func() time.Time,
) error {
	return st.Write(ctx, func(tx *store.WriteTx) error {
		items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		for _, item := range items {
			if !strings.HasPrefix(string(item.ID), deputyPreflightItemPrefix) {
				continue
			}
			item.Status = domain.StatusResolved
			item.ItemVersion++
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		if check.passed() {
			return nil
		}
		return putDeputyCard(ctx, tx, deputyPreflightItemPrefix, domain.HealthPostureBlocking,
			domain.HealthDiagnostic{Code: "deputy_preflight_failed", Impairs: domain.ImpairedCapabilityUnattendedAdmission},
			fmt.Sprintf("The deputy's start-up checks failed, so agent execution stays disabled: %s. %s.",
				strings.Join(check.Failed, ", "), check.Remediation), now)
	})
}

// fileDeputyInterruptedCard records a previous deputy on this root that
// ended without releasing its lease, which is how a dead deputy is noticed.
func fileDeputyInterruptedCard(
	ctx context.Context, st *store.Store, interrupted daemonlock.RigManifest, now func() time.Time,
) error {
	return st.Write(ctx, func(tx *store.WriteTx) error {
		return putDeputyCard(ctx, tx, deputyInterruptedItemPrefix, domain.HealthPostureAdvisory,
			domain.HealthDiagnostic{Code: "deputy_run_interrupted", Impairs: domain.ImpairedCapabilityNone},
			fmt.Sprintf("A previous deputy run on this root (%s@%s pid %d, started %s) ended without releasing "+
				"the production rig lease. This start recovered the lease and removed the run's recorded "+
				"containers, volumes, and networks; its tasks resume or fail on their own records.",
				interrupted.Owner.User, interrupted.Owner.Host, interrupted.Owner.PID,
				interrupted.AcquiredAt.Format(time.RFC3339)), now)
	})
}

func putDeputyCard(
	ctx context.Context, tx *store.WriteTx, prefix string, posture domain.HealthPosture,
	diagnostic domain.HealthDiagnostic, reason string, now func() time.Time,
) error {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return err
	}
	subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	names, err := tx.DisplayNamesFor(ctx, "project-system", subject)
	if err != nil {
		return err
	}
	createdAt := now().UTC()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        domain.ItemID(fmt.Sprintf("%s%d", prefix, state.Revision+1)),
		ProjectID: "project-system", Subject: subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason:            reason,
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		HealthDiagnostic:  &diagnostic,
		DisplayNames:      names, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional,
		CreatedAt:         &createdAt, Posture: &posture, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, item)
}
