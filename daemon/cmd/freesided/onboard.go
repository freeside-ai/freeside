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

func runOnboardCommand(
	ctx context.Context, args []string, stdout, stderr io.Writer,
) (err error) {
	if len(args) == 0 {
		return errors.New("repository owner/name is required")
	}
	repository := args[0]
	flags := flag.NewFlagSet("freesided onboard", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	stateDir := flags.String("state-dir", "", "GitHub App authority state directory (required)")
	registrationID := flags.Int64("registration-id", 0, "selected numeric GitHub App ID (required)")
	repositoryID := flags.Int64("repository-id", 0, "canonical numeric repository ID (required)")
	account := flags.String("account", "", "canonical repository-owning account login")
	accountID := flags.Int64("account-id", 0, "canonical numeric repository-owning account ID")
	installationID := flags.Int64("installation-id", 0, "selected installation ID; zero before GitHub assigns one")
	resume := flags.Bool("resume", false, "resume the existing bounded installation intent")
	installWait := flags.Duration("install-wait", 10*time.Minute, "maximum wait for native installation approval")
	credentialsDir := flags.String(
		"credentials-dir", "",
		"GitHub App credentials directory (defaults beside the state directory)")
	commit := flags.String("commit", "", "exact full lowercase repository commit (required)")
	baseRef := flags.String("base-ref", "", "branch whose live workflow authority is reviewed (required)")
	recipePath := flags.String("recipe", "", "detected trusted verification recipe (required)")
	sourceDir := flags.String(
		"source", "",
		"repository checkout used to detect .freeside/verify.json when -recipe is omitted")
	baseImage := flags.String("base-image", "", "digest-pinned approved agent base (required)")
	baseBuildRef := flags.String("base-build-ref", "", "local base tag matching -base-image (required)")
	reviewConfig := flags.String("review-config-digest", "", "digest of the reviewed automated-review configuration (required)")
	approval := flags.String("approve", "", "exact proposed review digest; omit for the one-time review")
	registry := flags.String("registry", "", "registry host/path destination")
	localRegistryPort := flags.Int("local-registry-port", 0, "managed loopback registry port")
	imageName := flags.String("image-name", "", "project image name")
	refTag := flags.String("ref-tag", "v1", "one-shot image tag prefix")
	gitPath := flags.String("git", "", "git executable (default from PATH)")
	containerPath := flags.String("container", "", "Apple container executable (default from PATH)")
	tempDir := flags.String("temp-dir", "", "bindable scratch parent")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	for _, required := range []struct {
		name  string
		value string
	}{
		{"-db", *dbPath},
		{"-state-dir", *stateDir},
		{"-commit", *commit},
		{"-base-ref", *baseRef},
		{"-base-image", *baseImage},
		{"-base-build-ref", *baseBuildRef},
		{"-review-config-digest", *reviewConfig},
	} {
		if required.value == "" {
			return fmt.Errorf("%s is required", required.name)
		}
	}
	if *registrationID <= 0 || *repositoryID <= 0 {
		return errors.New("-registration-id and -repository-id must be positive")
	}
	if *installWait <= 0 {
		return errors.New("-install-wait must be positive")
	}
	var recipe []byte
	if *recipePath == "" {
		if *sourceDir == "" {
			return errors.New("-recipe or -source is required")
		}
		recipe, err = detectRecipe(ctx, *gitPath, *sourceDir, *commit)
	} else {
		recipe, err = os.ReadFile(*recipePath)
	}
	if err != nil {
		return err
	}
	if _, err := verify.ParseRecipe(recipe); err != nil {
		return fmt.Errorf("detected recipe: %w", err)
	}
	st, err := store.Open(ctx, *dbPath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	authority, err := publish.NewInstallationAuthorityStore(*stateDir)
	if err != nil {
		return err
	}
	if *credentialsDir == "" {
		*credentialsDir = filepath.Join(filepath.Dir(*stateDir), "credentials")
	}
	keystore, err := publish.NewKeystore(*credentialsDir, *stateDir)
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
		ctx, authority, registrationIDs, *registrationID, *repositoryID,
	)
	if err != nil {
		return err
	}
	if trusted == nil {
		if *account == "" || *accountID <= 0 || *installationID < 0 {
			return errors.New(
				"-account, positive -account-id, and non-negative -installation-id " +
					"are required until the repository is trusted")
		}
		intent := operations.InstallationIntentRequest{
			RegistrationID: *registrationID,
			Account:        *account,
			AccountID:      *accountID,
			InstallationID: *installationID,
			RepositoryID:   *repositoryID,
			ExpiresAt:      time.Now().UTC().Add(*installWait),
		}
		if !*resume {
			installURL, err := installationURLForRegistration(
				*credentialsDir, *stateDir, *registrationID,
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
			*registrationID, *repositoryID, time.Now,
		)
		builder, builderErr := projectimage.New(projectimage.Options{
			GitPath: *gitPath, ContainerPath: *containerPath,
			TempDir: *tempDir, Log: stderr, Tokens: tokens,
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
			Repository: repository, RepositoryID: *repositoryID,
			RegistrationID: *registrationID,
			BaseRef:        *baseRef,
			ApprovalDigest: domain.Digest(*approval),
			Policy: operations.OnboardPolicy{
				PRExecution:    domain.PRExecutionAuditedSameRepo,
				CommitPlan:     domain.CommitPlanSingleCommit,
				MessageRuleset: domain.MessageRulesetGitHub1,
				ReviewMode:     domain.ReviewFreesideInvoked,
				ReviewConfig:   domain.Digest(*reviewConfig),
			},
			Image: projectimage.Request{
				Repository: repository, RepositoryID: *repositoryID,
				CommitSHA: *commit, Recipe: recipe,
				BaseImageRef: domain.ImageRef(*baseImage), BaseBuildRef: *baseBuildRef,
				Registry: *registry, LocalRegistryPort: *localRegistryPort,
				ImageName: *imageName, RefTag: *refTag,
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
			ctx, st, authority, *credentialsDir, *stateDir, *installWait,
			func(janitor *publish.InstallationJanitor) (bool, error) {
				return janitor.AllowsRepository(
					*registrationID, trusted.InstallationID, *repositoryID,
				), nil
			},
			runOnboard,
		)
	}
	return withDurableInstallationPoll(
		ctx, st, authority, *credentialsDir, *stateDir, *registrationID, runOnboard,
	)
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
