package signet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestSignetWireGoldens(t *testing.T) {
	createdAt := time.Date(2026, 8, 10, 12, 34, 56, 0, time.UTC)
	acceptedAt := createdAt.Add(time.Minute)
	expiresAt := createdAt.Add(24 * time.Hour)
	runID := domain.RunID("run-569")
	conversationID := domain.ConversationID("conversation-569")
	intervalSeconds := int64(3600)

	item := domain.AttentionItem{
		ID: "item-569", ProjectID: "project-569",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-569", RunID: &runID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason:            "the implementation plan awaits approval",
		RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDiscuss},
		PRHeadSHA:         strings.Repeat("1", 40), ItemVersion: 3,
		InterruptionClass: domain.InterruptionPlannedGate,
		ConversationID:    &conversationID, ExpiresWhen: &expiresAt,
		Status: domain.StatusOpen,
	}
	delivery := domain.AttentionDelivery{
		ItemID: item.ID, DeviceID: "device-569", Channel: "ntfy", Attempt: 1,
		SubmittedAt: createdAt, ChannelAcceptedAt: &acceptedAt,
		Status: domain.DeliveryChannelAccepted,
	}
	run := domain.Run{
		ID: runID, ProjectID: item.ProjectID,
		SpecDigest:   domain.Digest("sha256:" + strings.Repeat("2", 64)),
		PolicyDigest: domain.Digest("sha256:" + strings.Repeat("3", 64)),
	}
	invocationID := domain.InvocationID("inv-569")
	observation := domain.RunObservation{
		RunID: runID,
		Milestones: []domain.RunMilestone{{
			RunID: runID, Kind: domain.MilestoneRunSubmitted,
			InvocationID: &invocationID, RecordedAt: createdAt,
		}},
		Invocations: []domain.InvocationObservation{{
			InvocationID: invocationID, RunID: runID,
			Status: domain.ObservedStatusRunning, Live: true, ObservedAt: acceptedAt,
		}},
	}
	conversation := domain.Conversation{ID: conversationID, Status: domain.ConversationIdle}
	schedule := domain.Schedule{
		ID: "schedule-569", ProjectID: item.ProjectID, Kind: domain.ScheduleDoctor,
		Subject:    domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
		Generation: 1, CreatedAt: createdAt, IntervalSeconds: &intervalSeconds,
		Status: domain.ScheduleArmed,
	}
	device := domain.Device{
		ID: "device-569", DisplayName: "Fixture iPhone",
		Status: domain.DeviceActive, PairedAt: createdAt,
	}
	command := domain.Command{
		CommandID: "command-569", DeviceID: device.ID, ItemID: item.ID,
		ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
		Action: domain.ActionApprove, Attachments: []domain.Digest{},
	}

	for name, fixture := range map[string]interface{ Validate() error }{
		"attention item": item,
		"delivery":       delivery,
		"run":            run,
		"conversation":   conversation,
		"schedule":       schedule,
		"device":         device,
		"command":        command,
	} {
		if err := fixture.Validate(); err != nil {
			t.Fatalf("%s fixture: %v", name, err)
		}
	}

	cases := []struct {
		name  string
		value any
	}{
		{
			name: "bootstrap-snapshot",
			value: BootstrapSnapshot{
				SyncEpoch: "sync-epoch-569", Revision: 23,
				AttentionItems: []AttentionItemSnapshot{
					itemSnapshot(item, store.Snapshot{AsOfRevision: 18, EntityVersion: 3}),
				},
				AttentionDeliveries: []AttentionDeliverySnapshot{
					deliverySnapshot(delivery, store.Snapshot{AsOfRevision: 19, EntityVersion: 1}),
				},
				Runs: []RunSnapshot{
					runSnapshot(
						run, store.Snapshot{AsOfRevision: 20, EntityVersion: 2},
						observation, 23,
					),
				},
				Conversations: []ConversationSnapshot{
					conversationSnapshot(conversation, store.Snapshot{AsOfRevision: 21, EntityVersion: 4}),
				},
				Schedules: []ScheduleSnapshot{
					scheduleSnapshot(schedule, store.Snapshot{AsOfRevision: 22, EntityVersion: 1}),
				},
			},
		},
		{name: "server-revision", value: ServerRevision{SyncEpoch: "sync-epoch-569", Revision: 23}},
		{name: "run-timeline", value: runTimeline(observation, 23, acceptedAt)},
		{
			name: "pairing-grant",
			value: PairingGrant{
				DeviceToken: "fixture-device-token",
				Device:      deviceSnapshot(device, store.Snapshot{AsOfRevision: 24, EntityVersion: 1}),
				NtfySubscription: NtfySubscription{
					ServerURL: "https://ntfy.example.test", Topic: "fs-55555555555555555555555555555555",
				},
			},
		},
		{
			name: "command-result",
			value: normalizeCommandResult(CommandResult{
				Record: command, Revision: 25,
			}),
		},
		{
			name: "stale-version-409",
			value: staleVersionResponse{
				Message: "stale item version",
				ReplacementItem: itemSnapshot(
					item, store.Snapshot{AsOfRevision: 26, EntityVersion: 4},
				),
			},
		},
		{
			name:  "attachment-receipt",
			value: attachmentReceipt{Digest: domain.Digest("sha256:" + strings.Repeat("4", 64))},
		},
		{name: "error-response", value: errorResponse{Message: "fixture error"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.MarshalIndent(tc.value, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, tc.name, append(got, '\n'))
		})
	}
}
