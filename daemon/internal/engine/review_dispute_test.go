package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func parkedReviewDispute(t *testing.T, prior bool) (*findingAdjudicationFixture, signet.ClientCommand) {
	t.Helper()
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP0,
		&domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}, "low", "high")
	f.writePath(t, f.headRoot)
	if prior {
		f.workflow.findingAdjudicator = newDeterministicFindingAdjudicator(modelRouteEntry(t, f.finding.ID, domain.RouteDecline, domain.ConfidenceHigh))
	} else {
		f.workflow.findingAdjudicator = nil
	}
	state, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPending {
		t.Fatalf("park = %v, %v", state, err)
	}
	var item domain.AttentionItem
	var snapshot store.Snapshot
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		item, snapshot, err = tx.GetAttentionItemSnapshot(f.ctx, productionReviewItemID(f.task.RunID, 1))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !item.Offers(domain.ActionAdjudicateFindings) {
		t.Fatalf("dispute does not offer adjudication: %#v", item)
	}
	return f, signet.ClientCommand{
		CommandID: "human-routes", DeviceID: "device-test", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionAdjudicateFindings, ItemVersion: item.ItemVersion,
			PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
			FindingRoutes: []signet.AlternativeChoice{{FindingID: f.finding.ID, Route: domain.RouteDecline}},
		},
	}
}

func TestReviewDisputeHumanDecisionContinuesAndReplays(t *testing.T) {
	for _, prior := range []bool{false, true} {
		for _, route := range []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer, domain.RouteRemediate} {
			t.Run(string(route)+map[bool]string{false: "/initial", true: "/successor"}[prior], func(t *testing.T) {
				f, command := parkedReviewDispute(t, prior)
				command.Payload.FindingRoutes[0].Route = route
				first, err := f.signet.Submit(f.ctx, command)
				if err != nil {
					t.Fatal(err)
				}
				replay, err := f.signet.Submit(f.ctx, command)
				if err != nil || replay.Revision != first.Revision {
					t.Fatalf("replay = %#v, %v", replay, err)
				}
				var digest domain.Digest
				for pass := 0; pass < 2; pass++ {
					state, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
					if err != nil {
						t.Fatal(err)
					}
					if route != domain.RouteRemediate && state != productionReviewPassed {
						t.Fatalf("human disposition still parked: %v", state)
					}
					if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
						artifact, err := tx.GetFindingAdjudicationForRound(f.ctx, f.task.RunID, 1)
						if err != nil {
							return err
						}
						wantRevision := 1
						if prior {
							wantRevision = 2
						}
						if artifact.Revision != wantRevision || artifact.Decision == nil || artifact.Decision.CommandID != command.CommandID || artifact.Entries[0].Producer != domain.AdjudicationProducerHuman || artifact.Entries[0].Route != route {
							t.Fatalf("artifact = %#v", artifact)
						}
						if digest != "" && digest != artifact.Digest {
							t.Fatal("replay changed artifact")
						}
						digest = artifact.Digest
						if route == domain.RouteRemediate {
							routes, err := effectiveFindingRoutesTx(f.ctx, tx, artifact)
							if err != nil {
								return err
							}
							if routes[f.finding.ID] != domain.RouteRemediate {
								t.Fatal("human remediation is not executable")
							}
						}
						if route != domain.RouteRemediate {
							dispositions, err := tx.ListFindingDispositions(f.ctx, f.task.RunID)
							if err != nil {
								return err
							}
							if len(dispositions) != 1 {
								t.Fatalf("dispositions = %#v", dispositions)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestReviewDisputeRejectsInvalidChoices(t *testing.T) {
	for name, mutate := range map[string]func(*signet.ClientCommand){
		"missing": func(c *signet.ClientCommand) { c.Payload.FindingRoutes = nil },
		"duplicate": func(c *signet.ClientCommand) {
			c.Payload.FindingRoutes = append(c.Payload.FindingRoutes, c.Payload.FindingRoutes[0])
		},
		"foreign":   func(c *signet.ClientCommand) { c.Payload.FindingRoutes[0].FindingID = "foreign" },
		"unoffered": func(c *signet.ClientCommand) { c.Payload.FindingRoutes[0].Route = domain.RouteDispute },
		"stale":     func(c *signet.ClientCommand) { c.Payload.ItemVersion++ },
	} {
		t.Run(name, func(t *testing.T) {
			f, command := parkedReviewDispute(t, false)
			mutate(&command)
			if _, err := f.signet.Submit(f.ctx, command); err == nil {
				t.Fatal("invalid command accepted")
			}
		})
	}
	for _, differentID := range []bool{false, true} {
		f, command := parkedReviewDispute(t, false)
		if _, err := f.signet.Submit(f.ctx, command); err != nil {
			t.Fatal(err)
		}
		if differentID {
			command.CommandID = "second-decision"
		} else {
			command.Payload.FindingRoutes[0].Route = domain.RouteDefer
		}
		if _, err := f.signet.Submit(f.ctx, command); err == nil {
			t.Fatal("conflicting or second decision accepted")
		}
	}
}

func TestReviewDisputeRecoversAtHumanArtifactBoundary(t *testing.T) {
	for _, side := range AllDurableTransitionSides {
		t.Run(string(side), func(t *testing.T) {
			f, command := parkedReviewDispute(t, false)
			if _, err := f.signet.Submit(f.ctx, command); err != nil {
				t.Fatal(err)
			}
			f.workflow.transitionHook = func(transition DurableTransition, observed DurableTransitionSide) error {
				if transition == DurableTransitionFindingAdjudication && observed == side {
					return errors.New("injected crash")
				}
				return nil
			}
			if _, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot); err == nil {
				t.Fatal("crash hook not reached")
			}
			f.workflow.transitionHook = nil
			for pass := 0; pass < 2; pass++ {
				if _, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestReviewDisputeBindingRejectsTamperedRows(t *testing.T) {
	for name, mutate := range map[string]func(*domain.ReviewDisputeBinding){
		"message":  func(b *domain.ReviewDisputeBinding) { b.Findings[0].Message = "forged" },
		"location": func(b *domain.ReviewDisputeBinding) { b.Findings[0].Location.Path = "daemon/other.go" },
		"severity": func(b *domain.ReviewDisputeBinding) { b.Findings[0].Severity = domain.FindingSeverityP3 },
		"identity": func(b *domain.ReviewDisputeBinding) {
			b.Findings[0].External = &domain.DisputedFindingExternal{ReviewerLogin: "forged", ThreadID: "thread"}
		},
		"routes": func(b *domain.ReviewDisputeBinding) {
			b.Findings[0].OfferedRoutes = []domain.AdjudicationRoute{domain.RouteDecline}
		},
		"missing row": func(b *domain.ReviewDisputeBinding) { b.FindingIDs = append(b.FindingIDs, "missing") },
		"foreign row": func(b *domain.ReviewDisputeBinding) { b.FindingIDs[0] = "foreign"; b.Findings[0].FindingID = "foreign" },
		"completion":  func(b *domain.ReviewDisputeBinding) { b.CompletionEvidence = adjudicationDigest("other") },
	} {
		t.Run(name, func(t *testing.T) {
			f, command := parkedReviewDispute(t, false)
			if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
				item, err := tx.GetAttentionItem(f.ctx, command.Payload.ItemID)
				if err != nil {
					return err
				}
				mutate(item.ReviewDispute)
				item.ItemVersion++
				return tx.PutAttentionItem(f.ctx, item)
			}); err == nil {
				t.Fatal("tampered dispute persisted")
			}
		})
	}
}

func TestReviewDisputeRejectsAnotherCandidateHead(t *testing.T) {
	f, command := parkedReviewDispute(t, false)
	err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(f.ctx, command.Payload.ItemID)
		if err != nil {
			return err
		}
		item.PRHeadSHA = strings.Repeat("c", 40)
		item.ItemVersion++
		return tx.PutAttentionItem(f.ctx, item)
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("changed head = %v", err)
	}
}

func TestReviewDisputeHTTPCommandExecutesHumanRoutes(t *testing.T) {
	f, command := parkedReviewDispute(t, false)
	handler := signet.NewHTTPHandler(f.signet, func(*http.Request) (domain.DeviceID, bool) {
		return command.DeviceID, true
	})
	body, err := json.Marshal(map[string]any{
		"command_id": command.CommandID, "device_id": command.DeviceID,
		"expected_entity_version": command.ExpectedEntityVersion,
		"expected_bindings":       map[string]any{},
		"payload": map[string]any{
			"kind": "decision", "item_id": command.Payload.ItemID, "action": command.Payload.Action,
			"item_version": command.Payload.ItemVersion, "pr_head_sha": command.Payload.PRHeadSHA,
			"artifact_digests": command.Payload.ArtifactDigests, "finding_routes": command.Payload.FindingRoutes,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/commands", bytes.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
		}
	}
	state, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPassed {
		t.Fatalf("execute HTTP decision = %v, %v", state, err)
	}
}

func TestHumanArtifactRequiresExactDurableDecision(t *testing.T) {
	for name, mutate := range map[string]func(*domain.FindingAdjudication){
		"command": func(a *domain.FindingAdjudication) { a.Decision.CommandID = "other" },
		"version": func(a *domain.FindingAdjudication) { a.Decision.ItemVersion++ },
		"time":    func(a *domain.FindingAdjudication) { a.CreatedAt = a.CreatedAt.Add(time.Second) },
		"route": func(a *domain.FindingAdjudication) {
			a.Entries[0], _ = domain.NewHumanAdjudicationEntry(a.Entries[0].FindingID, domain.RouteDefer)
		},
		"rationale": func(a *domain.FindingAdjudication) { a.Entries[0].Rationale = "forged rationale" },
	} {
		t.Run(name, func(t *testing.T) {
			f, command := parkedReviewDispute(t, false)
			if _, err := f.signet.Submit(f.ctx, command); err != nil {
				t.Fatal(err)
			}
			err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
				item, err := tx.GetAttentionItem(f.ctx, command.Payload.ItemID)
				if err != nil {
					return err
				}
				entry, err := domain.NewHumanAdjudicationEntry(f.finding.ID, domain.RouteDecline)
				if err != nil {
					return err
				}
				artifact, err := domain.NewHumanFindingAdjudication(f.task.RunID, 1, f.binding.run.SpecDigest, f.record.InstructionDigest, f.binding.resolvedPolicy.Digest,
					[]domain.FindingAdjudicationEntry{entry}, domain.AdjudicationDecision{CommandID: command.CommandID, ItemID: item.ID, ItemVersion: command.Payload.ItemVersion}, *item.DecidedAt)
				if err != nil {
					return err
				}
				mutate(&artifact)
				artifact.Digest, err = artifact.ComputeDigest()
				if err != nil {
					return err
				}
				return tx.PutFindingAdjudication(f.ctx, artifact)
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("forged artifact = %v", err)
			}
		})
	}
}
