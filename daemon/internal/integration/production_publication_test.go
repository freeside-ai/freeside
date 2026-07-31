package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

type productionRoom struct {
	recipe []byte
	read   func(context.Context) ([]byte, error)
	reads  int
	runs   int
	fail   bool
}

func (r *productionRoom) ReadRecipe(ctx context.Context) ([]byte, error) {
	r.reads++
	if r.read != nil {
		return r.read(ctx)
	}
	return bytes.Clone(r.recipe), nil
}

func (r *productionRoom) Run(
	_ context.Context, _ string, _ []string,
) (verify.StepResult, error) {
	r.runs++
	if r.fail {
		return verify.StepResult{ExitCode: 1, Output: []byte("injected verification failure\n")}, nil
	}
	return verify.StepResult{}, nil
}

type productionCrashSeams struct {
	afterVerification func() error
	afterPublication  func() error
	afterReady        func() error
	afterBlocked      func() error
	afterTerminal     func() error
	afterLockRelease  func() error
}

type productionPublicationHarness struct {
	*publicationHarness
	runID             domain.RunID
	projectID         domain.ProjectID
	image             domain.ProjectImage
	driver            *fake.StageDriver
	replay            engine.ProductionReplay
	room              *productionRoom
	workflow          *engine.Engine
	invocation        domain.InvocationID
	recipeReadTimeout time.Duration
}

func newProductionPublicationHarness(t *testing.T, resultHead string) *productionPublicationHarness {
	t.Helper()
	h := newPublicationHarness(t)
	// Production verification reads the onboarding-bound external recipe
	// embedded in the project image. The managed repository intentionally has
	// no in-tree .freeside/verify.json.

	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: fakePublicationRepo, RepositoryID: h.profile.RepositoryID,
		CommitSHA: h.baseSHA, RecipeDigest: h.recipeD,
		PreparationCommand: []string{"/usr/bin/true"},
		BaseImageRef:       domain.ImageRef("ghcr.io/freeside-ai/base@sha256:" + strings.Repeat("1", 64)),
		ImageRef:           domain.ImageRef("ghcr.io/freeside-ai/project@sha256:" + strings.Repeat("2", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.WriteInternal(h.ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordAuthIdentity(h.ctx, testIdentity, fakePublicationTime); err != nil {
			return err
		}
		if err := tx.RecordProjectImage(h.ctx, image); err != nil {
			return err
		}
		backend := productionPublicationBackend(t)
		conformance, err := domain.NewBackendConformance(domain.BackendConformanceInput{
			Backend: domain.BackendFreshVMReadOnlyVolumeHandoff,
			Outcome: domain.ConformancePassed, ConfigurationDigest: backend.ConfigurationDigest(),
			Capabilities: conformantCeiling(t), ProvedAt: fakePublicationTime,
		})
		if err != nil {
			return err
		}
		_, err = tx.RecordBackendConformance(h.ctx, conformance)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	replay := buildProductionReplay(t, h)
	if resultHead == "" {
		resultHead = replay.HeadSHA
	}
	runID := domain.RunID("run-production-publication")
	projectID := domain.ProjectID("project-production-publication")
	spec, policy, resolved := registerSubmissionArtifactsWithPaths(
		t, h.store, string(runID), "README.md",
	)
	submitted, err := engine.SubmitProductionRun(h.ctx, h.store, engine.ProductionRunSpec{
		RunID: runID, ProjectID: projectID, SpecArtifactID: spec.ID,
		PolicyArtifactID: policy.ID, ResolvedPolicy: resolved,
		Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewStageDriverAt(filepath.Join(h.workDir, "production-driver"))
	if err != nil {
		t.Fatal(err)
	}
	driver.Script(submitted.InvocationID, fake.StageScript{
		PendingInspects: 1, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{HeadSHA: resultHead, Summary: "Claude export completed."},
	})
	p := &productionPublicationHarness{
		publicationHarness: h, runID: runID, projectID: projectID,
		image: image, driver: driver, replay: replay,
		room:       &productionRoom{recipe: bytes.Clone(h.recipe)},
		invocation: submitted.InvocationID,
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	return p
}

func productionPublicationBackend(t *testing.T) fake.RunnerBackend {
	t.Helper()
	return fake.RunnerBackend{
		BackendName: string(domain.BackendFreshVMReadOnlyVolumeHandoff),
		Caps:        exec.NewCapabilitySet(conformantCeiling(t)...),
	}
}

func buildProductionReplay(t *testing.T, h *publicationHarness) engine.ProductionReplay {
	t.Helper()
	workspace := t.TempDir()
	writeFile(t, workspace, "README.md", "production change\n")
	handoff := filepath.Join(t.TempDir(), "handoff")
	manifest, err := export.Export(os.DirFS(workspace), handoff, export.Options{})
	if err != nil {
		t.Fatal(err)
	}
	manifestBody, err := manifest.Encode()
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := productionDigest(manifestBody)
	putProductionBlob(t, h, manifestDigest, manifestBody)
	for _, entry := range manifest.Entries {
		if entry.Kind != export.EntryRegular || entry.Digest == nil {
			continue
		}
		digest := domain.Digest(*entry.Digest)
		hexDigits := strings.TrimPrefix(string(digest), "sha256:")
		body, err := os.ReadFile(filepath.Join(handoff, "blobs", "sha256", hexDigits)) //nolint:gosec // test-owned export path and digest
		if err != nil {
			t.Fatal(err)
		}
		putProductionBlob(t, h, digest, body)
	}
	checkout := filepath.Join(t.TempDir(), "checkout")
	runGit(t, h.baseDir, "clone", "-q", "--no-hardlinks", ".", checkout)
	policy, err := (importer.Policy{Allowlist: []string{"README.md"}}).WithProtectedPaths(h.profile)
	if err != nil {
		t.Fatal(err)
	}
	options := importer.Options{
		BaseSHA: h.baseSHA, CommitDate: fakePublicationTime,
		AuthorName:  productionPublicationMetadata().CommitAuthor.Name(),
		AuthorEmail: productionPublicationMetadata().CommitAuthor.Email(),
		Policy:      policy,
	}
	imported, err := importer.Import(t.Context(), handoff, checkout, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.Findings) != 0 || imported.CommitSHA == "" {
		t.Fatalf("production replay import = %#v", imported)
	}
	author := strings.TrimSpace(runGit(
		t, checkout, "show", "-s", "--format=%an <%ae>", imported.CommitSHA,
	))
	wantAuthor := productionPublicationMetadata().CommitAuthor
	if author != wantAuthor.Name()+" <"+wantAuthor.Email()+">" {
		t.Fatalf("production commit author = %q, want App bot attribution", author)
	}
	return engine.ProductionReplay{
		InvocationID:    "inv-implement-run-production-publication",
		ObservedBaseSHA: h.baseSHA, HeadSHA: imported.CommitSHA,
		Manifest: manifest, ManifestDigest: manifestDigest, ImportOptions: options,
	}
}

func productionDigest(body []byte) domain.Digest {
	sum := sha256.Sum256(body)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func putProductionBlob(t *testing.T, h *publicationHarness, digest domain.Digest, body []byte) {
	t.Helper()
	if _, err := h.blobs.Put(digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func (p *productionPublicationHarness) newPublisher(t *testing.T) *publish.Publisher {
	t.Helper()
	ledger, err := publish.NewStoreLedger(p.store)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := publish.NewStoreTrustSource(p.store)
	if err != nil {
		t.Fatal(err)
	}
	authorizations, err := publish.NewStoreAuthorizationSource(p.store)
	if err != nil {
		t.Fatal(err)
	}
	tokens := p.tokens
	if tokens == nil {
		tokens = integrationTokenSource{}
	}
	return publish.NewPublisher(
		tokens, p.server.Client(), p.server.URL,
		integrationAuditor{audit: p.audit}, ledger, trust, authorizations,
	)
}

func (p *productionPublicationHarness) newEngine(
	t *testing.T, seams productionCrashSeams, withPublication bool,
) *engine.Engine {
	t.Helper()
	return p.newEngineWithApprovedRecipes(
		t, seams, withPublication, map[domain.Digest]bool{p.recipeD: true},
	)
}

func (p *productionPublicationHarness) newEngineWithApprovedRecipes(
	t *testing.T,
	seams productionCrashSeams,
	withPublication bool,
	approvedRecipes map[domain.Digest]bool,
) *engine.Engine {
	t.Helper()
	return p.newEngineForMode(
		t, seams, withPublication, approvedRecipes, domain.ModeUnattended, false,
	)
}

func (p *productionPublicationHarness) newEngineForMode(
	t *testing.T,
	seams productionCrashSeams,
	withPublication bool,
	approvedRecipes map[domain.Digest]bool,
	mode domain.OperatingMode,
	holdOnly bool,
) *engine.Engine {
	t.Helper()
	identity := testIdentity.ID
	environment := engine.AdmissionEnvironment{
		OperatingMode:  mode,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly, ImageRef: p.image.ImageRef,
		PromptPackageDigest: productionDigest([]byte("prompt package")),
		VendorInstructions: engine.VendorInstructionConfig{
			Vendor: domain.AgentVendorClaude, HostPath: "/nonexistent/production-publication-claude-md",
		},
		AuthIdentityID: &identity,
	}
	options := []engine.Option{
		engine.WithAdmission(
			productionPublicationBackend(t), nil,
			environment, func() time.Time { return fakePublicationTime },
		),
		engine.WithAdmissionDerivation(func(
			_ context.Context, _ domain.InvocationID,
		) (string, domain.BaseRevision, error) {
			return "freeside-handoff-production-publication-ws", domain.BaseRevision{
				Repo: fakePublicationRepo, RepositoryID: p.profile.RepositoryID,
				BaseRef: "main", BaseSHA: p.baseSHA,
			}, nil
		}),
	}
	if withPublication {
		options = append(options, engine.WithProductionPublication(engine.ProductionPublicationConfig{
			WorkDir:   filepath.Join(p.workDir, "production-publication"),
			Transport: p.transport, Publisher: p.newPublisher(t), Artifacts: p.blobs,
			ApprovedRecipes:   approvedRecipes,
			HoldOnly:          holdOnly,
			RecipeReadTimeout: p.recipeReadTimeout,
			HoldRetryInterval: time.Minute,
			Now:               func() time.Time { return p.now },
			NewRoom: func(image domain.ProjectImage) (engine.ProductionVerificationRoom, error) {
				if image.ID != p.image.ID {
					return nil, domain.ErrParentKeyMismatch
				}
				return p.room, nil
			},
			AfterVerification: seams.afterVerification,
			AfterPublication:  seams.afterPublication,
			AfterReady:        seams.afterReady, AfterBlocked: seams.afterBlocked,
			AfterTerminal:        seams.afterTerminal,
			AfterTaskLockRelease: seams.afterLockRelease,
		}))
	}
	workflow, err := engine.New(p.store, p.attention, p.driver, options...)
	if err != nil {
		t.Fatal(err)
	}
	return workflow
}

func (p *productionPublicationHarness) startAndRecordExport(t *testing.T) {
	t.Helper()
	record := p.startExecutionExport(t, p.replay.HeadSHA)
	if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, p.replay); err != nil {
		t.Fatal(err)
	}
}

func (p *productionPublicationHarness) startExecutionExport(
	t *testing.T,
	headSHA string,
) domain.ExecutionExport {
	t.Helper()
	result, err := p.workflow.Reconcile(p.ctx)
	if err != nil {
		t.Fatalf("start production invocation: %v", err)
	}
	if result.InvocationsStarted != 1 {
		t.Fatalf("start result = %#v", result)
	}
	var admission domain.ExecutionAdmission
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(p.ctx, p.invocation)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: p.invocation, AdmissionID: admission.ID,
		ObservedBaseSHA: p.baseSHA, HeadSHA: headSHA,
		ManifestDigest: p.replay.ManifestDigest, RecordedAt: fakePublicationTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (p *productionPublicationHarness) assertReady(t *testing.T) {
	p.assertReadyWithEvidence(t, 2)
}

func (p *productionPublicationHarness) assertReadyWithEvidence(t *testing.T, wantEvidence int) {
	t.Helper()
	var ready domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		ready, err = tx.GetAttentionItem(p.ctx, domain.ItemID("production-ready-"+string(p.runID)))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ready.Type != domain.AttentionReadyForFinalReview || ready.PRHeadSHA != p.replay.HeadSHA ||
		len(ready.EvidenceSnapshot) != wantEvidence {
		t.Fatalf("ready item = %#v", ready)
	}
	refs, prs := p.forge.counts()
	if refs != 1 || prs != 1 {
		t.Fatalf("forge effects = %d refs, %d PRs, want one each", refs, prs)
	}
	pullRequests := p.forge.pullRequests()
	metadata := productionPublicationMetadata()
	if len(pullRequests) != 1 || pullRequests[0].Title != metadata.Title ||
		!strings.HasPrefix(pullRequests[0].Body, strings.TrimRight(metadata.Body, "\n")+"\n\n") {
		t.Fatalf("published PR metadata = %#v, want operator-authored title/body", pullRequests)
	}
	var terminal store.QueueEntry
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		terminal, err = tx.GetInbox(p.ctx, string(p.invocation))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if terminal.Kind != productionTerminalKind {
		t.Fatalf("terminal kind = %q", terminal.Kind)
	}
}

func TestProductionExecutionPublishesOnlyAfterCleanVerification(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	result, err := p.workflow.Reconcile(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.ResultsAccepted != 1 || result.ReadyItemsCreated != 1 ||
		result.PublicationTasksCompleted != 1 || result.LastPRNumber == 0 {
		t.Fatalf("publication result = %#v", result)
	}
	p.assertReady(t)
	if p.room.runs != 1 {
		t.Fatalf("verification commands = %d, want 1", p.room.runs)
	}
	if p.room.reads != 1 {
		t.Fatalf("project-image recipe reads = %d, want 1", p.room.reads)
	}
	beforePushes := p.transport.pushCount()
	if result, err := p.workflow.Reconcile(p.ctx); err != nil ||
		result.ResultsAccepted != 0 || result.PublicationTasksCompleted != 0 {
		t.Fatalf("converged replay = %#v, %v", result, err)
	}
	if p.transport.pushCount() != beforePushes {
		t.Fatal("converged replay repeated the publication transport")
	}
}

func TestProductionVerificationRejectsRecipeNotBoundToProjectImage(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	mismatched := []byte(`{"commands":[["different-check"]],"capture":"none"}`)
	p.room.recipe = mismatched
	p.workflow = p.newEngineWithApprovedRecipes(
		t, productionCrashSeams{}, true,
		map[domain.Digest]bool{
			p.recipeD:                       true,
			verify.RecipeDigest(mismatched): true,
		},
	)
	p.startAndRecordExport(t)
	if _, err := p.workflow.Reconcile(p.ctx); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("mismatched project-image recipe error = %v", err)
	}
	if p.room.reads != 1 || p.room.runs != 0 {
		t.Fatalf("mismatched recipe reads/runs = %d/%d, want 1/0", p.room.reads, p.room.runs)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("mismatched recipe caused effects: %d refs, %d PRs", refs, prs)
	}
}

func TestProductionRecipeExtractionIsBounded(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.recipeReadTimeout = 25 * time.Millisecond
	p.room.read = func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("recipe extraction timeout = %#v, %v", result, err)
	}
	if p.room.reads != 1 || p.room.runs != 0 {
		t.Fatalf("timed-out recipe reads/runs = %d/%d, want 1/0", p.room.reads, p.room.runs)
	}
	if replay, err := p.workflow.Reconcile(p.ctx); err != nil || replay != (engine.ReconcileResult{}) {
		t.Fatalf("immediate timed-out recipe replay = %#v, %v", replay, err)
	}
	if p.room.reads != 1 {
		t.Fatalf("timed-out recipe was read %d times before backoff elapsed, want 1", p.room.reads)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("timed-out recipe extraction caused effects: %d refs, %d PRs", refs, prs)
	}
	p.room.read = nil
	p.now = p.now.Add(time.Minute)
	if result, err := p.workflow.Reconcile(p.ctx); err != nil ||
		result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("recipe extraction timeout recovery = %#v, %v", result, err)
	}
	p.assertReady(t)
}

func TestProductionPublicationRestartsAcrossDurableBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		seams func(error) productionCrashSeams
	}{
		{"verification", func(err error) productionCrashSeams {
			return productionCrashSeams{afterVerification: func() error { return err }}
		}},
		{"publication", func(err error) productionCrashSeams {
			return productionCrashSeams{afterPublication: func() error { return err }}
		}},
		{"ready", func(err error) productionCrashSeams {
			return productionCrashSeams{afterReady: func() error { return err }}
		}},
		{"terminal", func(err error) productionCrashSeams {
			return productionCrashSeams{afterTerminal: func() error { return err }}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.workflow = p.newEngine(t, tc.seams(errors.New("injected crash")), true)
			p.startAndRecordExport(t)
			if _, err := p.workflow.Reconcile(p.ctx); err == nil {
				t.Fatal("crash seam did not interrupt reconciliation")
			}
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.workflow.Reconcile(p.ctx); err != nil {
				t.Fatalf("restart reconciliation: %v", err)
			}
			p.assertReady(t)
			refs, prs := p.forge.counts()
			if refs != 1 || prs != 1 {
				t.Fatalf("restart duplicated effects: %d refs, %d PRs", refs, prs)
			}
			if p.room.reads != 1 || p.room.runs != 1 {
				t.Fatalf("restart repeated recipe extraction/verification: %d/%d", p.room.reads, p.room.runs)
			}
		})
	}
}

func TestAttendedRestartHoldsQueuedUnattendedPublication(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.workflow = p.newEngineForMode(
		t, productionCrashSeams{}, true, nil, domain.ModeAttendedDev, true,
	)
	before, err := p.store.ServerState(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("attended publication hold = %#v, %v", result, err)
	}
	after, err := p.store.ServerState(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("attended publication hold moved server state from %#v to %#v", before, after)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("attended publication hold caused effects: %d refs, %d PRs", refs, prs)
	}
	if p.room.reads != 0 || p.room.runs != 0 {
		t.Fatalf("attended publication hold verified work: %d reads, %d runs", p.room.reads, p.room.runs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
		if err != nil {
			return err
		}
		if task.Dispatched() {
			return fmt.Errorf("attended publication task status = %q, want pending", task.Status)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.workflow.Reconcile(p.ctx); err != nil {
		t.Fatalf("resume held publication after unattended restart: %v", err)
	}
	p.assertReady(t)
	if refs, prs := p.forge.counts(); refs != 1 || prs != 1 {
		t.Fatalf("resumed publication effects = %d refs, %d PRs, want 1/1", refs, prs)
	}
}

func TestProductionPublicationRecoversWithoutPrivateDriverReplay(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	var executionExport domain.ExecutionExport
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		executionExport, err = tx.GetExecutionExportRecord(p.ctx, p.invocation)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.RecordProductionExecutionExport(
		p.ctx, p.store, executionExport, p.replay,
	); err != nil {
		t.Fatalf("identical atomic completion did not converge: %v", err)
	}
	var task store.QueueEntry
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		task, err = tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if task.Kind != engine.KindProductionPublicationRequested {
		t.Fatalf("atomic completion task kind = %q", task.Kind)
	}
	checkpoint := filepath.Join(t.TempDir(), "freeside-checkpoint.db")
	if err := p.store.Checkpoint(p.ctx, checkpoint); err != nil {
		t.Fatalf("checkpoint atomic completion: %v", err)
	}
	if _, err := p.store.Restore(p.ctx, checkpoint); err != nil {
		t.Fatalf("restore atomic completion: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(p.workDir, "production-driver")); err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewStageDriverAt(filepath.Join(p.workDir, "production-driver"))
	if err != nil {
		t.Fatal(err)
	}
	p.driver = driver
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.workflow.Reconcile(p.ctx); err != nil {
		t.Fatalf("SQLite-and-artifact recovery required private replay: %v", err)
	}
	p.assertReady(t)
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("converged replay = %#v, %v", result, err)
	}
}

func TestUnattendedExecutionExportUsesAtomicPathAfterAttendedRestart(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	result, err := p.workflow.Reconcile(p.ctx)
	if err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("start result = %#v, %v", result, err)
	}
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			p.ctx, "production-publication/"+string(p.runID), "conflicting-task", []byte("different"),
		)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var admission domain.ExecutionAdmission
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(p.ctx, p.invocation)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: p.invocation, AdmissionID: admission.ID,
		ObservedBaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
		ManifestDigest: p.replay.ManifestDigest, RecordedAt: fakePublicationTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := store.Open(p.ctx, p.dbPath, store.Options{
		ApprovedRecipes: map[domain.Digest]bool{p.recipeD: true},
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: {},
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	p.store = restarted
	if err := engine.RecordExecutionExport(p.ctx, p.store, record, p.replay); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("attended-restart completion error = %v, want atomic task conflict", err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetExecutionExportRecord(p.ctx, p.invocation)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rolled-back execution export lookup = %v, want not found", err)
	}
}

func TestProductionPublicationRestartsAcrossExternalEffectBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*productionPublicationHarness)
	}{
		{"transport push", func(p *productionPublicationHarness) { p.transport.failNextPush() }},
		{"pull request create", func(p *productionPublicationHarness) { p.forge.failAfterNextPRCreate() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.startAndRecordExport(t)
			tc.inject(p)
			if result, err := p.workflow.Reconcile(p.ctx); err != nil || result != (engine.ReconcileResult{}) {
				t.Fatalf("contained external-effect failure = %#v, %v", result, err)
			}
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.workflow.Reconcile(p.ctx); err != nil {
				t.Fatalf("restart reconciliation: %v", err)
			}
			p.assertReady(t)
			if refs, prs := p.forge.counts(); refs != 1 || prs != 1 {
				t.Fatalf("restart duplicated effects: %d refs, %d PRs", refs, prs)
			}
		})
	}
}

func TestFinalizedProductionPublicationSurvivesLaterTrustDrift(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterPublication: func() error { return errors.New("stop after durable publication outcome") },
	}, true)
	p.startAndRecordExport(t)
	if _, err := p.workflow.Reconcile(p.ctx); err == nil {
		t.Fatal("publication outcome seam did not interrupt reconciliation")
	}
	reviseWaivedTrustProfile(t, p.store)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.workflow.Reconcile(p.ctx); err != nil {
		t.Fatalf("finalize durable publication after trust drift: %v", err)
	}
	p.assertReady(t)
}

func TestReadyProductionPublicationWinsOverLaterExternalConflict(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterReady: func() error { return errors.New("stop after durable ready item") },
	}, true)
	p.startAndRecordExport(t)
	if _, err := p.workflow.Reconcile(p.ctx); err == nil {
		t.Fatal("ready-item seam did not interrupt reconciliation")
	}
	p.forge.clearRefs()
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	result, err := p.workflow.Reconcile(p.ctx)
	if err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("recover ready publication after external conflict = %#v, %v", result, err)
	}
	if _, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ready recovery created contradictory blocked item: %v", err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 1 {
		t.Fatalf("ready recovery changed external effects: %d refs/%d PRs", refs, prs)
	}
	if replay, err := p.workflow.Reconcile(p.ctx); err != nil || replay != (engine.ReconcileResult{}) {
		t.Fatalf("ready-conflict replay = %#v, %v", replay, err)
	}
}

func TestReadyProductionPublicationMissingPrerequisiteFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		arg   any
	}{
		{"outcome", "DELETE FROM inbox WHERE kind = ?", publish.IntentKindOutcome},
		{"checkpoint", "DELETE FROM inbox WHERE idempotency_key = ?", "production-verification/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.workflow = p.newEngine(t, productionCrashSeams{
				afterReady: func() error { return errors.New("stop after durable ready item") },
			}, true)
			p.startAndRecordExport(t)
			if _, err := p.workflow.Reconcile(p.ctx); err == nil {
				t.Fatal("ready-item seam did not interrupt reconciliation")
			}
			arg := tc.arg
			if tc.name == "checkpoint" {
				arg = tc.arg.(string) + string(p.runID)
			}
			raw, err := sql.Open("sqlite", p.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			result, err := raw.ExecContext(p.ctx, tc.query, arg)
			closeErr := raw.Close()
			if err != nil || closeErr != nil {
				t.Fatal(errors.Join(err, closeErr))
			}
			if removed, err := result.RowsAffected(); err != nil || removed != 1 {
				t.Fatalf("removed prerequisite rows = %d, %v, want 1", removed, err)
			}
			p.forge.clearRefs()
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.workflow.Reconcile(p.ctx); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("orphaned ready recovery error = %v, want parent-key mismatch", err)
			}
			if _, err := p.attention.GetAttentionItem(
				p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
			); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("orphaned ready recovery created blocked item: %v", err)
			}
			if refs, prs := p.forge.counts(); refs != 0 || prs != 1 {
				t.Fatalf("orphaned ready recovery changed external effects: %d refs/%d PRs", refs, prs)
			}
		})
	}
}

func TestFinalizedProductionPublicationSurvivesLaterRecipeRevocation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seams productionCrashSeams
	}{
		{"after outcome", productionCrashSeams{
			afterPublication: func() error { return errors.New("stop after durable publication outcome") },
		}},
		{"after ready", productionCrashSeams{
			afterReady: func() error { return errors.New("stop after durable ready item") },
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.workflow = p.newEngine(t, tc.seams, true)
			p.startAndRecordExport(t)
			if _, err := p.workflow.Reconcile(p.ctx); err == nil {
				t.Fatal("publication seam did not interrupt reconciliation")
			}
			p.workflow = p.newEngineWithApprovedRecipes(
				t, productionCrashSeams{}, true,
				map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
			)
			if result, err := p.workflow.Reconcile(p.ctx); err != nil ||
				result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
				t.Fatalf("finalize durable publication after recipe revocation = %#v, %v", result, err)
			}
			if tc.name == "after outcome" {
				p.assertReadyWithEvidence(t, 0)
			} else {
				p.assertReady(t)
			}
			if _, err := p.attention.GetAttentionItem(
				p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
			); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("finalized publication created contradictory blocked item: %v", err)
			}
		})
	}
	t.Run("after revoked ready", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.workflow = p.newEngine(t, productionCrashSeams{
			afterPublication: func() error { return errors.New("stop after durable publication outcome") },
		}, true)
		p.startAndRecordExport(t)
		if _, err := p.workflow.Reconcile(p.ctx); err == nil {
			t.Fatal("publication outcome seam did not interrupt reconciliation")
		}
		revokedRecipes := map[domain.Digest]bool{
			productionDigest([]byte("unrelated recipe")): true,
		}
		p.workflow = p.newEngineForMode(
			t,
			productionCrashSeams{afterReady: func() error { return errors.New("stop after redacted ready item") }},
			true, revokedRecipes, domain.ModeUnattended, false,
		)
		if _, err := p.workflow.Reconcile(p.ctx); err == nil {
			t.Fatal("redacted ready seam did not interrupt reconciliation")
		}
		p.workflow = p.newEngineWithApprovedRecipes(
			t, productionCrashSeams{}, true, revokedRecipes,
		)
		if result, err := p.workflow.Reconcile(p.ctx); err != nil ||
			result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
			t.Fatalf("recover redacted ready after recipe revocation = %#v, %v", result, err)
		}
		p.assertReadyWithEvidence(t, 0)
	})
}

func TestProductionPublicationConflictIsDurablyHeld(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.conflictNextPush()
	result, err := p.workflow.Reconcile(p.ctx)
	if err != nil || result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 1 {
		t.Fatalf("conflicted publication hold = %#v, %v", result, err)
	}
	hold, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hold.Item.Reason, "external branch or pull request conflicts") ||
		!slices.Equal(hold.Item.RequestedDecision, []domain.Action{domain.ActionInspectTrustFailure}) {
		t.Fatalf("publication conflict hold = %#v", hold.Item)
	}
	fetchesBeforeReplay := p.transport.fetchCount()
	if replay, err := p.workflow.Reconcile(p.ctx); err != nil ||
		replay.PublicationTasksCompleted != 0 || replay.BlockedItemsCreated != 0 {
		t.Fatalf("idempotent publication conflict hold = %#v, %v", replay, err)
	}
	if fetches := p.transport.fetchCount(); fetches != fetchesBeforeReplay {
		t.Fatalf("held publication immediate replay fetched %d bases, want unchanged %d",
			fetches, fetchesBeforeReplay)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 0 {
		t.Fatalf("conflicted publication effects = %d refs/%d PRs", refs, prs)
	}
	p.forge.clearRefs()
	p.now = p.now.Add(time.Minute)
	if _, err := p.workflow.Reconcile(p.ctx); err != nil {
		t.Fatalf("recover publication after conflict repair: %v", err)
	}
	p.assertReady(t)
	hold, err = p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if hold.Item.Status != domain.StatusSuperseded {
		t.Fatalf("recovered conflict hold status = %q, want superseded", hold.Item.Status)
	}
}

func TestProductionPublicationTransientFailureBacksOffWithoutStoppingEngine(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.failFetch(&net.DNSError{Err: "temporary", Name: "github.com"})

	result, err := p.workflow.Reconcile(p.ctx)
	if err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("transient publication reconcile = %#v, %v", result, err)
	}
	if fetches := p.transport.fetchCount(); fetches != 1 {
		t.Fatalf("transient publication fetches = %d, want 1", fetches)
	}
	if replay, err := p.workflow.Reconcile(p.ctx); err != nil || replay != (engine.ReconcileResult{}) {
		t.Fatalf("immediate transient replay = %#v, %v", replay, err)
	}
	if fetches := p.transport.fetchCount(); fetches != 1 {
		t.Fatalf("transient backoff fetched %d bases, want 1", fetches)
	}

	p.transport.failFetch(nil)
	p.now = p.now.Add(time.Minute)
	if result, err := p.workflow.Reconcile(p.ctx); err != nil ||
		result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("transient publication recovery = %#v, %v", result, err)
	}
	p.assertReady(t)
}

func TestProductionPublicationMutableAppAuthorityBacksOff(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.tokens = inactiveIntegrationTokenSource{}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)

	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("inactive App authority reconcile = %#v, %v", result, err)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 0 {
		t.Fatalf("inactive App authority effects = %d refs/%d PRs, want converged push only", refs, prs)
	}

	p.tokens = integrationTokenSource{}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if result, err := p.workflow.Reconcile(p.ctx); err != nil ||
		result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("restored App authority reconcile = %#v, %v", result, err)
	}
	p.assertReady(t)
}

func TestProductionPublicationPermanentFetchRefusalIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "missing base", err: publish.ErrRemoteMissingBase},
		{name: "trust profile drift", err: publish.ErrTrustProfileDrift},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.startAndRecordExport(t)
			p.transport.failFetch(fmt.Errorf("fetch refused: %w", tc.err))

			if result, err := p.workflow.Reconcile(p.ctx); err != nil || result.BlockedItemsCreated != 1 {
				t.Fatalf("permanent fetch refusal reconcile = %#v, %v", result, err)
			}
			p.transport.failFetch(nil)
			p.now = p.now.Add(time.Minute)
			if result, err := p.workflow.Reconcile(p.ctx); err != nil ||
				result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
				t.Fatalf("repaired permanent fetch refusal = %#v, %v", result, err)
			}
			p.assertReady(t)
		})
	}
}

func TestProductionPublicationHoldAdvancesToDefinitiveBlock(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.failFetch(fmt.Errorf("base disappeared: %w", publish.ErrRemoteMissingBase))
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("initial repairable hold = %#v, %v", result, err)
	}
	itemID := domain.ItemID("production-publish-blocked-" + string(p.runID))
	hold, err := p.attention.GetAttentionItem(p.ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}

	p.transport.failFetch(nil)
	p.room.fail = true
	p.now = p.now.Add(time.Minute)
	if result, err := p.workflow.Reconcile(p.ctx); err != nil ||
		result.PublicationTasksCompleted != 1 || result.BlockedItemsCreated != 1 {
		t.Fatalf("definitive block after repaired hold = %#v, %v", result, err)
	}
	blocked, err := p.attention.GetAttentionItem(p.ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Item.ItemVersion != hold.Item.ItemVersion+1 ||
		blocked.Item.Reason != "Verification or current policy findings blocked production publication." ||
		!slices.Equal(blocked.Item.RequestedDecision, []domain.Action{
			domain.ActionInspectTrustFailure, domain.ActionStop,
		}) || blocked.Item.Status != domain.StatusOpen {
		t.Fatalf("definitive successor = %#v", blocked.Item)
	}
	if blocked.Item.Timing != hold.Item.Timing ||
		!reflect.DeepEqual(blocked.Item.ConversationID, hold.Item.ConversationID) ||
		!reflect.DeepEqual(blocked.Item.ExpiresWhen, hold.Item.ExpiresWhen) {
		t.Fatalf("definitive successor lost hold metadata: before=%#v after=%#v",
			hold.Item, blocked.Item)
	}
}

func TestProductionPublicationReleaseFailurePreservesOutcome(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterLockRelease: func() error { return errors.New("injected lock release failure") },
	}, true)
	p.startAndRecordExport(t)

	result, err := p.workflow.Reconcile(p.ctx)
	if err != nil || result.PublicationTasksCompleted != 1 ||
		result.ReadyItemsCreated != 1 || result.LastPRNumber <= 0 {
		t.Fatalf("release failure result = %#v, %v", result, err)
	}
	p.assertReady(t)
}

func TestProductionPublicationCorruptCheckpointStillFailsLoud(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		_, inserted, err := tx.RecordInbox(
			p.ctx,
			"production-verification/"+string(p.runID),
			"production_verification_checkpoint",
			[]byte(`{"version":`),
		)
		if err == nil && !inserted {
			return errors.New("corrupt checkpoint unexpectedly existed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := p.workflow.Reconcile(p.ctx); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("corrupt production checkpoint error = %v, want parent-key mismatch", err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("corrupt production checkpoint caused effects: %d refs/%d PRs", refs, prs)
	}
}

func TestProductionPublicationReadyPrecedesHoldSupersession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action domain.Action
	}{
		{name: "open"},
		{name: "dismissed", action: domain.ActionDismiss},
		{name: "stopped", action: domain.ActionStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.startAndRecordExport(t)
			p.transport.conflictNextPush()
			if result, err := p.workflow.Reconcile(p.ctx); err != nil || result.BlockedItemsCreated != 1 {
				t.Fatalf("conflicted publication hold = %#v, %v", result, err)
			}
			p.forge.clearRefs()
			p.workflow = p.newEngine(t, productionCrashSeams{
				afterReady: func() error { return errors.New("stop after durable ready item") },
			}, true)
			if _, err := p.workflow.Reconcile(p.ctx); err == nil {
				t.Fatal("ready-item seam did not interrupt held-publication recovery")
			}
			hold, err := p.attention.GetAttentionItem(
				p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
			)
			if err != nil {
				t.Fatal(err)
			}
			if hold.Item.Status != domain.StatusOpen {
				t.Fatalf("hold status before ready recovery = %q, want open", hold.Item.Status)
			}
			ready, err := p.attention.GetAttentionItem(
				p.ctx, domain.ItemID("production-ready-"+string(p.runID)),
			)
			if err != nil {
				t.Fatal(err)
			}
			if tc.action != "" {
				deviceID := domain.DeviceID("device-ready-" + tc.name)
				if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
					return tx.PutDevice(p.ctx, domain.Device{
						ID: deviceID, DisplayName: "Ready recovery device",
						Status: domain.DeviceActive, PairedAt: fakePublicationTime,
					})
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
					CommandID: "ready-" + tc.name, DeviceID: deviceID,
					ExpectedEntityVersion: ready.EntityVersion,
					Payload: signet.DecisionPayload{
						ItemID: ready.Item.ID, Action: tc.action,
						ItemVersion: ready.Item.ItemVersion, PRHeadSHA: ready.Item.PRHeadSHA,
						ArtifactDigests: ready.Item.ArtifactDigests,
					},
				}); err != nil {
					t.Fatal(err)
				}
				ready, err = p.attention.GetAttentionItem(p.ctx, ready.Item.ID)
				if err != nil {
					t.Fatal(err)
				}
				if ready.Item.Status == domain.StatusOpen || ready.Item.DecidedAt == nil {
					t.Fatalf("ready successor = %#v", ready.Item)
				}
			}
			successor := ready.Item
			p.forge.clearRefs()
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			result, err := p.workflow.Reconcile(p.ctx)
			if err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
				t.Fatalf("recover ready publication after later conflict = %#v, %v", result, err)
			}
			hold, err = p.attention.GetAttentionItem(p.ctx, hold.Item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if hold.Item.Status != domain.StatusSuperseded {
				t.Fatalf("recovered hold status = %q, want superseded", hold.Item.Status)
			}
			recovered, err := p.attention.GetAttentionItem(p.ctx, ready.Item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(recovered.Item, successor) {
				t.Fatalf("ready successor changed on recovery:\n got: %#v\nwant: %#v",
					recovered.Item, successor)
			}
			if refs, prs := p.forge.counts(); refs != 0 || prs != 1 {
				t.Fatalf("ready recovery changed external effects: %d refs/%d PRs", refs, prs)
			}
		})
	}
}

func TestRecipeRevocationRetainsPendingPublicationIntent(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.failNextPush()
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("contained transport failure = %#v, %v", result, err)
	}
	p.workflow = p.newEngineWithApprovedRecipes(
		t, productionCrashSeams{}, true,
		map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
	)
	if result, err := p.workflow.Reconcile(p.ctx); err != nil ||
		result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 1 {
		t.Fatalf("revoked recipe with pending intent = %#v, %v", result, err)
	}
	hold, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hold.Item.Reason, "durably held") ||
		len(hold.Item.EvidenceSnapshot) != 0 ||
		!slices.Equal(hold.Item.RequestedDecision, []domain.Action{domain.ActionInspectTrustFailure}) {
		t.Fatalf("pending intent hold = %#v", hold.Item)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
		if err != nil {
			return err
		}
		if task.Dispatched() {
			t.Errorf("pending-intent task status = %q, want pending", task.Status)
		}
		_, err = tx.GetInbox(p.ctx, string(p.invocation))
		if err == nil {
			return errors.New("pending-intent terminal unexpectedly exists")
		}
		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("read pending-intent terminal: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := p.workflow.Reconcile(p.ctx); err != nil ||
		result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("idempotent revoked-recipe hold = %#v, %v", result, err)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.workflow.Reconcile(p.ctx); err != nil {
		t.Fatalf("recover retained publication after recipe approval: %v", err)
	}
	p.assertReady(t)
	hold, err = p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if hold.Item.Status != domain.StatusSuperseded {
		t.Fatalf("recovered publication hold status = %q, want superseded", hold.Item.Status)
	}
}

func TestProductionPublicationHoldRefreshesWhenCauseChanges(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.conflictNextPush()
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("external-conflict hold = %#v, %v", result, err)
	}
	itemID := domain.ItemID("production-publish-blocked-" + string(p.runID))
	conflict, err := p.attention.GetAttentionItem(p.ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	p.forge.clearRefs()
	p.workflow = p.newEngineWithApprovedRecipes(
		t, productionCrashSeams{}, true,
		map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
	)
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("recipe-revocation hold refresh = %#v, %v", result, err)
	}
	revoked, err := p.attention.GetAttentionItem(p.ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Item.ItemVersion != conflict.Item.ItemVersion+1 ||
		!strings.Contains(revoked.Item.Reason, "current trust no longer approves") ||
		strings.Contains(revoked.Item.Reason, "external branch or pull request conflicts") {
		t.Fatalf("refreshed publication hold = %#v", revoked.Item)
	}
	beforeReplay, err := p.store.ServerState(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("idempotent refreshed hold = %#v, %v", result, err)
	}
	afterReplay, err := p.store.ServerState(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterReplay != beforeReplay {
		t.Fatalf("refreshed hold replay moved server state from %#v to %#v", beforeReplay, afterReplay)
	}
}

func TestProductionExportSurvivesUntilPublicationComposerReturns(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{}, false)
	p.startAndRecordExport(t)
	if _, err := p.workflow.Reconcile(p.ctx); err == nil ||
		!strings.Contains(err.Error(), "publication workflow is not configured") {
		t.Fatalf("pre-publication completion error = %v", err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("uncomposed publication caused effects: %d/%d", refs, prs)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.workflow.Reconcile(p.ctx); err != nil {
		t.Fatal(err)
	}
	p.assertReady(t)
}

func TestProductionVerificationAndHeadMismatchNeverReachExternalEffects(t *testing.T) {
	t.Run("blocked reason must match checkpoint authority", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.room.fail = true
		p.workflow = p.newEngine(t, productionCrashSeams{
			afterBlocked: func() error { return errors.New("stop after blocked item") },
		}, true)
		p.startAndRecordExport(t)
		if _, err := p.workflow.Reconcile(p.ctx); err == nil {
			t.Fatal("blocked-item seam did not interrupt reconciliation")
		}
		blocked, err := p.attention.GetAttentionItem(
			p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
		)
		if err != nil {
			t.Fatal(err)
		}
		blocked.Item.Reason = "Current trust state definitively blocked publication."
		forged, err := json.Marshal(blocked.Item)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := sql.Open("sqlite", p.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		result, err := raw.ExecContext(
			p.ctx, "UPDATE attention_items SET body = ? WHERE id = ?", string(forged), blocked.Item.ID,
		)
		closeErr := raw.Close()
		if err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			t.Fatalf("forged blocked rows = %d, %v", changed, err)
		}
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		if _, err := p.workflow.Reconcile(p.ctx); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("impossible blocked reason = %v, want parent-key mismatch", err)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("impossible blocked reason caused forge effects: %d/%d", refs, prs)
		}
	})

	t.Run("open blocked item survives recipe revocation", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.room.fail = true
		p.workflow = p.newEngine(t, productionCrashSeams{
			afterBlocked: func() error { return errors.New("stop after blocked item") },
		}, true)
		p.startAndRecordExport(t)
		if _, err := p.workflow.Reconcile(p.ctx); err == nil {
			t.Fatal("blocked-item seam did not interrupt reconciliation")
		}
		blocked, err := p.attention.GetAttentionItem(
			p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(blocked.Item.EvidenceSnapshot) == 0 {
			t.Fatal("verification block carried no evidence")
		}
		p.workflow = p.newEngineWithApprovedRecipes(
			t, productionCrashSeams{}, true,
			map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
		)
		result, err := p.workflow.Reconcile(p.ctx)
		if err != nil || result.ResultsAccepted != 1 || result.BlockedItemsCreated != 1 {
			t.Fatalf("recover blocked item after recipe revocation = %#v, %v", result, err)
		}
		current, err := p.attention.GetAttentionItem(p.ctx, blocked.Item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(current.Item, blocked.Item) {
			t.Fatalf("blocked item changed after recipe revocation: %#v", current.Item)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("blocked recovery caused forge effects: %d/%d", refs, prs)
		}
	})

	t.Run("resolved blocked item survives recipe revocation", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.room.fail = true
		p.workflow = p.newEngine(t, productionCrashSeams{
			afterBlocked: func() error { return errors.New("stop after blocked item") },
		}, true)
		p.startAndRecordExport(t)
		if _, err := p.workflow.Reconcile(p.ctx); err == nil {
			t.Fatal("blocked-item seam did not interrupt reconciliation")
		}
		blocked, err := p.attention.GetAttentionItem(
			p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
		)
		if err != nil {
			t.Fatal(err)
		}
		deviceID := domain.DeviceID("device-blocked-recovery")
		if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
			return tx.PutDevice(p.ctx, domain.Device{
				ID: deviceID, DisplayName: "Blocked recovery device",
				Status: domain.DeviceActive, PairedAt: fakePublicationTime,
			})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
			CommandID: "stop-blocked-recovery", DeviceID: deviceID,
			ExpectedEntityVersion: blocked.EntityVersion,
			Payload: signet.DecisionPayload{
				ItemID: blocked.Item.ID, Action: domain.ActionStop,
				ItemVersion: blocked.Item.ItemVersion, PRHeadSHA: blocked.Item.PRHeadSHA,
				ArtifactDigests: blocked.Item.ArtifactDigests,
			},
		}); err != nil {
			t.Fatal(err)
		}
		resolvedSnapshot, err := p.attention.GetAttentionItem(p.ctx, blocked.Item.ID)
		if err != nil {
			t.Fatal(err)
		}
		resolved := resolvedSnapshot.Item

		p.workflow = p.newEngineWithApprovedRecipes(
			t, productionCrashSeams{}, true,
			map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
		)
		result, err := p.workflow.Reconcile(p.ctx)
		if err != nil {
			t.Fatalf("recover resolved blocked item: %v", err)
		}
		if result.ResultsAccepted != 1 || result.BlockedItemsCreated != 1 ||
			result.ReadyItemsCreated != 0 {
			t.Fatalf("resolved-block recovery result = %#v", result)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			task, err := tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
			if err != nil {
				return err
			}
			if !task.Dispatched() {
				t.Errorf("recovered blocked task status = %q, want dispatched", task.Status)
			}
			terminal, err := tx.GetInbox(p.ctx, string(p.invocation))
			if err != nil {
				return err
			}
			if terminal.Kind != productionTerminalKind {
				t.Errorf("recovered blocked terminal kind = %q", terminal.Kind)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("resolved-block recovery caused forge effects: %d/%d", refs, prs)
		}
		current, err := p.attention.GetAttentionItem(p.ctx, resolved.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(current.Item, resolved) {
			t.Fatalf("resolved blocked item changed on recovery: %#v", current.Item)
		}
		if replay, err := p.workflow.Reconcile(p.ctx); err != nil || replay != (engine.ReconcileResult{}) {
			t.Fatalf("resolved-block replay = %#v, %v", replay, err)
		}
	})

	t.Run("verification failure", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.room.fail = true
		p.startAndRecordExport(t)
		result, err := p.workflow.Reconcile(p.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result.ResultsAccepted != 1 || result.BlockedItemsCreated != 1 ||
			result.ReadyItemsCreated != 0 {
			t.Fatalf("blocked result = %#v", result)
		}
		blocked, err := p.attention.GetAttentionItem(
			p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
		)
		if err != nil {
			t.Fatal(err)
		}
		wantActions := []domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop}
		if !slices.Equal(blocked.Item.RequestedDecision, wantActions) {
			t.Fatalf("blocked actions = %v, want %v", blocked.Item.RequestedDecision, wantActions)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			task, err := tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
			if err != nil {
				return err
			}
			if !task.Dispatched() {
				t.Errorf("blocked publication task status = %q, want dispatched", task.Status)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var terminal store.QueueEntry
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			terminal, err = tx.GetInbox(p.ctx, string(p.invocation))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if terminal.Kind != productionTerminalKind {
			t.Fatalf("blocked terminal kind = %q", terminal.Kind)
		}
		beforeReplay, err := p.store.ServerState(p.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if replay, err := p.workflow.Reconcile(p.ctx); err != nil || replay != (engine.ReconcileResult{}) {
			t.Fatalf("blocked replay = %#v, %v", replay, err)
		}
		afterReplay, err := p.store.ServerState(p.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if afterReplay != beforeReplay {
			t.Fatalf("blocked replay moved server state from %#v to %#v", beforeReplay, afterReplay)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("unverified candidate caused effects: %d/%d", refs, prs)
		}
	})
	for _, tc := range []struct {
		name            string
		checkpointFirst bool
	}{{"revoked project-image recipe", false}, {"recipe revoked after checkpoint", true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.startAndRecordExport(t)
			if tc.checkpointFirst {
				p.workflow = p.newEngine(t, productionCrashSeams{
					afterVerification: func() error { return errors.New("stop after checkpoint") },
				}, true)
				if _, err := p.workflow.Reconcile(p.ctx); err == nil {
					t.Fatal("verification seam did not stop after checkpoint")
				}
			}
			verificationRuns := p.room.runs
			p.workflow = p.newEngineWithApprovedRecipes(
				t, productionCrashSeams{}, true,
				map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
			)
			result, err := p.workflow.Reconcile(p.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if result.ResultsAccepted != 1 || result.BlockedItemsCreated != 1 ||
				result.ReadyItemsCreated != 0 {
				t.Fatalf("revoked-recipe result = %#v", result)
			}
			blocked, err := p.attention.GetAttentionItem(
				p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(blocked.Item.Reason, "no longer approves") {
				t.Fatalf("revoked-recipe reason = %q", blocked.Item.Reason)
			}
			if len(blocked.Item.EvidenceSnapshot) != 0 {
				t.Fatalf("revoked-recipe evidence = %#v, want none", blocked.Item.EvidenceSnapshot)
			}
			if p.room.runs != verificationRuns {
				t.Fatalf("revoked recipe ran %d additional verification commands", p.room.runs-verificationRuns)
			}
			if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
				t.Fatalf("revoked recipe caused effects: %d/%d", refs, prs)
			}
			if replay, err := p.workflow.Reconcile(p.ctx); err != nil || replay != (engine.ReconcileResult{}) {
				t.Fatalf("revoked-recipe replay = %#v, %v", replay, err)
			}
		})
	}
	t.Run("target base advanced before publication", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.audit.AuditedCommitSHA = strings.Repeat("9", 40)
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		p.startAndRecordExport(t)
		result, err := p.workflow.Reconcile(p.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result.ResultsAccepted != 1 || result.BlockedItemsCreated != 1 ||
			result.ReadyItemsCreated != 0 {
			t.Fatalf("advanced-base result = %#v", result)
		}
		blocked, err := p.attention.GetAttentionItem(
			p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
		)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(blocked.Item.Reason, "target base advanced") {
			t.Fatalf("advanced-base reason = %q", blocked.Item.Reason)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("advanced base caused effects: %d/%d", refs, prs)
		}
		var pending []store.QueueEntry
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			pending, err = tx.ListPendingOutbox(p.ctx, publish.IntentKindPublication)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatalf("advanced base left publication intents: %#v", pending)
		}
	})
	t.Run("head mismatch", func(t *testing.T) {
		p := newProductionPublicationHarness(t, strings.Repeat("f", 40))
		record := p.startExecutionExport(t, strings.Repeat("f", 40))
		if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, p.replay); err == nil ||
			!strings.Contains(err.Error(), "replay disagrees") {
			t.Fatalf("head mismatch error = %v", err)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("mismatched head caused effects: %d/%d", refs, prs)
		}
	})
	t.Run("replay policy mismatch", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.replay.ImportOptions.Policy.Allowlist = nil
		record := p.startExecutionExport(t, p.replay.HeadSHA)
		if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, p.replay); err == nil ||
			!strings.Contains(err.Error(), "import options disagree") {
			t.Fatalf("replay policy mismatch error = %v", err)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetExecutionExportRecord(p.ctx, p.invocation)
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("policy-mismatched export lookup = %v, want not found", err)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("policy-mismatched task lookup = %v, want not found", err)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("policy-mismatched replay caused effects: %d/%d", refs, prs)
		}
	})
	t.Run("replay commit date mismatch", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.replay.ImportOptions.CommitDate = p.replay.ImportOptions.CommitDate.Add(time.Minute)
		record := p.startExecutionExport(t, p.replay.HeadSHA)
		if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, p.replay); err == nil ||
			!strings.Contains(err.Error(), "replay disagrees") {
			t.Fatalf("replay commit-date mismatch error = %v", err)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("commit-date-mismatched replay caused effects: %d/%d", refs, prs)
		}
	})
}

func TestProductionPublicationBackupBindsReplayBlobs(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	// Interrupt after the task and verification checkpoint exist so the
	// outbox extractor can be checked against the exact replay blob set.
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error { return errors.New("stop after checkpoint") },
	}, true)
	if _, err := p.workflow.Reconcile(p.ctx); err == nil {
		t.Fatal("verification seam did not stop")
	}
	var task store.QueueEntry
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entries, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		if len(entries) != 1 {
			return fmt.Errorf("publication tasks = %d", len(entries))
		}
		task = entries[0]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	digests, err := engine.ProductionPublicationBackupPayloadDigests(task)
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) < 2 {
		t.Fatalf("backup digests = %v, want manifest and workspace blobs", digests)
	}
	var payload map[string]any
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	replay, ok := payload["replay"].(map[string]any)
	if !ok {
		t.Fatalf("task replay payload = %#v, want object", payload["replay"])
	}
	replay["manifest_digest"] = "sha256:" + strings.Repeat("0", 64)
	tampered, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	task.Payload = tampered
	if _, err := engine.ProductionPublicationBackupPayloadDigests(task); err == nil {
		t.Fatal("backup extractor accepted replay metadata whose role digest was tampered")
	}
}
