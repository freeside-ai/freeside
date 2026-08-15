package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func runOnboardMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runOnboardCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided onboard:", err)
		os.Exit(1)
	}
}

type onboardConfig struct {
	Repository        string
	DBPath            string
	StateDir          string
	RegistrationID    int64
	RepositoryID      int64
	Account           string
	AccountID         int64
	InstallationID    int64
	Resume            bool
	InstallWait       time.Duration
	CredentialsDir    string
	Commit            string
	BaseRef           string
	RecipePath        string
	SourceDir         string
	BaseImage         string
	BaseBuildRef      string
	ReviewConfig      string
	CommitPlan        domain.CommitPlanMode
	Approval          string
	Registry          string
	LocalRegistryPort int
	ImageName         string
	RefTag            string
	GitPath           string
	ContainerPath     string
	TempDir           string
	DNS               []string
	BuildProxy        string
}

type stringList []string

func (v *stringList) String() string { return fmt.Sprint([]string(*v)) }

func (v *stringList) Set(value string) error {
	if value == "" {
		return errors.New("value must not be empty")
	}
	*v = append(*v, value)
	return nil
}

func runOnboardCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer,
) (err error) {
	cfg, err := parseOnboardConfig(args, stderr)
	if err != nil {
		return err
	}
	var recipe []byte
	if cfg.RecipePath == "" {
		recipe, err = detectRecipe(ctx, cfg.GitPath, cfg.SourceDir, cfg.Commit)
	} else {
		recipe, err = os.ReadFile(cfg.RecipePath)
	}
	if err != nil {
		return err
	}
	if _, err := verify.ParseRecipe(recipe); err != nil {
		return fmt.Errorf("detected recipe: %w", err)
	}
	st, err := store.Open(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	authority, err := publish.NewInstallationAuthorityStore(cfg.StateDir)
	if err != nil {
		return err
	}
	if cfg.CredentialsDir == "" {
		cfg.CredentialsDir = filepath.Join(filepath.Dir(cfg.StateDir), "credentials")
	}
	keystore, err := publish.NewKeystore(cfg.CredentialsDir, cfg.StateDir)
	if err != nil {
		return err
	}
	apps, err := keystore.ListApps()
	if err != nil {
		return err
	}
	registrationIDs := make([]int64, 0, len(apps))
	for _, app := range apps {
		registrationIDs = append(registrationIDs, app.AppID)
	}
	trusted, err := trustedInstallation(
		ctx, authority, registrationIDs, cfg.RegistrationID, cfg.RepositoryID,
	)
	if err != nil {
		return err
	}
	if trusted == nil {
		if cfg.Account == "" || cfg.AccountID <= 0 || cfg.InstallationID < 0 {
			return errors.New(
				"-account, positive -account-id, and non-negative -installation-id " +
					"are required until the repository is trusted")
		}
		intent := operations.InstallationIntentRequest{
			RegistrationID: cfg.RegistrationID,
			Account:        cfg.Account,
			AccountID:      cfg.AccountID,
			InstallationID: cfg.InstallationID,
			RepositoryID:   cfg.RepositoryID,
			ExpiresAt:      time.Now().UTC().Add(cfg.InstallWait),
		}
		if !cfg.Resume {
			installURL, err := installationURLForRegistration(
				cfg.CredentialsDir, cfg.StateDir, cfg.RegistrationID,
			)
			if err != nil {
				return err
			}
			pending, err := operations.BeginInstallation(ctx, authority, intent, time.Now)
			if err != nil {
				return err
			}
			// The recorded intent gets its durable §5.16 poll schedule in the
			// same session: --resume (or the daemon) drives it, and the
			// envelope's expiry bounds it across sessions.
			pollScheduler, err := newInstallPollScheduler(st, authority, noPendingReady{})
			if err != nil {
				return err
			}
			if err := armInstallationPoll(ctx, pollScheduler, intent.RegistrationID,
				pending.ActiveEpoch, pending.DurableIntentRevision, pending.ExpiresAt); err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(struct {
				Status          string                        `json:"status"`
				Pending         publish.PendingEnvelopeRecord `json:"pending"`
				InstallationURL string                        `json:"installation_url"`
			}{
				Status: "installation_pending", Pending: pending,
				InstallationURL: installURL,
			})
		}
	}
	runOnboard := func(
		janitor *publish.InstallationJanitor,
		keystore *publish.Keystore,
		client *http.Client,
	) error {
		recorder, recorderErr := publish.NewStoreRecorder(st)
		if recorderErr != nil {
			return recorderErr
		}
		trust, trustErr := publish.NewStoreTrustSource(st)
		if trustErr != nil {
			return trustErr
		}
		minter := publish.NewMinterWithJanitor(
			keystore, client, defaultGitHubAPIBase,
			recorder, trust, time.Now, janitor,
		)
		gate := onboardingJanitorGate{janitor: janitor}
		tokens := publish.NewOnboardingTokenSource(
			minter, authority, gate,
			cfg.RegistrationID, cfg.RepositoryID, time.Now,
		)
		builder, builderErr := projectimage.New(projectimage.Options{
			GitPath: cfg.GitPath, ContainerPath: cfg.ContainerPath,
			TempDir: cfg.TempDir, Log: stderr, Tokens: tokens,
			Record: func(recordCtx context.Context, image domain.ProjectImage) error {
				return st.WriteInternal(recordCtx, func(tx *store.InternalTx) error {
					return tx.RecordProjectImage(recordCtx, image)
				})
			},
			LookupRecordedRef: func(
				lookupCtx context.Context, ref string,
			) (bool, error) {
				recorded := false
				err := st.Read(lookupCtx, func(tx *store.ReadTx) error {
					var lookupErr error
					recorded, lookupErr = tx.ProjectImageRefRecorded(
						lookupCtx, domain.ImageRef(ref))
					return lookupErr
				})
				return recorded, err
			},
		})
		if builderErr != nil {
			return builderErr
		}
		auditor, auditorErr := publish.NewGitHubWorkflowAuditor(
			tokens, client, defaultGitHubAPIBase, time.Now,
		)
		if auditorErr != nil {
			return auditorErr
		}
		result, runErr := (operations.Onboard{
			Store: st, Builder: builder, Auditor: auditor, Authority: authority,
			Documents: authority, Gate: gate, Now: time.Now,
		}).Run(ctx, operations.OnboardRequest{
			Repository: cfg.Repository, RepositoryID: cfg.RepositoryID,
			RegistrationID: cfg.RegistrationID,
			BaseRef:        cfg.BaseRef,
			ApprovalDigest: domain.Digest(cfg.Approval),
			Policy: operations.OnboardPolicy{
				PRExecution:    domain.PRExecutionAuditedSameRepo,
				CommitPlan:     cfg.CommitPlan,
				MessageRuleset: domain.MessageRulesetGitHub1,
				ReviewMode:     domain.ReviewFreesideInvoked,
				ReviewConfig:   domain.Digest(cfg.ReviewConfig),
			},
			Image: projectimage.Request{
				Repository: cfg.Repository, RepositoryID: cfg.RepositoryID,
				CommitSHA: cfg.Commit, Recipe: recipe,
				BaseImageRef: domain.ImageRef(cfg.BaseImage), BaseBuildRef: cfg.BaseBuildRef,
				Registry: cfg.Registry, LocalRegistryPort: cfg.LocalRegistryPort,
				ImageName: cfg.ImageName, RefTag: cfg.RefTag,
				DNS: cfg.DNS, BuildProxy: cfg.BuildProxy,
			},
		})
		if runErr != nil {
			return runErr
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			return fmt.Errorf("write onboarding result: %w", err)
		}
		return nil
	}
	if trusted != nil {
		return withInstallationJanitor(
			ctx, st, authority, cfg.CredentialsDir, cfg.StateDir, cfg.InstallWait,
			func(janitor *publish.InstallationJanitor) (bool, error) {
				return janitor.AllowsRepository(
					cfg.RegistrationID, trusted.InstallationID, cfg.RepositoryID,
				), nil
			},
			runOnboard,
		)
	}
	return withDurableInstallationPoll(
		ctx, st, authority, cfg.CredentialsDir, cfg.StateDir, cfg.RegistrationID, runOnboard,
	)
}

func parseOnboardConfig(args []string, output io.Writer) (onboardConfig, error) {
	if len(args) == 0 {
		return onboardConfig{}, errors.New("repository owner/name is required")
	}
	flags := flag.NewFlagSet("freesided onboard", flag.ContinueOnError)
	flags.SetOutput(output)
	cfg := onboardConfig{Repository: args[0]}
	var commitPlan string
	var dns stringList
	flags.StringVar(&cfg.DBPath, "db", "", "SQLite database path (required)")
	flags.StringVar(&cfg.StateDir, "state-dir", "", "GitHub App authority state directory (required)")
	flags.Int64Var(&cfg.RegistrationID, "registration-id", 0, "selected numeric GitHub App ID (required)")
	flags.Int64Var(&cfg.RepositoryID, "repository-id", 0, "canonical numeric repository ID (required)")
	flags.StringVar(&cfg.Account, "account", "", "canonical repository-owning account login")
	flags.Int64Var(&cfg.AccountID, "account-id", 0, "canonical numeric repository-owning account ID")
	flags.Int64Var(&cfg.InstallationID, "installation-id", 0, "selected installation ID; zero before GitHub assigns one")
	flags.BoolVar(&cfg.Resume, "resume", false, "resume the existing bounded installation intent")
	flags.DurationVar(&cfg.InstallWait, "install-wait", 10*time.Minute, "maximum wait for native installation approval")
	flags.StringVar(&cfg.CredentialsDir, "credentials-dir", "",
		"GitHub App credentials directory (defaults beside the state directory)")
	flags.StringVar(&cfg.Commit, "commit", "", "exact full lowercase repository commit (required)")
	flags.StringVar(&cfg.BaseRef, "base-ref", "", "branch whose live workflow authority is reviewed (required)")
	flags.StringVar(&cfg.RecipePath, "recipe", "", "detected trusted verification recipe (required)")
	flags.StringVar(&cfg.SourceDir, "source", "",
		"repository checkout used to detect .freeside/verify.json when -recipe is omitted")
	flags.StringVar(&cfg.BaseImage, "base-image", "", "digest-pinned approved agent base (required)")
	flags.StringVar(&cfg.BaseBuildRef, "base-build-ref", "", "local base tag matching -base-image (required)")
	flags.StringVar(&cfg.ReviewConfig, "review-config-digest", "", "digest of the reviewed automated-review configuration (required)")
	flags.StringVar(&commitPlan,
		"commit-plan", string(domain.CommitPlanSingleCommit),
		"commit-plan mode: single_commit or plan_preferred")
	flags.StringVar(&cfg.Approval, "approve", "", "exact proposed review digest; omit for the one-time review")
	flags.StringVar(&cfg.Registry, "registry", "", "registry host/path destination")
	flags.IntVar(&cfg.LocalRegistryPort, "local-registry-port", 0, "managed loopback registry port")
	flags.StringVar(&cfg.ImageName, "image-name", "", "project image name")
	flags.StringVar(&cfg.RefTag, "ref-tag", "v1", "one-shot image tag prefix")
	flags.StringVar(&cfg.GitPath, "git", "", "git executable (default from PATH)")
	flags.StringVar(&cfg.ContainerPath, "container", "", "Apple container executable (default from PATH)")
	flags.StringVar(&cfg.TempDir, "temp-dir", "", "bindable scratch parent")
	flags.Var(&dns, "dns", "build DNS server; repeatable")
	flags.StringVar(&cfg.BuildProxy, "build-proxy", "",
		"optional build-only HTTP proxy URL without credentials")
	if err := flags.Parse(args[1:]); err != nil {
		return onboardConfig{}, err
	}
	if flags.NArg() != 0 {
		return onboardConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	cfg.DNS = append([]string(nil), dns...)
	for _, required := range []struct {
		name  string
		value string
	}{
		{"-db", cfg.DBPath},
		{"-state-dir", cfg.StateDir},
		{"-commit", cfg.Commit},
		{"-base-ref", cfg.BaseRef},
		{"-base-image", cfg.BaseImage},
		{"-base-build-ref", cfg.BaseBuildRef},
		{"-review-config-digest", cfg.ReviewConfig},
	} {
		if required.value == "" {
			return onboardConfig{}, fmt.Errorf("%s is required", required.name)
		}
	}
	cfg.CommitPlan = domain.CommitPlanMode(commitPlan)
	if !slices.Contains(domain.AllCommitPlanModes, cfg.CommitPlan) {
		return onboardConfig{}, fmt.Errorf(
			"-commit-plan %q is invalid; valid values: %v",
			commitPlan, domain.AllCommitPlanModes,
		)
	}
	if cfg.RegistrationID <= 0 || cfg.RepositoryID <= 0 {
		return onboardConfig{}, errors.New("-registration-id and -repository-id must be positive")
	}
	if cfg.InstallWait <= 0 {
		return onboardConfig{}, errors.New("-install-wait must be positive")
	}
	if cfg.RecipePath == "" && cfg.SourceDir == "" {
		return onboardConfig{}, errors.New("-recipe or -source is required")
	}
	return cfg, nil
}

// withDurableInstallationPoll resumes a pending install-or-expansion intent
// through its durable §5.16 poll schedule: the throwaway janitor still
// reconciles coverage on its own bounded cadence (an operational probe, not
// the daemon's job), while readiness is observed by scheduler-fired
// occurrences whose expiry is the envelope's own bound, so the wait
// survives sessions instead of restarting with each invocation.
func withDurableInstallationPoll(
	ctx context.Context,
	st *store.Store,
	authority *publish.InstallationAuthorityStore,
	credentialsDir string,
	stateDir string,
	registrationID int64,
	run func(*publish.InstallationJanitor, *publish.Keystore, *http.Client) error,
) error {
	keystore, err := publish.NewKeystore(credentialsDir, stateDir)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	recorder, err := publish.NewStoreRecorder(st)
	if err != nil {
		return err
	}
	janitor, err := publish.NewInstallationJanitor(
		keystore, client, defaultGitHubAPIBase, authority, authority, recorder,
		time.Now, defaultJanitorRemovalBound,
	)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(runCtx, 500*time.Millisecond) }()
	fail := func(err error) error {
		cancel()
		return errors.Join(err, <-done)
	}

	snapshot, err := authority.InstallationAuthority(ctx, registrationID)
	if err != nil {
		return fail(err)
	}
	if snapshot.Pending == nil {
		return fail(errors.New("pending installation intent disappeared"))
	}
	sched, err := newInstallPollScheduler(st, authority, janitor)
	if err != nil {
		return fail(err)
	}
	if err := armInstallationPoll(ctx, sched, registrationID,
		snapshot.Pending.ActiveEpoch, snapshot.Pending.DurableIntentRevision,
		snapshot.Pending.ExpiresAt); err != nil {
		return fail(err)
	}

	waitCtx, stopWait := context.WithCancel(runCtx)
	defer stopWait()
	waited := make(chan error, 1)
	go func() {
		waited <- awaitInstallationPoll(waitCtx, st, sched, installPollScheduleID(registrationID))
	}()
	select {
	case err := <-done:
		stopWait()
		<-waited
		if err == nil {
			err = errors.New("installation janitor stopped before the grant matched")
		}
		return err
	case err := <-waited:
		if err != nil {
			return fail(err)
		}
	}
	runErr := run(janitor, keystore, client)
	cancel()
	return errors.Join(runErr, <-done)
}

func installationURLForRegistration(
	credentialsDir, stateDir string,
	registrationID int64,
) (string, error) {
	keystore, err := publish.NewKeystore(credentialsDir, stateDir)
	if err != nil {
		return "", err
	}
	apps, err := keystore.ListApps()
	if err != nil {
		return "", err
	}
	var registration *publish.AppRegistration
	for _, app := range apps {
		if app.AppID != registrationID {
			continue
		}
		if registration != nil {
			return "", errors.New("registration ID resolves to multiple local Apps")
		}
		candidate := app.Registration()
		registration = &candidate
	}
	if registration == nil {
		return "", fmt.Errorf("registration %d has no local App credentials", registrationID)
	}
	onboarder := publish.NewCredentialOnboarder(
		keystore,
		&http.Client{Timeout: 30 * time.Second},
		defaultGitHubAPIBase,
		defaultGitHubRemoteBase,
		time.Now,
		nil,
	)
	return onboarder.InstallationURL(*registration)
}

func detectRecipe(ctx context.Context, gitPath, sourceDir, commit string) ([]byte, error) {
	recipe, err := verify.ReadRecipeAtCommit(ctx, gitPath, sourceDir, commit)
	if err != nil {
		return nil, fmt.Errorf("detect recipe: %w", err)
	}
	return recipe, nil
}

func trustedInstallation(
	ctx context.Context,
	authority publish.InstallationAuthoritySource,
	localRegistrationIDs []int64,
	selectedRegistrationID, repositoryID int64,
) (*publish.TrustedInstallation, error) {
	var found *publish.TrustedInstallation
	var foundRegistrationID int64
	for _, registrationID := range localRegistrationIDs {
		snapshot, err := authority.InstallationAuthority(ctx, registrationID)
		if err != nil {
			return nil, err
		}
		for _, binding := range snapshot.TrustedInstallations {
			for _, id := range binding.RepositoryIDs {
				if id == repositoryID {
					if found != nil {
						return nil, fmt.Errorf(
							"repository %d is trusted by multiple local App installations: %w",
							repositoryID, publish.ErrAmbiguousInstallation,
						)
					}
					candidate := binding
					found = &candidate
					foundRegistrationID = registrationID
				}
			}
		}
	}
	if found != nil && foundRegistrationID != selectedRegistrationID {
		return nil, fmt.Errorf(
			"repository %d is already trusted under registration %d, not selected registration %d: %w",
			repositoryID, foundRegistrationID, selectedRegistrationID,
			publish.ErrAmbiguousInstallation,
		)
	}
	return found, nil
}

type onboardingJanitorGate struct {
	janitor *publish.InstallationJanitor
}

func (g onboardingJanitorGate) AllowsRepository(
	registrationID, installationID, repositoryID int64,
) bool {
	return g.janitor.AwaitAllowsRepository(
		registrationID, installationID, repositoryID,
	)
}

func (g onboardingJanitorGate) PendingReady(
	envelope publish.PendingInstallationEnvelope,
) (int64, bool) {
	return g.janitor.AwaitPendingReady(envelope)
}

func withInstallationJanitor(
	ctx context.Context,
	st *store.Store,
	authority *publish.InstallationAuthorityStore,
	credentialsDir string,
	stateDir string,
	wait time.Duration,
	ready func(*publish.InstallationJanitor) (bool, error),
	run func(*publish.InstallationJanitor, *publish.Keystore, *http.Client) error,
) error {
	if wait <= 0 {
		return errors.New("-install-wait must be positive")
	}
	keystore, err := publish.NewKeystore(credentialsDir, stateDir)
	if err != nil {
		return err
	}
	recorder, err := publish.NewStoreRecorder(st)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	janitor, err := publish.NewInstallationJanitor(
		keystore,
		client,
		defaultGitHubAPIBase,
		authority,
		authority,
		recorder,
		time.Now,
		defaultJanitorRemovalBound,
	)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(runCtx, 500*time.Millisecond) }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		isReady, readyErr := ready(janitor)
		if readyErr != nil {
			cancel()
			return errors.Join(readyErr, <-done)
		}
		if isReady {
			runErr := run(janitor, keystore, client)
			cancel()
			return errors.Join(runErr, <-done)
		}
		select {
		case err := <-done:
			if err == nil {
				return errors.New("installation janitor stopped before the grant matched")
			}
			return err
		case <-ticker.C:
		case <-timer.C:
			cancel()
			return errors.Join(
				fmt.Errorf("wait for selected installation: %w", context.DeadlineExceeded),
				<-done,
			)
		}
	}
}
