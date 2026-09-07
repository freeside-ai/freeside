package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func scopeConflictFixture() domain.ScopeConflict {
	return domain.ScopeConflict{Version: domain.ScopeConflictEncodingVersion, Paths: []string{"AGENTS.md"}, Decision: domain.Decision{
		Question: "Keep the approved scope?", WhyBlocking: "The renamed helper leaves the repository invariant in AGENTS.md stale.",
		Options: []domain.DecisionOption{{Label: "Keep scope", Tradeoffs: "Leave documentation unmet."}, {Label: "Stop", Tradeoffs: "Start a new run with wider approved paths."}}, Recommendation: "Stop",
	}}
}

func (p *productionPublicationHarness) startScopeConflict(t *testing.T) {
	t.Helper()
	conflict := scopeConflictFixture()
	canonical, err := domain.EncodeScopeConflict(conflict)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(conflict, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	canonicalDigest := productionDigest(canonical)
	if _, err := p.blobs.Put(canonicalDigest, bytes.NewReader(canonical)); err != nil {
		t.Fatal(err)
	}
	digest := productionDigest(body)
	if _, err := p.blobs.Put(digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	p.replay.Evidence = export.EvidenceManifest{Version: export.EvidenceManifestVersion, Entries: []export.EvidenceEntry{{
		Label: export.ScopeConflictEvidenceLabel, MediaType: "application/json", Size: int64(len(body)), Digest: export.Digest(digest),
		Provenance: export.EvidenceProvenance{ProducerClass: export.EvidenceProducerAgent, ProducerInvocationID: string(p.invocation), HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivityNormal},
	}}}
	manifest, err := p.replay.Evidence.Encode()
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := productionDigest(manifest)
	if _, err := p.blobs.Put(manifestDigest, bytes.NewReader(manifest)); err != nil {
		t.Fatal(err)
	}
	p.replay.EvidenceManifestDigest = &manifestDigest
	record := p.startExecutionExport(t, p.replay.HeadSHA)
	record.EvidenceManifestDigest = &manifestDigest
	claim := domain.AgentClaim{
		Label: export.ScopeConflictEvidenceLabel, Artifact: domain.ArtifactID("scope-conflict-" + p.invocation + "-canonical"), Digest: canonicalDigest,
		Provenance: domain.Provenance{ProducerClass: domain.ProducerAgent, ProducerInvocationID: p.invocation, HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal},
		Metadata:   domain.EvidenceMetadata{MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: int64(len(canonical)), CreatedAt: p.now, Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable},
	}
	metadata := claim.Metadata
	metadata.Source = domain.EvidenceSourceRun
	artifact, err := domain.NewArtifact(domain.ArtifactInput{ID: claim.Artifact, Type: domain.ArtifactKindEvidence, Digest: canonicalDigest, Provenance: claim.Provenance, Metadata: metadata}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(p.ctx, artifact); err != nil {
			return err
		}
		return tx.PutAgentClaims(p.ctx, p.invocation, []domain.AgentClaim{claim})
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, p.replay); err != nil {
		t.Fatal(err)
	}
}

func (p *productionPublicationHarness) answerScopeConflict(t *testing.T, action domain.Action) error {
	t.Helper()
	snapshot, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID("question-"+p.invocation))
	if err != nil {
		t.Fatal(err)
	}
	device := domain.DeviceID("scope-device")
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{ID: device, DisplayName: "Scope test", Status: domain.DeviceActive, PairedAt: p.now})
	}); err != nil {
		t.Fatal(err)
	}
	message := ""
	if action != domain.ActionStop {
		message = "Keep scope; the AGENTS.md update remains required follow-up work."
	}
	_, err = p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: "scope-" + string(action), DeviceID: device, ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: snapshot.Item.ID, ItemVersion: snapshot.Item.ItemVersion, PRHeadSHA: snapshot.Item.PRHeadSHA,
			ArtifactDigests: snapshot.Item.ArtifactDigests, Action: action, Message: message,
		},
	})
	return err
}

func TestProductionScopeConflictDecisionSurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint("stop=", stop), func(t *testing.T) {
			t.Parallel()
			issue := 77
			p := newProductionPublicationHarnessWithFiles(t, "", nil, &issue, nil)
			p.startScopeConflict(t)
			before := p.declaration.DeclaredPaths
			var policyBefore domain.ResolvedPolicy
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				var err error
				policyBefore, err = tx.GetResolvedPolicy(p.ctx, p.runID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			result, err := p.reconcileLanes()
			if err != nil {
				t.Fatal(err)
			}
			if result.ReadyItemsCreated != 0 || len(p.forge.pullRequests()) != 0 || p.room.runs != 0 {
				t.Fatalf("conflict escaped hold: %+v", result)
			}
			item, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID("question-"+p.invocation))
			if err != nil {
				t.Fatal(err)
			}
			if item.Item.AgentQuestion.ScopeConflict.Paths[0] != "AGENTS.md" {
				t.Fatal("missing path facts")
			}
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				hold, found, err := tx.GetRunHold(p.ctx, p.runID)
				if err != nil {
					return err
				}
				if !found || hold.Reason != domain.HoldScopeConflict {
					return fmt.Errorf("scope hold = %+v, found %v", hold, found)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, badScope := range [][]string{{"*.ts"}, {"AGENTS.md"}} {
				forged := item.Item
				question := *forged.AgentQuestion
				scope := *question.ScopeConflict
				scope.DeclaredPaths = badScope
				question.ScopeConflict = &scope
				forged.AgentQuestion = &question
				forged.ItemVersion++
				if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(p.ctx, forged) }); err == nil {
					t.Fatal("forged declared scope accepted")
				}
			}
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if err := p.answerScopeConflict(t, domain.ActionAnswerAndRetry); err == nil {
				t.Fatal("unoffered retry accepted")
			}
			action := domain.ActionAnswerWithoutRetry
			if stop {
				action = domain.ActionStop
			}
			if err := p.answerScopeConflict(t, action); err != nil {
				t.Fatal(err)
			}
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			p.now = p.now.Add(2 * time.Minute)
			result, err = p.reconcileLanes()
			if err != nil {
				t.Fatal(err)
			}
			if stop {
				if len(p.forge.pullRequests()) != 0 || result.PublicationTasksCompleted != 1 {
					t.Fatalf("stop result: %+v", result)
				}
			} else {
				if result.ReadyItemsCreated != 1 {
					t.Fatalf("answer result: %+v", result)
				}
				prs := p.forge.pullRequests()
				if len(prs) != 1 || !strings.Contains(prs[0].Body, "## Freeside Scope Decision") || !strings.Contains(prs[0].Body, "AGENTS.md") || !strings.Contains(prs[0].Body, "remains required") {
					t.Fatalf("missing scope decision: %+v", prs)
				}
				p.refuseStrippedScopeRepair(t)
				ready, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID("production-ready-"+p.runID))
				if err != nil || ready.Item.ScopeDecision == nil {
					t.Fatalf("ready decision: %+v, %v", ready, err)
				}
				for _, omit := range []bool{false, true} {
					forged := ready.Item
					decision := *forged.ScopeDecision
					decision.Answer = "All documentation is complete."
					forged.ScopeDecision = &decision
					if omit {
						forged.ScopeDecision = nil
					}
					forged.ItemVersion++
					if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(p.ctx, forged) }); !errors.Is(err, domain.ErrParentKeyMismatch) {
						t.Fatalf("forged/omitted ready decision: %v", err)
					}
				}
			}
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				policy, err := tx.GetResolvedPolicy(p.ctx, p.runID)
				if err != nil {
					return err
				}
				declaration, err := tx.GetWorkUnitDeclarationByRun(p.ctx, p.runID)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(policyBefore, policy) || !reflect.DeepEqual(before, declaration.DeclaredPaths) {
					return errors.New("persisted policy or declaration changed")
				}
				if stop {
					milestones, err := tx.ListRunMilestones(p.ctx, p.runID)
					if err != nil {
						return err
					}
					for _, milestone := range milestones {
						if milestone.Kind == domain.MilestonePublicationBlocked && milestone.Reason != nil && *milestone.Reason == domain.HoldScopeConflict {
							return nil
						}
					}
					return errors.New("stop omitted scope-conflict milestone")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			result, err = p.reconcileLanes()
			if err != nil || result.PublicationTasksCompleted != 0 || result.ReadyItemsCreated != 0 {
				t.Fatalf("replay: %+v, %v", result, err)
			}
		})
	}
}

func TestProductionScopeConflictCannotDisappearDuringRemediation(t *testing.T) {
	p, _ := prepareRemediationPublicationLifecycleWithScope(t, true, true)
	verified := p.room.runs
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.ReadyItemsCreated != 0 || result.PublicationTasksCompleted != 0 || len(p.forge.pullRequests()) != 0 || p.room.runs != verified {
		t.Fatalf("replacement dropped scope obligation: %+v", result)
	}
	item, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID("production-scope-quarantined-1-"+p.runID))
	if err != nil || item.Item.Status != domain.StatusOpen || !strings.Contains(item.Item.Reason, "out-of-scope work remains unmet") {
		t.Fatalf("missing scope quarantine: %+v, %v", item, err)
	}
}

func TestProductionScopeConflictUnreadableEvidenceQuarantines(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startScopeConflict(t)
	digest := string(p.replay.Evidence.Entries[0].Digest)
	path := filepath.Join(p.blobDir, "sha256-"+strings.TrimPrefix(digest, "sha256:"))
	if err := os.WriteFile(path, []byte("corrupt test evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 0 || p.room.runs != 0 || len(p.forge.pullRequests()) != 0 {
		t.Fatalf("unreadable scope evidence = %+v, %v", result, err)
	}
	item, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID("production-scope-quarantined-1-"+p.runID))
	if err != nil || item.Item.Status != domain.StatusOpen {
		t.Fatalf("missing scope quarantine: %+v, %v", item, err)
	}
}

func (p *productionPublicationHarness) refuseStrippedScopeRepair(t *testing.T) {
	t.Helper()
	var checkpoint struct {
		Authorization domain.CandidateAuthorization `json:"authorization"`
		Artifacts     []domain.Artifact             `json:"artifacts"`
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(p.ctx, "production-verification/"+string(p.runID)+"/"+p.replay.HeadSHA)
		if err != nil {
			return err
		}
		return json.Unmarshal(entry.Payload, &checkpoint)
	}); err != nil {
		t.Fatal(err)
	}
	auth := checkpoint.Authorization
	c := publish.Candidate{
		Repo: auth.Repo, BaseRef: "main", HeadSHA: auth.HeadSHA, Title: "Erased scope", Body: "Everything is complete.",
		Artifacts: checkpoint.Artifacts, RecipeDigest: &auth.VerificationRecipeDigest, InvocationID: domain.ProductionPublicationInvocationID(p.runID),
		AuthorizationID: &auth.ID, TrustProfileDigest: &auth.TrustProfileDigest, Advisories: publish.AdvisoryFindings(auth.Findings),
	}
	var digests []domain.Digest
	for _, a := range c.Artifacts {
		digests = append(digests, a.Digest)
	}
	identity, err := publish.DeriveIdentity(publish.IdentityInput{Repo: c.Repo, BaseRef: c.BaseRef, SourceHeadSHA: c.HeadSHA, ArtifactDigests: digests, RecipeDigest: c.RecipeDigest})
	if err != nil {
		t.Fatal(err)
	}
	var outcome publish.Outcome
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(p.ctx, publish.OutcomeKey(identity))
		if err != nil {
			return err
		}
		outcome, err = publish.DecodeOutcome(entry.Payload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := p.forge.pullRequests()[0].Body
	for _, run := range []domain.RunID{"", "unrelated-run"} {
		c.RunID = run
		if err := p.newPublisher(t).ConvergeOutcome(p.ctx, c, map[domain.Digest]bool{auth.VerificationRecipeDigest: true}, identity, outcome); !errors.Is(err, publish.ErrPublicationConflict) {
			t.Fatalf("stripped scope repair = %v", err)
		}
		if p.forge.pullRequests()[0].Body != before {
			t.Fatal("stripped repair changed PR body")
		}
	}
	c.RunID = "" // Ordinary publication admits an unreserved, run-independent attempt.
	c.InvocationID = "scope-converging-attempt"
	for _, removeLiveSection := range []bool{false, true} {
		liveBody := before
		if removeLiveSection {
			liveBody = "Scope notice removed externally.\n\n" + identity.Marker()
		}
		p.forge.mu.Lock()
		p.forge.prs[0].Body = liveBody
		writes := maps.Clone(p.forge.writeCounts)
		p.forge.mu.Unlock()
		if _, err := p.newPublisher(t).Publish(p.ctx, c, map[domain.Digest]bool{auth.VerificationRecipeDigest: true}); !errors.Is(err, publish.ErrPublicationConflict) {
			t.Errorf("converging run erased durable scope (live section removed %v): %v", removeLiveSection, err)
		}
		p.forge.mu.Lock()
		unchanged := reflect.DeepEqual(writes, p.forge.writeCounts) && p.forge.prs[0].Body == liveBody
		p.forge.mu.Unlock()
		if !unchanged {
			t.Errorf("conflicting scope convergence reached the forge (live section removed %v)", removeLiveSection)
		}
		key, err := publish.IntentKey(c.InvocationID, publish.IntentKindPublication)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error { _, err := tx.GetOutbox(p.ctx, key); return err }); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("conflicting convergence committed an intent (live section removed %v): %v", removeLiveSection, err)
		}
	}
	p.forge.mu.Lock()
	p.forge.prs[0].Body = before
	p.forge.mu.Unlock()
	c.RunID = p.runID
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		c.ScopeDecision, err = tx.ScopeDecisionForCandidate(p.ctx, c.RunID, c.HeadSHA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.newPublisher(t).Publish(p.ctx, c, map[domain.Digest]bool{auth.VerificationRecipeDigest: true}); !errors.Is(err, publish.ErrUnauthorizedPublication) {
		t.Fatalf("scope publication without durable producing owner = %v", err)
	}
}
