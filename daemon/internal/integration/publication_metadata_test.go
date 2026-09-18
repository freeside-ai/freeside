package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const publicAccount = "# Preserve retries after restart\n\nKeep the whole approved change and preserve recorded results.\n\nScope excludes live deployment; live checks remain unrun.\n"

// Exercise the real client command, specification return and human approval,
// then hand the exact reserved implementation run to the publication fixture.
func submitClientForPublication(t *testing.T, h *publicationHarness, image domain.ProjectImage, project domain.ProjectID, name, commandID string, prior *productionPublicationHarness) (engine.ProductionRun, []byte) {
	t.Helper()
	prov := domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: productionDigest([]byte("client-policy"))}
	keys := []domain.PolicyKey{
		{Key: "paths", Value: "README.md", Provenance: prov},
		{Key: specify.PolicySpecApproval, Value: "true", Provenance: prov},
		{Key: specify.PolicyMaxIterations, Value: "2", Provenance: prov},
		{Key: specify.PolicyStageActiveTime, Value: "1m", Provenance: prov},
		{Key: specify.PolicyApprovalWait, Value: "1h", Provenance: prov},
		{Key: specify.PolicyResearchAllowlist, Value: "https://docs.example", Provenance: prov},
		{Key: specify.PolicyResearchMaxBytes, Value: "1024", Provenance: prov},
	}
	service := signet.NewService(h.store, signet.WithBlobStore(h.blobs), signet.WithTaskSubmitter(engine.NewTaskSubmitter(h.blobs, func(domain.ProjectID) (engine.ManualInitiator, bool) {
		return engine.ManualInitiator{PolicyKeys: keys, CommitAuthor: productionPublicationMetadata().CommitAuthor}, true
	})))
	if err := h.store.Write(h.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(h.ctx, domain.Device{ID: "client-device", DisplayName: "Client", Status: domain.DeviceActive, PairedAt: h.now})
	}); err != nil {
		t.Fatal(err)
	}
	result, err := service.Submit(h.ctx, signet.ClientCommand{
		CommandID: commandID, DeviceID: "client-device", Kind: domain.CommandKindSubmitTask,
		SubmitTask: signet.SubmitTaskPayload{ProjectID: project, Source: "https://github.com/example/project/issues/82", Name: name},
	})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		ImplementationRunID domain.RunID `json:"implementation_run_id"`
	}
	if err := h.store.Read(h.ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(h.ctx, engine.SpecificationDispatchMarkerKey(result.Submission.SpecificationRunID))
		if err != nil {
			return err
		}
		return json.Unmarshal(entry.Payload, &request)
	}); err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewStageDriverAt(filepath.Join(h.workDir, "production-driver"))
	if err != nil {
		t.Fatal(err)
	}
	title := "Refined task name"
	if err := specifyfake.Script(driver, domain.SpecificationInvocationID(result.Submission.SpecificationRunID, 1), 0, 0, specify.Output{
		Specification: &specify.Specification{Title: &title, Summary: "Approved change", Body: "# Approved Specification\n\nPreserve recorded results after restart.", Addressals: []specify.Addressal{}},
	}); err != nil {
		t.Fatal(err)
	}
	fetcher, err := specify.NewFetcher(h.store, h.blobs, nil)
	if err != nil {
		t.Fatal(err)
	}
	prompt := productionDigest([]byte("prompt package"))
	putProductionBlob(t, h, prompt, []byte("prompt package"))
	identity := testIdentity.ID
	specification := engine.WithSpecification(engine.SpecificationConfig{
		Fetcher: fetcher, Blobs: h.blobs, Now: func() time.Time { return h.now }, PromptPackageDigest: prompt,
		ValidateDelivery: func(context.Context, exec.StartSpec) error { return nil },
	})
	workflow, err := engine.New(h.store, service, driver,
		engine.WithAdmission(productionPublicationBackend(t), nil, engine.AdmissionEnvironment{
			OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
			EgressProfile: domain.EgressProviderOnly, ImageRef: image.ImageRef, PromptPackageDigest: prompt,
			VendorInstructions: engine.VendorInstructionConfig{Vendor: domain.AgentVendorClaude, Delivery: domain.VendorInstructionDeliveryAppendFile, HostPath: "/nonexistent/client-fixture-CLAUDE.md"},
			Base:               domain.BaseRevision{Repo: h.profile.Repo, RepositoryID: h.profile.RepositoryID, BaseRef: "main", BaseSHA: h.baseSHA},
			Workspace:          "client-specification", AuthIdentityID: &identity,
		}, func() time.Time { return h.now }),
		specification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if prior != nil {
		// A second submission shares a daemon with the retained production
		// attempt; both lanes must be composed while specification runs.
		prior.driver = driver
		workflow = prior.newEngineForMode(t, productionCrashSeams{}, true,
			map[domain.Digest]bool{prior.recipeD: true}, domain.ModeUnattended, false, specification)
	}
	if _, err := workflow.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	approval, err := service.GetAttentionItem(h.ctx, domain.ItemID("spec-approval-"+string(request.ImplementationRunID)+"-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Submit(h.ctx, signet.ClientCommand{
		CommandID: "approve-" + commandID, DeviceID: "client-device", ExpectedEntityVersion: approval.EntityVersion,
		Payload: signet.DecisionPayload{ItemID: approval.Item.ID, ItemVersion: approval.Item.ItemVersion, ArtifactDigests: approval.Item.ArtifactDigests, Action: domain.ActionApprove},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	var run domain.Run
	if err := h.store.Read(h.ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(h.ctx, request.ImplementationRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reader, err := h.blobs.Open(run.SpecDigest)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err != nil || closeErr != nil {
		t.Fatalf("read approved specification: %v, %v", err, closeErr)
	}
	return engine.ProductionRun{Run: run, InvocationID: productionInvocationForRun(run.ID), StageID: domain.StageID("implement-" + string(run.ID))}, body
}

func TestClientSubmissionPublishesCandidateMetadata(t *testing.T) {
	for _, name := range []string{"", "Operator task name"} {
		t.Run(name, func(t *testing.T) {
			p := newProductionPublicationHarnessWithMetadata(t, newPublicationHarness(t), "", nil, nil, nil, engine.ProductionPublication{}, nil, name)
			if p.declaration == nil || p.declaration.BoundIssue != nil || p.declaration.CompletionCriterion != domain.CompletionBoundPRMerged {
				t.Fatal("client source granted issue-closing authority")
			}
			p.replay = withPublicAccount(t, p, p.replay, publicAccount)
			p.startAndRecordExport(t)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			assertPublicMetadata(t, p)
		})
	}
}

// Export the real launcher-shaped public and private channels, retaining their
// original sensitivity, then let production replay run the real importer.
func withPublicAccount(t *testing.T, p *productionPublicationHarness, replay engine.ProductionReplay, content string) engine.ProductionReplay {
	t.Helper()
	workspace := t.TempDir()
	writeFile(t, workspace, export.PublicationEvidencePath, content)
	writeFile(t, workspace, export.SummaryEvidencePath, "Private operational context must not appear in a PR.")
	descriptor := export.EvidenceSourceManifest{Version: export.EvidenceSourceVersion, Sources: []export.EvidenceSource{
		{Label: export.PublicationEvidenceLabel, Path: export.PublicationEvidencePath, MediaType: "text/markdown", HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivityNormal, ProducerInvocationID: string(replay.InvocationID)},
		{Label: export.SummaryEvidenceLabel, Path: export.SummaryEvidencePath, MediaType: "text/markdown", HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivitySensitive, ProducerInvocationID: string(replay.InvocationID)},
	}}
	body, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, workspace, export.EvidenceDescriptorPath, string(body))
	handoff := filepath.Join(t.TempDir(), "handoff")
	if _, err := export.Export(os.DirFS(workspace), handoff, export.Options{}); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(filepath.Join(handoff, export.EvidenceFilename)) //nolint:gosec // test export
	if err != nil {
		t.Fatal(err)
	}
	replay.Evidence, err = export.DecodeEvidenceManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	digest := productionDigest(body)
	putProductionBlob(t, p.publicationHarness, digest, body)
	replay.EvidenceManifestDigest = &digest
	for _, entry := range replay.Evidence.Entries {
		body, err := os.ReadFile(filepath.Join(handoff, export.EvidenceBlobsDirname, "sha256", strings.TrimPrefix(string(entry.Digest), "sha256:"))) //nolint:gosec // exporter-generated digest
		if err != nil {
			t.Fatal(err)
		}
		putProductionBlob(t, p.publicationHarness, domain.Digest(entry.Digest), body)
	}
	return replay
}

func newPublicMetadataHarness(t *testing.T) *productionPublicationHarness {
	t.Helper()
	metadata := productionPublicationMetadata()
	metadata.Title, metadata.Body = "", ""
	metadata.Recipe = "freeside.client-publication/v1"
	metadata.SourceIssue = "https://github.com/example/project/issues/82"
	p := newProductionPublicationHarnessWithMetadata(t, newPublicationHarness(t), "", nil, nil, nil, metadata, nil)
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged, DeclaredPaths: []string{"README.md"},
	}, p.runID, p.projectID, p.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error { return tx.RecordWorkUnitDeclaration(p.ctx, declaration) }); err != nil {
		t.Fatal(err)
	}
	p.declaration = &declaration
	return p
}

func assertPublicMetadata(t *testing.T, p *productionPublicationHarness) string {
	t.Helper()
	prs := p.forge.pullRequests()
	if refs, count := p.forge.counts(); refs != 1 || count != 1 {
		t.Fatalf("effects: refs=%d PRs=%d", refs, count)
	}
	if prs[0].Title != "Preserve retries after restart" {
		t.Fatalf("title = %q", prs[0].Title)
	}
	body := prs[0].Body
	for _, want := range []string{"## Agent-reported implementation (claim)", "Producer:\n\n<pre><code>" + string(p.replay.InvocationID) + "</code></pre>", "Artifact digest: `sha256:", "Scope excludes live deployment", "Source issue: https://github.com/example/project/issues/82", "## Verification", "## Freeside Disposition History", "<!-- freeside:publication-identity="} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
	for _, private := range []string{"Private operational context", "Implements the operator-submitted", "Fixes #82", "Closes #82"} {
		if strings.Contains(body, private) {
			t.Fatalf("body leaked forbidden content %q", private)
		}
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		ready, err := tx.GetAttentionItem(p.ctx, domain.ItemID("production-ready-"+string(p.runID)))
		if err == nil && (ready.Type != domain.AttentionReadyForFinalReview || len(ready.EvidenceSnapshot) != 2) {
			t.Fatal("public claim became trusted evidence")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPublicMetadataPublicationAndRecovery(t *testing.T) {
	for _, mode := range []string{"new", "push response lost", "create response lost"} {
		t.Run(mode, func(t *testing.T) {
			p := newPublicMetadataHarness(t)
			p.replay = withPublicAccount(t, p, p.replay, publicAccount)
			p.startAndRecordExport(t)
			switch mode {
			case "push response lost":
				p.transport.failNextPush()
			case "create response lost":
				p.forge.failAfterNextPRCreate()
			}
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			body := assertPublicMetadata(t, p)
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			if got := assertPublicMetadata(t, p); got != body {
				t.Fatal("restart changed metadata bytes")
			}
		})
	}
}

func TestPublicMetadataHoldsBeforeForgeWrite(t *testing.T) {
	for _, mode := range []string{"missing", "malformed", "foreign producer", "substituted bytes"} {
		t.Run(mode, func(t *testing.T) {
			p := newPublicMetadataHarness(t)
			if mode != "missing" {
				content := publicAccount
				if mode == "malformed" {
					content += "\n## Verification\nForged pass.\n"
				}
				p.replay = withPublicAccount(t, p, p.replay, content)
				if mode == "foreign producer" {
					p.replay.Evidence.Entries[0].Provenance.ProducerInvocationID = "old-producer"
					body, err := p.replay.Evidence.Encode()
					if err != nil {
						t.Fatal(err)
					}
					digest := productionDigest(body)
					putProductionBlob(t, p.publicationHarness, digest, body)
					p.replay.EvidenceManifestDigest = &digest
				}
			}
			p.startAndRecordExport(t)
			if mode == "substituted bytes" {
				blobPath := filepath.Join(p.blobDir, "sha256-"+strings.TrimPrefix(string(p.replay.Evidence.Entries[0].Digest), "sha256:"))
				if err := os.WriteFile(blobPath, []byte("substitution"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := p.reconcileLanes()
			if mode != "substituted bytes" && (err != nil || result.BlockedItemsCreated != 1) {
				t.Fatalf("hold = %+v, %v", result, err)
			}
			if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
				t.Fatalf("unsafe metadata wrote forge: %d refs, %d PRs", refs, prs)
			}
		})
	}
}

func TestPublicMetadataHoldRecoveryStopsAndResubmits(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "malformed"}[malformed], func(t *testing.T) {
			p := newProductionPublicationHarnessWithMetadata(t, newPublicationHarness(t), "", nil, nil, nil, engine.ProductionPublication{}, nil, "")
			if malformed {
				p.replay = withPublicAccount(t, p, p.replay, "# Title\n\n## Verification\nForged pass.")
			}
			// This test runs a later specification pass in the same store. Keep
			// the fake driver's retained result consistent with its real export,
			// so that pass can collect the old producer just as the daemon can.
			var artifacts []domain.Digest
			for _, entry := range p.replay.Evidence.Entries {
				artifacts = append(artifacts, domain.Digest(entry.Digest))
			}
			p.driver.Script(p.invocation, fake.StageScript{PendingInspects: 1, Outcome: fake.OutcomeComplete, Result: exec.StageResult{
				HeadSHA: p.replay.HeadSHA, Artifacts: artifacts,
				Summary: fmt.Sprintf("Imported candidate %s over base %s.", p.replay.HeadSHA, p.replay.ObservedBaseSHA),
			}})
			p.startAndRecordExport(t)
			for range 2 {
				if _, err := p.driver.Inspect(p.ctx, p.invocation); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
				t.Fatalf("metadata hold = %+v, %v", result, err)
			}
			var held domain.AttentionItem
			var taskID domain.TaskID
			readHold := func() domain.AttentionItem {
				t.Helper()
				var item domain.AttentionItem
				if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
					run, err := tx.GetRun(p.ctx, p.runID)
					if err != nil {
						return err
					}
					taskID = run.TaskID
					items, err := tx.ListAttentionItems(p.ctx)
					for _, snapshot := range items {
						candidate := snapshot.Value
						if candidate.Type == domain.AttentionPublishBlocked && candidate.Subject.RunID != nil && *candidate.Subject.RunID == p.runID {
							item = candidate
						}
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return item
			}
			held = readHold()
			for _, required := range []string{"stop_task", "POST /commands", "cancellation=confirmed", "updated producer prompt package", "new task with a new command ID", "existing PR remains unchanged"} {
				if !strings.Contains(held.Reason, required) {
					t.Fatalf("hold omits recovery step %q", required)
				}
			}
			if held.Offers(domain.ActionStop) || held.Status != domain.StatusOpen {
				t.Fatal("metadata hold offered an unsupported card Stop")
			}
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			if got := readHold(); !reflect.DeepEqual(got, held) {
				t.Fatal("restart changed immutable hold or recovery instructions")
			}
			synctest.Test(t, func(t *testing.T) {
				service := signet.NewService(p.store)
				state, err := p.store.ServerState(p.ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := service.Submit(p.ctx, signet.ClientCommand{
					CommandID: "stop-metadata-task", DeviceID: "client-device", Kind: domain.CommandKindStopTask,
					ExpectedEntityVersion: state.Revision,
					StopTask:              signet.StopTaskPayload{TaskID: taskID, ProjectID: p.projectID, ExpectedSyncEpoch: state.SyncEpoch},
				}); err != nil {
					t.Fatal(err)
				}
				coordinator, err := engine.New(p.store, service, p.driver, engine.WithTaskCancellationRuntime(engine.TaskCancellationRuntime{
					Timeout: time.Second,
					StopRun: func(ctx context.Context, run domain.Run, reviews []domain.ReviewRequestRecord) error {
						// These fake adapters own no OS processes. Cancel and collect
						// their durable terminals to prove specification/implementation settled;
						// the fixture's synchronous verification room has already returned.
						for _, stage := range run.Stages {
							for _, attempt := range stage.Attempts {
								if err := p.driver.Cancel(ctx, attempt.InvocationID); err != nil {
									return err
								}
								result, err := p.driver.Collect(ctx, attempt.InvocationID)
								if err != nil || (result.Status != exec.StatusCompleted && result.Status != exec.StatusCanceled) {
									t.Errorf("owned %s stage %s: status=%s, error=%v", stage.Name, attempt.InvocationID, result.Status, err)
									return errors.New("owned fake stage lacks a durable completion")
								}
							}
						}
						for _, review := range reviews {
							if _, err := p.reviewer.Poll(ctx, review.InvocationID); err != nil {
								t.Errorf("owned review %s: %v", review.InvocationID, err)
								return err
							}
						}
						return nil
					},
				}))
				if err != nil {
					t.Fatal(err)
				}
				if err := coordinator.ReconcileTaskCancellations(p.ctx); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if err := coordinator.ReconcileTaskCancellations(p.ctx); err != nil {
					t.Fatal(err)
				}
				if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
					task, err := tx.GetTask(p.ctx, taskID)
					if err == nil && (task.Cancellation == nil || task.Cancellation.State != domain.TaskCancellationConfirmed || domain.TaskWIP(task)) {
						t.Fatalf("held task failed to stop and release WIP: %+v", task.Cancellation)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
				t.Fatal("held or stopped task wrote the forge")
			}
			replacement := newProductionPublicationHarnessWithMetadata(t, p.publicationHarness, "", nil, nil, nil, engine.ProductionPublication{}, p, "", "replacement-publication")
			if replacement.runID == p.runID {
				t.Fatal("new submission reused cancelled implementation identity")
			}
			replacement.replay = withPublicAccount(t, replacement, replacement.replay, publicAccount)
			replacement.startAndRecordExport(t)
			if _, err := replacement.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			assertPublicMetadata(t, replacement)
		})
	}
}

func TestPublicMetadataRepairsDriftFromSameCandidate(t *testing.T) {
	p := newPublicMetadataHarness(t)
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	p.workflow = p.newEngine(t, productionCrashSeams{afterReady: func() error { return errors.New("interrupt after ready") }}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("ready crash seam did not fire")
	}
	want := assertPublicMetadata(t, p)
	p.forge.mu.Lock()
	p.forge.prs[0].Title = "external title"
	p.forge.prs[0].Body = strings.Replace(p.forge.prs[0].Body, "Keep the whole approved change", "Externally changed prose", 1)
	p.forge.mu.Unlock()
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if got := assertPublicMetadata(t, p); got != want {
		t.Fatal("drift repair changed original rendering")
	}
}

func TestPublicMetadataRemediationUsesNewProducer(t *testing.T) {
	p := newPublicMetadataHarness(t)
	p.replay = withPublicAccount(t, p, p.replay, "# Initial candidate\n\nInitial whole change.")
	testProductionAdjudicatedRemediation(t, p, true)
}

func TestPublicMetadataFeedbackUsesNewProducer(t *testing.T) {
	testPublishedFeedbackRetry(t, "public-metadata")
}

func TestPublicMetadataCancellationFencesPublicationAndRepair(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "before publication", true: "before repair"}[published], func(t *testing.T) {
			p := newPublicMetadataHarness(t)
			p.replay = withPublicAccount(t, p, p.replay, publicAccount)
			interrupt := func() error { return errors.New("pause at publication boundary") }
			seams := productionCrashSeams{afterVerification: interrupt}
			if published {
				seams = productionCrashSeams{afterReady: interrupt}
			}
			p.workflow = p.newEngine(t, seams, true)
			p.startAndRecordExport(t)
			if _, err := p.reconcileLanes(); err == nil {
				t.Fatal("boundary did not pause")
			}
			var taskID domain.TaskID
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				run, err := tx.GetRun(p.ctx, p.runID)
				taskID = run.TaskID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			confirmFixtureTask(t, p.store, taskID)
			if published {
				p.forge.mu.Lock()
				p.forge.prs[0].Title = "external title after Stop"
				p.forge.mu.Unlock()
			}
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			want := 0
			if published {
				want = 1
			}
			if refs, prs := p.forge.counts(); refs != want || prs != want {
				t.Fatal("Stop allowed a new publication effect")
			}
			if published && p.forge.pullRequests()[0].Title != "external title after Stop" {
				t.Fatal("Stop allowed metadata repair")
			}
		})
	}
}
