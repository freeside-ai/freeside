package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type feedbackInputSource struct{ blobs *signet.BlobStore }

func (s feedbackInputSource) OpenContext(_ context.Context, digest domain.Digest) (io.ReadCloser, error) {
	return s.blobs.Open(digest)
}

func TestProductionReturnToAgentPreflightsCompletePrompt(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(map[bool]string{false: "new feedback", true: "retained pending feedback"}[persisted], func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				policy, err := tx.GetResolvedPolicy(p.ctx, p.runID)
				if err != nil {
					return err
				}
				body, err := json.Marshal(policy.Keys)
				if err != nil {
					return err
				}
				_, err = p.blobs.Put(policy.Digest, bytes.NewReader(body))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			p.startAndRecordExport(t)
			if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
				t.Fatalf("publish baseline: %#v, %v", result, err)
			}
			ready, err := p.attention.GetAttentionItem(p.ctx, domain.ProductionReadyItemID(p.runID))
			if err != nil {
				t.Fatal(err)
			}
			const commandID = "return-size-check"
			const deviceID domain.DeviceID = "feedback-device"
			if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
				return tx.PutDevice(p.ctx, domain.Device{ID: deviceID, DisplayName: "Feedback device", Status: domain.DeviceActive, PairedAt: p.now})
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
				CommandID: commandID, DeviceID: deviceID, ExpectedEntityVersion: ready.EntityVersion,
				Payload: signet.DecisionPayload{
					ItemID: ready.Item.ID, ItemVersion: ready.Item.ItemVersion,
					PRHeadSHA: ready.Item.PRHeadSHA, ArtifactDigests: ready.Item.ArtifactDigests,
					Action: domain.ActionReturnToAgent, Message: "Correct the decision note; preserve code and tests.",
				},
			}); err != nil {
				t.Fatal(err)
			}
			// The package fits by itself; the approved spec, policy and complete
			// candidate patch make the rendered continuation exceed transport limits.
			body := []byte("<!-- freeside:render-prior-artifacts=v1 -->\n" + strings.Repeat("x", 1<<20))
			p.remediationPromptPackage = productionDigest(body)
			if _, err := p.blobs.Put(p.remediationPromptPackage, strings.NewReader(string(body))); err != nil {
				t.Fatal(err)
			}
			materializer, err := exec.NewMaterializer(feedbackInputSource{p.blobs}, exec.MaterializerOptions{
				MaxInputBytes: exec.ProductionMaxInputBytes, MaxTotalBytes: exec.ProductionMaxTotalInputBytes,
			})
			if err != nil {
				t.Fatal(err)
			}
			validationCalls := 0
			validate := func(ctx context.Context, spec exec.StartSpec) error {
				validationCalls++
				inputs, err := materializer.Materialize(ctx, spec)
				if err != nil {
					return err
				}
				if len(inputs.PriorArtifacts()) == 0 {
					t.Fatal("preflight omitted candidate feedback")
				}
				if err := claude.ValidatePromptInputs(inputs); err != nil {
					return errors.Join(engine.ErrProductionInputUndeliverable, err)
				}
				return nil
			}
			if !persisted {
				p.productionDelivery = validate
			}
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
				t.Fatal(err)
			}
			invocation := domain.InvocationID("inv-operator-feedback-" + commandID)
			failureID := domain.ItemID("operator-feedback-undeliverable-" + commandID)
			if persisted {
				// Model durable work queued before validation was wired correctly.
				p.productionDelivery = validate
				p.workflow = p.newEngine(t, productionCrashSeams{}, true)
				if result, err := p.workflow.Reconcile(p.ctx); err != nil || result.InvocationsStarted != 0 {
					t.Fatalf("pending feedback dispatch: %#v, %v", result, err)
				}
				failureID = domain.ItemID("execution-failure-" + string(invocation))
			}
			if validationCalls != 1 {
				t.Fatalf("validation calls = %d, want 1", validationCalls)
			}
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				if _, err := tx.GetExecutionAdmissionRecord(p.ctx, invocation); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("undeliverable feedback admitted: %v", err)
				}
				item, err := tx.GetAttentionItem(p.ctx, failureID)
				if err != nil {
					return err
				}
				if item.Type != domain.AttentionExecutionFailure || item.Status != domain.StatusOpen {
					t.Fatalf("failure item: %#v", item)
				}
				entry, err := tx.GetOutbox(p.ctx, string(invocation))
				if persisted {
					if err != nil || !entry.Dispatched() {
						t.Fatalf("pending refusal not settled: %#v, %v", entry, err)
					}
				} else if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("fresh refusal queued work: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := p.driver.Inspect(p.ctx, invocation); !errors.Is(err, exec.ErrUnknownInvocation) {
				t.Fatalf("undeliverable feedback reached driver: %v", err)
			}
			if result, err := p.reconcileLanes(); err != nil || result.InvocationsStarted != 0 {
				t.Fatalf("refusal replay started work: %#v, %v", result, err)
			}
			if validationCalls != 1 {
				t.Fatalf("refusal replay repeated preflight: %d calls", validationCalls)
			}
			if len(p.forge.pullRequests()) != 1 || p.transport.pushCount() != 1 {
				t.Fatal("feedback refusal republished candidate")
			}
		})
	}
}
