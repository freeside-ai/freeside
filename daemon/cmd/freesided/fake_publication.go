package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const (
	defaultGitHubAPIBase       = "https://api.github.com"
	defaultGitHubRemoteBase    = "https://github.com"
	defaultJanitorInterval     = 30 * time.Second
	defaultJanitorRemovalBound = 10
	fakePublicationIsolation   = "process_local"
)

type fakePublicationCommandConfig struct {
	DBPath            string
	FakeDriverDir     string
	StateDir          string
	CredentialsDir    string
	WorkDir           string
	WorkspaceDir      string
	RecipeFile        string
	RecipeRepoPath    string
	Repo              string
	BaseRef           string
	BaseSHA           string
	AllowedPaths      []string
	RunID             domain.RunID
	ProjectID         domain.ProjectID
	Title             string
	Body              string
	ReconcileInterval time.Duration
	JanitorInterval   time.Duration
}

type fakePublicationCommandResult struct {
	OperatingMode  string               `json:"operating_mode"`
	IsolationClass string               `json:"isolation_class"`
	RunID          domain.RunID         `json:"run_id"`
	ItemID         domain.ItemID        `json:"item_id"`
	ItemType       domain.AttentionType `json:"item_type"`
	HeadSHA        string               `json:"head_sha,omitempty"`
	PRNumber       int                  `json:"pr_number,omitempty"`
}

func runFakePublicationCommand(
	ctx context.Context,
	cfg fakePublicationCommandConfig,
) (_ fakePublicationCommandResult, err error) {
	replayFound, err := prepareFakePublicationConfig(
		ctx, &cfg, fakePublicationReplayBinding,
	)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		return fakePublicationCommandResult{}, fmt.Errorf("open artifact store: %w", err)
	}
	replay, loadedReplay, err := fakePublicationReplay(ctx, cfg, blobs)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	var recipe []byte
	if replayFound {
		if !loadedReplay || replay.WorkspaceDir != cfg.WorkspaceDir ||
			replay.WorkDir != cfg.WorkDir {
			return fakePublicationCommandResult{}, errors.New(
				"durable publication replay changed during bootstrap",
			)
		}
		recipe = replay.Recipe
	} else {
		if loadedReplay {
			return fakePublicationCommandResult{}, errors.New(
				"durable publication replay appeared during bootstrap; retry the command",
			)
		}
		recipe, err = os.ReadFile(cfg.RecipeFile)
		if err != nil {
			return fakePublicationCommandResult{}, fmt.Errorf("read verification recipe: %w", err)
		}
		if _, err := verify.ParseRecipe(recipe); err != nil {
			return fakePublicationCommandResult{}, fmt.Errorf("verification recipe: %w", err)
		}
	}
	recipeDigest := verify.RecipeDigest(recipe)
	approvedRecipes := map[domain.Digest]bool{recipeDigest: true}

	st, _, err := openStoreWithTopicKey(
		ctx, cfg.DBPath, store.Options{ApprovedRecipes: approvedRecipes},
	)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	defer func() { err = errors.Join(err, st.Close()) }()

	driver, err := fake.NewStageDriverAt(cfg.FakeDriverDir)
	if err != nil {
		return fakePublicationCommandResult{}, fmt.Errorf("open fake stage driver: %w", err)
	}
	attention := signet.NewService(st, signet.WithBlobStore(blobs))
	client := &http.Client{Timeout: 30 * time.Second}
	keystore, err := publish.NewKeystore(cfg.CredentialsDir, cfg.StateDir)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	authority, err := publish.NewInstallationAuthorityStore(cfg.StateDir)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	recorder, err := publish.NewStoreRecorder(st)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	janitor, err := publish.NewInstallationJanitor(
		keystore, client, defaultGitHubAPIBase, authority, authority, recorder, time.Now,
		defaultJanitorRemovalBound,
	)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	trust, err := publish.NewStoreTrustSource(st)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	minter := publish.NewMinterWithJanitor(
		keystore, client, defaultGitHubAPIBase, recorder, trust, time.Now, janitor,
	)
	tokens := publish.NewCachedTokenSource(minter, time.Now)
	gitTransport, err := publish.NewTransport(tokens, publish.TransportOptions{
		RemoteBase: defaultGitHubRemoteBase,
	})
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	transport, err := engine.NewGitPublicationTransport(gitTransport)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	auditor, err := publish.NewGitHubWorkflowAuditor(
		tokens, client, defaultGitHubAPIBase, time.Now,
	)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	ledger, err := publish.NewStoreLedger(st)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	authz, err := publish.NewStoreAuthorizationSource(st)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	publisher := publish.NewPublisher(
		tokens, client, defaultGitHubAPIBase, auditor, ledger, trust, authz,
	)
	// Claim the transport's single publication authority for this
	// publisher: from here on its gate verdicts are the only ones that
	// transport's PushHead honours, and the claim cannot be displaced.
	if err := gitTransport.AuthorizePublisher(publisher); err != nil {
		return fakePublicationCommandResult{}, err
	}
	workflow, err := engine.New(
		st, attention, autoScriptStageDriver{StageDriver: driver},
		engine.WithFakePublication(engine.FakePublicationConfig{
			WorkDir: cfg.WorkDir, Recipe: recipe, RecipePath: cfg.RecipeRepoPath,
			ProtectedRoots: []string{
				cfg.DBPath, cfg.DBPath + ".blobs", cfg.FakeDriverDir,
				cfg.StateDir, cfg.CredentialsDir, cfg.RecipeFile,
			},
			ApprovedRecipes: approvedRecipes, Transport: transport, Publisher: publisher,
			Artifacts: blobs,
			NewRoom:   func(home string) verify.Room { return &verify.ProcRoom{Home: home} },
			Now:       time.Now,
		}),
	)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}

	_, err = workflow.StartFakePublication(ctx, engine.FakePublicationSpec{
		RunID: cfg.RunID, ProjectID: cfg.ProjectID, WorkspaceDir: cfg.WorkspaceDir,
		Repo: cfg.Repo, BaseRef: cfg.BaseRef, BaseSHA: cfg.BaseSHA,
		AllowedPaths:             cfg.AllowedPaths,
		VerificationInvocationID: domain.InvocationID("verify-" + string(cfg.RunID)),
		PublicationInvocationID:  domain.InvocationID("publish-" + string(cfg.RunID)),
		Title:                    cfg.Title, Body: cfg.Body, OperatingMode: engine.OperatingModeAttendedDev,
	})
	if err != nil {
		return fakePublicationCommandResult{}, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	janitorDone := make(chan struct{})
	var janitorRunErr error
	go func() {
		janitorRunErr = janitor.Run(runCtx, cfg.JanitorInterval)
		close(janitorDone)
	}()
	defer func() {
		cancel()
		<-janitorDone
		if err == nil && janitorRunErr != nil {
			err = fmt.Errorf("installation janitor: %w", janitorRunErr)
		}
	}()

	ticker := time.NewTicker(cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		_, reconcileErr := workflow.ReconcileFakePublication(ctx, cfg.RunID)
		terminalReadable, err := fakePublicationTerminalReadable(reconcileErr)
		if err != nil {
			return fakePublicationCommandResult{}, err
		}
		// A dispatched replay must be recovered under active janitor coverage
		// before its durable terminal item can be returned.
		if terminalReadable {
			if result, ok, err := existingFakePublicationResult(ctx, attention, cfg); err != nil {
				return fakePublicationCommandResult{}, err
			} else if ok {
				replay, found, stateErr := engine.LoadFakePublicationReplay(
					ctx, st, blobs, cfg.RunID,
				)
				if stateErr != nil {
					return fakePublicationCommandResult{}, stateErr
				}
				if !found || !replay.Dispatched {
					return fakePublicationCommandResult{}, fmt.Errorf(
						"terminal publication task %q is not dispatched", cfg.RunID,
					)
				}
				return result, nil
			}
		}
		select {
		case <-janitorDone:
			if janitorRunErr != nil {
				return fakePublicationCommandResult{}, fmt.Errorf("installation janitor: %w", janitorRunErr)
			}
			return fakePublicationCommandResult{}, errors.New("installation janitor stopped")
		case <-ctx.Done():
			return fakePublicationCommandResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func fakePublicationTerminalReadable(reconcileErr error) (bool, error) {
	if reconcileErr == nil {
		return true, nil
	}
	if errors.Is(reconcileErr, publish.ErrJanitorInactive) {
		return false, nil
	}
	return false, reconcileErr
}

type fakePublicationReplayBindingLoader func(
	context.Context,
	fakePublicationCommandConfig,
) (engine.FakePublicationReplayBinding, bool, error)

func prepareFakePublicationConfig(
	ctx context.Context,
	cfg *fakePublicationCommandConfig,
	load fakePublicationReplayBindingLoader,
) (bool, error) {
	if err := cfg.withDefaults(); err != nil {
		return false, err
	}
	binding, found, err := load(ctx, *cfg)
	if err != nil {
		return false, err
	}
	if found {
		cfg.WorkspaceDir = binding.WorkspaceDir
		cfg.WorkDir = binding.WorkDir
	}
	if err := cfg.resolveAndValidatePaths(!found); err != nil {
		return false, err
	}
	return found, nil
}

func fakePublicationReplayBinding(
	ctx context.Context,
	cfg fakePublicationCommandConfig,
) (_ engine.FakePublicationReplayBinding, _ bool, err error) {
	if _, err := os.Stat(cfg.DBPath); errors.Is(err, os.ErrNotExist) {
		return engine.FakePublicationReplayBinding{}, false, nil
	} else if err != nil {
		return engine.FakePublicationReplayBinding{}, false, err
	}
	bootstrap, _, err := openStoreWithTopicKey(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		return engine.FakePublicationReplayBinding{}, false, err
	}
	defer func() { err = errors.Join(err, bootstrap.Close()) }()

	return engine.LoadFakePublicationReplayBinding(ctx, bootstrap, cfg.RunID)
}

func fakePublicationReplay(
	ctx context.Context,
	cfg fakePublicationCommandConfig,
	artifacts engine.ArtifactStore,
) (_ engine.FakePublicationReplay, _ bool, err error) {
	bootstrap, _, err := openStoreWithTopicKey(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		return engine.FakePublicationReplay{}, false, err
	}
	defer func() { err = errors.Join(err, bootstrap.Close()) }()

	return engine.LoadFakePublicationReplay(ctx, bootstrap, artifacts, cfg.RunID)
}

func (cfg *fakePublicationCommandConfig) withDefaultsAndValidate() error {
	if err := cfg.withDefaults(); err != nil {
		return err
	}
	return cfg.resolveAndValidatePaths(true)
}

func (cfg *fakePublicationCommandConfig) withDefaults() error {
	switch {
	case cfg.DBPath == "":
		return errors.New("-db is required")
	case cfg.StateDir == "":
		return errors.New("-publication-state-dir is required")
	case cfg.CredentialsDir == "":
		return errors.New("-publication-credentials-dir is required")
	case cfg.WorkspaceDir == "":
		return errors.New("-publication-workspace is required")
	case cfg.RecipeFile == "":
		return errors.New("-publication-recipe is required")
	case cfg.Repo == "":
		return errors.New("-publication-repo is required")
	case cfg.BaseRef == "":
		return errors.New("-publication-base-ref is required")
	case cfg.BaseSHA == "":
		return errors.New("-publication-base-sha is required")
	}
	if cfg.RunID == "" {
		cfg.RunID = "run-fake-publication"
	}
	if cfg.ProjectID == "" {
		cfg.ProjectID = "project-fake-publication"
	}
	if cfg.Title == "" {
		cfg.Title = "Publish attended fake candidate"
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = cfg.DBPath + ".publication"
	}
	if cfg.FakeDriverDir == "" {
		cfg.FakeDriverDir = cfg.DBPath + ".fake-stage-driver"
	}
	if cfg.ReconcileInterval == 0 {
		cfg.ReconcileInterval = defaultReconcileInterval
	}
	if cfg.JanitorInterval == 0 {
		cfg.JanitorInterval = defaultJanitorInterval
	}
	if cfg.RecipeRepoPath == "" {
		cfg.RecipeRepoPath = verify.DefaultRecipePath
	}
	if cfg.ReconcileInterval < 0 || cfg.JanitorInterval < 0 {
		return errors.New("publication intervals must be positive")
	}
	if len(cfg.AllowedPaths) == 0 {
		cfg.AllowedPaths = []string{"**"}
	}
	for i, pattern := range cfg.AllowedPaths {
		cfg.AllowedPaths[i] = strings.TrimSpace(pattern)
		if cfg.AllowedPaths[i] == "" {
			return errors.New("publication allowed path is empty")
		}
	}
	return nil
}

func (cfg *fakePublicationCommandConfig) resolveAndValidatePaths(resolveAmbientRecipe bool) error {
	type publicationPath struct {
		name string
		path *string
		dir  bool
	}
	paths := []publicationPath{
		{"-publication-workspace", &cfg.WorkspaceDir, true},
		{"-db", &cfg.DBPath, false},
		{"-publication-work-dir", &cfg.WorkDir, true},
		{"-fake-stage-driver-dir", &cfg.FakeDriverDir, true},
		{"-publication-state-dir", &cfg.StateDir, true},
		{"-publication-credentials-dir", &cfg.CredentialsDir, true},
	}
	if resolveAmbientRecipe {
		paths = append(paths, publicationPath{"-publication-recipe", &cfg.RecipeFile, false})
	}
	for _, candidate := range paths {
		resolved, err := resolvePublicationPath(*candidate.path)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", candidate.name, err)
		}
		*candidate.path = resolved
	}
	blobDir, err := resolvePublicationPath(cfg.DBPath + ".blobs")
	if err != nil {
		return fmt.Errorf("resolve -db artifact directory: %w", err)
	}
	foldCase := publicationFilesystemCaseInsensitive(cfg.WorkspaceDir)
	pathKey := func(path string) string {
		if foldCase {
			return strings.ToLower(path)
		}
		return path
	}
	workspace := pathKey(cfg.WorkspaceDir)
	for _, candidate := range paths[1:] {
		target := pathKey(*candidate.path)
		if publicationPathContains(workspace, target) ||
			(candidate.dir && publicationPathContains(target, workspace)) {
			return fmt.Errorf("%s overlaps -publication-workspace", candidate.name)
		}
	}
	blobDir = pathKey(blobDir)
	if publicationPathContains(workspace, blobDir) ||
		publicationPathContains(blobDir, workspace) {
		return errors.New("-db artifact directory overlaps -publication-workspace")
	}
	return nil
}

func existingFakePublicationResult(
	ctx context.Context,
	attention *signet.Service,
	cfg fakePublicationCommandConfig,
) (fakePublicationCommandResult, bool, error) {
	terminalItems := []struct {
		id       domain.ItemID
		itemType domain.AttentionType
	}{
		{engine.FakePublicationReadyItemID(cfg.RunID), domain.AttentionReadyForFinalReview},
		{engine.FakePublicationBlockedItemID(cfg.RunID), domain.AttentionPublishBlocked},
	}
	var found *fakePublicationCommandResult
	for _, terminal := range terminalItems {
		snapshot, err := attention.GetAttentionItem(ctx, terminal.id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return fakePublicationCommandResult{}, false, err
		}
		item := snapshot.Item
		if item.ID != terminal.id || item.ProjectID != cfg.ProjectID ||
			item.Type != terminal.itemType || item.Subject.Type != domain.SubjectRun ||
			item.Subject.ID != domain.SubjectID(cfg.RunID) || item.Subject.RunID == nil ||
			*item.Subject.RunID != cfg.RunID {
			return fakePublicationCommandResult{}, false,
				fmt.Errorf("terminal publication item %q does not match run", terminal.id)
		}
		result := fakePublicationCommandResult{
			OperatingMode:  engine.OperatingModeAttendedDev,
			IsolationClass: fakePublicationIsolation,
			RunID:          cfg.RunID,
			ItemID:         terminal.id,
			ItemType:       terminal.itemType,
			HeadSHA:        item.PRHeadSHA,
		}
		if terminal.itemType == domain.AttentionReadyForFinalReview {
			reason, _, ok := engine.ParseFakePublicationTerminalReason(item.Reason)
			if !ok {
				return fakePublicationCommandResult{}, false,
					fmt.Errorf("terminal publication item %q has no task binding", terminal.id)
			}
			prefix := cfg.Repo + "#"
			const suffix = " is published and ready for final review."
			if !strings.HasPrefix(reason, prefix) || !strings.HasSuffix(reason, suffix) {
				return fakePublicationCommandResult{}, false,
					fmt.Errorf("terminal publication item %q has invalid ready reason", terminal.id)
			}
			number := strings.TrimSuffix(strings.TrimPrefix(reason, prefix), suffix)
			prNumber, err := strconv.Atoi(number)
			if err != nil || prNumber <= 0 {
				return fakePublicationCommandResult{}, false,
					fmt.Errorf("terminal publication item %q has invalid pull request number", terminal.id)
			}
			result.PRNumber = prNumber
		}
		if found != nil {
			return fakePublicationCommandResult{}, false,
				fmt.Errorf("run %q has multiple terminal publication items", cfg.RunID)
		}
		found = &result
	}
	if found != nil {
		return *found, true, nil
	}
	return fakePublicationCommandResult{}, false, nil
}

func resolvePublicationPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for current := filepath.Clean(abs); ; {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %s", abs)
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
}

func publicationPathContains(outer, inner string) bool {
	relative, err := filepath.Rel(outer, inner)
	if err != nil {
		return false
	}
	return relative == "." ||
		(relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func publicationFilesystemCaseInsensitive(path string) bool {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			base := filepath.Base(current)
			for i, char := range []byte(base) {
				var alternate byte
				switch {
				case char >= 'a' && char <= 'z':
					alternate = char - ('a' - 'A')
				case char >= 'A' && char <= 'Z':
					alternate = char + ('a' - 'A')
				default:
					continue
				}
				changed := []byte(base)
				changed[i] = alternate
				other, err := os.Stat(filepath.Join(filepath.Dir(current), string(changed)))
				return err == nil && os.SameFile(info, other)
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}
