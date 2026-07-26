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
	OperatingMode string               `json:"operating_mode"`
	RunID         domain.RunID         `json:"run_id"`
	ItemID        domain.ItemID        `json:"item_id"`
	ItemType      domain.AttentionType `json:"item_type"`
	HeadSHA       string               `json:"head_sha,omitempty"`
	PRNumber      int                  `json:"pr_number,omitempty"`
}

func runFakePublicationCommand(
	ctx context.Context,
	cfg fakePublicationCommandConfig,
) (_ fakePublicationCommandResult, err error) {
	if err := cfg.withDefaultsAndValidate(); err != nil {
		return fakePublicationCommandResult{}, err
	}
	recipe, err := os.ReadFile(cfg.RecipeFile)
	if err != nil {
		return fakePublicationCommandResult{}, fmt.Errorf("read verification recipe: %w", err)
	}
	if _, err := verify.ParseRecipe(recipe); err != nil {
		return fakePublicationCommandResult{}, fmt.Errorf("verification recipe: %w", err)
	}
	recipeDigest := verify.RecipeDigest(recipe)
	approvedRecipes := map[domain.Digest]bool{recipeDigest: true}

	st, err := store.Open(ctx, cfg.DBPath, store.Options{ApprovedRecipes: approvedRecipes})
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	defer func() { err = errors.Join(err, st.Close()) }()

	driver, err := fake.NewStageDriverAt(cfg.FakeDriverDir)
	if err != nil {
		return fakePublicationCommandResult{}, fmt.Errorf("open fake stage driver: %w", err)
	}
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		return fakePublicationCommandResult{}, fmt.Errorf("open artifact store: %w", err)
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
	janitor, err := publish.NewInstallationJanitor(
		keystore, client, defaultGitHubAPIBase, authority, authority, time.Now,
		defaultJanitorRemovalBound,
	)
	if err != nil {
		return fakePublicationCommandResult{}, err
	}
	recorder, err := publish.NewStoreRecorder(st)
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
	workflow, err := engine.New(
		st, attention, autoScriptStageDriver{StageDriver: driver},
		engine.WithFakePublication(engine.FakePublicationConfig{
			WorkDir: cfg.WorkDir, Recipe: recipe, RecipePath: cfg.RecipeRepoPath,
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
		_, err := workflow.Reconcile(ctx)
		if err != nil {
			return fakePublicationCommandResult{}, err
		}
		if result, ok, err := existingFakePublicationResult(ctx, attention, cfg); err != nil {
			return fakePublicationCommandResult{}, err
		} else if ok {
			return result, nil
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

func (cfg *fakePublicationCommandConfig) withDefaultsAndValidate() error {
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
	paths := []struct {
		name string
		path *string
		dir  bool
	}{
		{"-publication-workspace", &cfg.WorkspaceDir, true},
		{"-db", &cfg.DBPath, false},
		{"-publication-work-dir", &cfg.WorkDir, true},
		{"-fake-stage-driver-dir", &cfg.FakeDriverDir, true},
		{"-publication-state-dir", &cfg.StateDir, true},
		{"-publication-credentials-dir", &cfg.CredentialsDir, true},
		{"-publication-recipe", &cfg.RecipeFile, false},
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
	workspace := strings.ToLower(cfg.WorkspaceDir)
	for _, candidate := range paths[1:] {
		target := strings.ToLower(*candidate.path)
		if publicationPathContains(workspace, target) ||
			(candidate.dir && publicationPathContains(target, workspace)) {
			return fmt.Errorf("%s overlaps -publication-workspace", candidate.name)
		}
	}
	blobDir = strings.ToLower(blobDir)
	if publicationPathContains(workspace, blobDir) ||
		publicationPathContains(blobDir, workspace) {
		return errors.New("-db artifact directory overlaps -publication-workspace")
	}
	for i, pattern := range cfg.AllowedPaths {
		cfg.AllowedPaths[i] = strings.TrimSpace(pattern)
		if cfg.AllowedPaths[i] == "" {
			return errors.New("publication allowed path is empty")
		}
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
		{domain.ItemID("ready-" + string(cfg.RunID)), domain.AttentionReadyForFinalReview},
		{domain.ItemID("publish-blocked-" + string(cfg.RunID)), domain.AttentionPublishBlocked},
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
			OperatingMode: engine.OperatingModeAttendedDev,
			RunID:         cfg.RunID,
			ItemID:        terminal.id,
			ItemType:      terminal.itemType,
			HeadSHA:       item.PRHeadSHA,
		}
		if terminal.itemType == domain.AttentionReadyForFinalReview {
			prefix := cfg.Repo + "#"
			const suffix = " is published and ready for final review."
			if !strings.HasPrefix(item.Reason, prefix) || !strings.HasSuffix(item.Reason, suffix) {
				return fakePublicationCommandResult{}, false,
					fmt.Errorf("terminal publication item %q has invalid ready reason", terminal.id)
			}
			number := strings.TrimSuffix(strings.TrimPrefix(item.Reason, prefix), suffix)
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
