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
	t.Parallel()
	createdAt := time.Date(2026, 8, 10, 12, 34, 56, 0, time.UTC)
	acceptedAt := createdAt.Add(time.Minute)
	expiresAt := createdAt.Add(24 * time.Hour)
	runID := domain.RunID("run-569")
	taskID := domain.TaskID("task-569")
	conversationID := domain.ConversationID("conversation-569")
	intervalSeconds := int64(3600)
	// The task's implementer line, written by the router at first admission
	// on the second-soonest reset after it passed over a marked pool.
	taskLine, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: taskID, Role: domain.RoleImplementer, Agent: "claude-b",
		Source: domain.TaskLineSourceRouter, SetBy: "router:" + string(taskID),
		Provenance: &domain.TaskLineProvenance{
			PolicyRevision: 2, Tier: domain.TaskLineSelectionSoonestReset, Pool: "pool-b",
			RankedBy: []domain.TaskLineRankingReading{
				{Pool: "pool-a", Window: "seven_day", ResetsAt: createdAt.Add(24 * time.Hour), ObservedAt: createdAt.Add(-time.Hour)},
				{Pool: "pool-b", Window: "seven_day", ResetsAt: createdAt.Add(48 * time.Hour), ObservedAt: createdAt.Add(-time.Hour)},
			},
			PassedOver: &domain.TaskLinePassedOverMark{
				Enrollment: "anthropic-a/claude_code", Pool: "pool-a", Window: "five_hour", Source: domain.ExhaustedMarkProviderRefusal,
				MarkedAt: createdAt.Add(-30 * time.Minute), ResetsAt: new(createdAt.Add(2 * time.Hour)),
			},
		},
	}, 1, nil, createdAt)
	if err != nil {
		t.Fatal(err)
	}

	item := domain.AttentionItem{
		ID: "item-569", ProjectID: "project-569",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-569", RunID: &runID, TaskID: &taskID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason:            "the implementation plan awaits approval",
		RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDiscuss},
		DecisionSurface: domain.DecisionSurfaceRef{
			Epoch: 1, Digest: domain.Digest("sha256:" + strings.Repeat("4", 64)),
		},
		PRHeadSHA: strings.Repeat("1", 40), ItemVersion: 3,
		InterruptionClass: domain.InterruptionPlannedGate,
		ConversationID:    &conversationID, CreatedAt: &createdAt, ExpiresWhen: &expiresAt,
		Status: domain.StatusOpen,
	}
	delivery := domain.AttentionDelivery{
		ItemID: item.ID, DeviceID: "device-569", Channel: "ntfy", Attempt: 1,
		SubmittedAt: createdAt, ChannelAcceptedAt: &acceptedAt,
		Status: domain.DeliveryChannelAccepted,
	}
	run := domain.Run{
		ID: runID, TaskID: taskID, ProjectID: item.ProjectID,
		SpecDigest:   domain.Digest("sha256:" + strings.Repeat("2", 64)),
		PolicyDigest: domain.Digest("sha256:" + strings.Repeat("3", 64)),
	}
	invocationID := domain.InvocationID("inv-569")
	publicationInvocationID := domain.ProductionPublicationInvocationID(runID)
	mergedAt := createdAt.Add(3 * time.Hour)
	// The observation pins a completed run (#1134): the timeline ends with
	// work_unit_completed, and the projection facts carry the completion,
	// the superseding attempt, and the spend figure.
	observation := domain.RunObservation{
		RunID: runID,
		Milestones: []domain.RunMilestone{
			{
				RunID: runID, Kind: domain.MilestoneRunSubmitted,
				InvocationID: &invocationID, RecordedAt: createdAt,
			},
			{
				RunID: runID, Kind: domain.MilestonePublicationReady,
				InvocationID: &publicationInvocationID, RecordedAt: createdAt.Add(2 * time.Hour),
			},
			{
				RunID: runID, Kind: domain.MilestoneWorkUnitCompleted,
				InvocationID: &publicationInvocationID, RecordedAt: mergedAt,
			},
		},
		Invocations: []domain.InvocationObservation{{
			InvocationID: invocationID, RunID: runID,
			Status: domain.ObservedStatusCompleted, Live: false, ObservedAt: acceptedAt,
		}},
	}
	boundIssue := 724
	facts := runProjectionFacts{
		completion: &WorkUnitCompletionFacts{
			PRNumber: 725, MergeCommitSHA: strings.Repeat("5", 40),
			BoundIssue: &boundIssue, RecordedAt: mergedAt,
		},
		cost: &domain.CostSoFar{Currency: "USD", Amount: "17.5", Invocations: 1, Complete: true},
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
	revokedAt := createdAt.Add(48 * time.Hour)
	revokedDevice := domain.Device{
		ID: "device-570", DisplayName: "Fixture Mac",
		Status: domain.DeviceRevoked, PairedAt: createdAt, RevokedAt: &revokedAt,
	}
	pairingFacts := PairingFacts{
		HostDisplayName: "fixture-host.local",
		CodeExpiresAt:   createdAt.Add(10 * time.Minute),
		ConnectionMode:  domain.ConnectionLoopback,
		GrantedScope:    domain.DeviceScopeOperator,
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
		"revoked device": revokedDevice,
		"command":        command,
	} {
		if err := fixture.Validate(); err != nil {
			t.Fatalf("%s fixture: %v", name, err)
		}
	}

	// The run timeline's admitted facts (#979): the implementer's, every fact
	// recorded, and a reviewer of another lineage group.
	timelineFacts := facts
	timelineFacts.admissions = []domain.InvocationAdmissionFacts{
		{
			InvocationID: invocationID, Kind: domain.AdmissionKindExecution,
			Role: new(domain.RoleImplementer), AdmittedAt: acceptedAt,
			AgentDigest:     new(domain.Digest("sha256:" + strings.Repeat("a", 64))),
			SelectionSource: new(domain.AgentSelectionSourceTaskLine), Harness: new(domain.HarnessClientClaudeCode),
			Model: new("claude-opus-5-5"), RequestedEffort: new(domain.EffortHigh), NativeEffort: new("high"),
			CostOwner: new("platform-team"), LineageGroup: new("anthropic"),
			CredentialMode: new(domain.CredentialSubscriptionContained), EgressProfile: new(domain.EgressProviderOnly),
			OperatingMode: new(domain.ModeUnattended), IsolationClass: new("fresh_vm_read_only_volume_handoff"),
		},
		{
			InvocationID: "review-569-1", Kind: domain.AdmissionKindReview,
			Role: new(domain.RoleReviewer), AdmittedAt: acceptedAt.Add(time.Hour),
			AgentDigest:     new(domain.Digest("sha256:" + strings.Repeat("b", 64))),
			SelectionSource: new(domain.AgentSelectionSourceLineup), Harness: new(domain.HarnessClientCodexCLI),
			Model: new("gpt-5.2-codex"), RequestedEffort: new(domain.EffortHigh), NativeEffort: new("high"),
			CostOwner: new("platform-team"), LineageGroup: new("openai"),
			Independence: []domain.IndependenceEntry{
				{WritingRole: domain.RoleImplementer, Lineage: domain.LineageDiffered},
				{WritingRole: domain.RoleRemediator, Lineage: domain.LineageUnknown},
			},
		},
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
					(&Service{}).itemSnapshot(item, store.Snapshot{AsOfRevision: 18, EntityVersion: 3}),
				},
				AttentionDeliveries: []AttentionDeliverySnapshot{
					deliverySnapshot(delivery, store.Snapshot{AsOfRevision: 19, EntityVersion: 1}),
				},
				Tasks: []TaskSnapshot{{AsOfRevision: 23, EntityVersion: 23, Task: Task{
					ID: taskID, ProjectID: item.ProjectID, CreatedAt: createdAt, LastActivityAt: mergedAt,
					DisplayNames: domain.DisplayNames{Project: domain.DisplayName{Text: "owner/repo", Source: domain.DisplayNameSourceName}, Task: domain.DisplayName{Text: "Improve navigation", Source: domain.DisplayNameSourceSpecification}},
					Source: &TaskSource{
						Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: new(domain.ArtifactID("source-569")),
						WorkItemArtifactDigest: new(domain.Digest("sha256:" + strings.Repeat("f", 64))),
					},
					Lifecycle: new(domain.TaskFinished), CurrentPosition: &TaskPosition{RunID: runID},
					CampaignIDs: []domain.CampaignID{}, RunIDs: []domain.RunID{runID},
					WIP: false,
					LifecycleFacts: []TaskLifecycleFact{
						{Kind: domain.TaskLifecycleStarted, RunID: runID, RecordedAt: createdAt},
						{Kind: domain.TaskLifecycleCompleted, RunID: runID, BindingUnitID: new(domain.WorkUnitIDForRun(runID)), RecordedAt: mergedAt},
					},
					TaskLines: []TaskLine{wireTaskLine(taskLine)},
					Attachments: []domain.TaskAttachment{{
						Digest:    "sha256:2222222222222222222222222222222222222222222222222222222222222222",
						Name:      "design_handoff/screens/home.png",
						MediaType: domain.TaskAttachmentMediaPNG, ClaimedMediaType: "image/png", SizeBytes: 48213,
					}},
				}}},
				Runs: []RunSnapshot{
					runSnapshot(
						run, store.Snapshot{AsOfRevision: 20, EntityVersion: 2},
						observation, domain.ConcludeRun(observation), 23,
						&domain.DisplayNames{
							Project: domain.DisplayName{Text: "owner/repo", Source: domain.DisplayNameSourceName},
							Task:    domain.DisplayName{Text: "Improve navigation", Source: domain.DisplayNameSourceSpecification},
						},
						facts,
					),
				},
				Conversations: []ConversationSnapshot{
					conversationSnapshot(conversation, store.Snapshot{AsOfRevision: 21, EntityVersion: 4}),
				},
				Schedules: []ScheduleSnapshot{
					scheduleSnapshot(schedule, store.Snapshot{AsOfRevision: 22, EntityVersion: 1}),
				},
				UsagePools:     usagePoolsFixture(t, createdAt),
				RoutingPolicy:  routingPolicyFixture(t, createdAt),
				WorkflowPolicy: workflowPolicyFixture(t, createdAt),
				AgentCatalog:   bootstrapCatalogFixture(t, agentCatalogFixture(t), createdAt),
				UnattendedOperation: UnattendedOperationSnapshot{
					Admission: domain.UnattendedAdmissionStopped,
					Stops: []UnattendedStop{
						{
							Kind:      domain.UnattendedStopOperator,
							ItemID:    new(domain.ItemID("system-health-unattended-stopped-command-980")),
							CommandID: new("command-980"),
							Since:     new(createdAt.Add(4 * time.Minute)),
						},
						{
							Kind:   domain.UnattendedStopBlockingSystemHealth,
							ItemID: new(domain.ItemID("system-health-daemon-durable-stop-22")),
							Since:  &createdAt,
						},
					},
				},
			},
		},
		{name: "server-revision", value: ServerRevision{
			SyncEpoch: "sync-epoch-569", Revision: 23,
			UnattendedOperation: UnattendedOperationSnapshot{
				Admission: domain.UnattendedAdmissionOpen, Stops: []UnattendedStop{},
			},
			LineupRevision: lineupRevision(agentCatalogFixture(t)),
		}},
		{name: "run-timeline", value: runTimeline(observation, 23, acceptedAt, timelineFacts)},
		{name: "task-timeline", value: taskTimelineFixture(t)},
		{name: "pairing-facts", value: pairingFacts},
		{
			name: "pairing-grant",
			value: PairingGrant{
				DeviceToken: "fixture-device-token",
				Device:      deviceSnapshot(device, store.Snapshot{AsOfRevision: 24, EntityVersion: 1}),
				NtfySubscription: NtfySubscription{
					ServerURL: "https://ntfy.example.test", Topic: "fs-55555555555555555555555555555555",
				},
				Facts: pairingFacts,
			},
		},
		{
			// One active device with recorded activity and notifications on,
			// and one revoked device with neither: last_seen_at renders an
			// instant and an explicit null, notifications_enabled both values.
			name: "device-list",
			value: []DeviceListEntry{
				{
					AsOfRevision: 24, EntityVersion: 1, Device: device, LastSeenAt: new(createdAt.Add(24 * time.Hour)),
					NotificationsEnabled: true,
				},
				{AsOfRevision: 26, EntityVersion: 2, Device: revokedDevice},
			},
		},
		{
			name: "command-result",
			value: normalizeCommandResult(CommandResult{
				Record: command, Revision: 25,
			}),
		},
		{
			name: "task-submission-result",
			value: normalizeCommandResult(CommandResult{
				Submission: &domain.TaskSubmission{
					CommandID: "cmd-570", Origin: domain.TaskSubmissionOriginDevice, DeviceID: new(domain.DeviceID("device-569")), ProjectID: "project-569",
					SourceDigest:       domain.Digest("sha256:" + strings.Repeat("6", 64)),
					TaskID:             "task-570",
					SpecificationRunID: "run-570",
					Name:               domain.DisplayName{Text: "Add a health endpoint", Source: domain.DisplayNameSourceOperator},
				},
				Revision: 26,
			}),
		},
		{
			name: "stale-version-409",
			value: staleVersionResponse{
				Message: "stale item version",
				ReplacementItem: (&Service{}).itemSnapshot(
					item, store.Snapshot{AsOfRevision: 26, EntityVersion: 4},
				),
			},
		},
		{
			name:  "attachment-receipt",
			value: attachmentReceipt{Digest: domain.Digest("sha256:" + strings.Repeat("5", 64))},
		},
		{name: "error-response", value: errorResponse{Message: "fixture error"}},
		{
			name:  "effect-proposal-facts-verified",
			value: effectProposalFactsFixture(domain.ClosureProvenanceVerified, domain.ClosureFlagOriginProposeSite, true, true),
		},
		{
			name:  "effect-proposal-facts-recommended",
			value: effectProposalFactsFixture(domain.ClosureProvenanceRecommended, domain.ClosureFlagOriginProposeSite, true, true),
		},
		{
			// A daemon_fallback proposal always carries resolves=false and, as an
			// initially admitted proposal, no supersedes, so this case also pins
			// the null supersedes rendering.
			name:  "effect-proposal-facts-fallback",
			value: effectProposalFactsFixture(domain.ClosureProvenanceVerified, domain.ClosureFlagOriginDaemonFallback, false, false),
		},
		{
			// The filing arm with a policy milestone; source_issue_closure and
			// supersedes render explicit null.
			name:  "effect-proposal-facts-filing",
			value: followUpFilingFactsFixture(),
		},
		{
			// The adoption arm naming both a recipe digest and a host; the
			// closure and filing arms and supersedes render explicit null.
			name:  "effect-proposal-facts-adoption",
			value: imagePolicyAdoptionFactsFixture(),
		},
	}
	cases = append(cases, struct {
		name  string
		value any
	}{
		// A daemon with no usage pool, no routing policy, and no committed
		// agent tree: an empty list and explicit nulls, never an absent field.
		name: "bootstrap-snapshot-empty",
		value: BootstrapSnapshot{
			SyncEpoch: "sync-epoch-569", Revision: 1,
			AttentionItems: []AttentionItemSnapshot{}, AttentionDeliveries: []AttentionDeliverySnapshot{},
			Runs: []RunSnapshot{}, Tasks: []TaskSnapshot{}, Conversations: []ConversationSnapshot{},
			Schedules:           []ScheduleSnapshot{},
			UnattendedOperation: UnattendedOperationSnapshot{Admission: domain.UnattendedAdmissionOpen, Stops: []UnattendedStop{}},
			UsagePools:          []UsagePoolSnapshot{},
			AgentCatalog:        bootstrapCatalogFixture(t, nil, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
			WorkflowPolicy:      renderWorkflowPolicy(domain.WorkflowPolicyLayers{}, nil, 1),
		},
	})
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

// effectProposalFactsFixture builds a fixed, valid EffectProposalFactsSnapshot
// with UTC-independent constant values. withSupersedes renders the revised-card
// shape (a non-null supersedes carrying the prior digest and prior resolves);
// without it the initially admitted shape (null supersedes).
func effectProposalFactsFixture(
	provenance domain.ClosureProvenance, origin domain.ClosureFlagOrigin, resolves, withSupersedes bool,
) EffectProposalFactsSnapshot {
	snapshot := EffectProposalFactsSnapshot{
		AsOfRevision: 27, EntityVersion: 5, ItemVersion: 2,
		ProposalDigest: domain.Digest("sha256:" + strings.Repeat("7", 64)),
		EffectKind:     domain.EffectSourceIssueClosure,
		SourceIssueClosure: &SourceIssueClosureFacts{
			Target:     domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 724},
			Resolves:   resolves,
			Provenance: provenance,
			Origin:     origin,
			Merge: ProspectiveMergeFacts{
				PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("9", 64)),
				CandidateHeadSHA:    strings.Repeat("a", 40),
				BaseRef:             "main",
				BaseSHA:             strings.Repeat("b", 40),
			},
		},
	}
	if withSupersedes {
		snapshot.Supersedes = &EffectProposalRevisionFacts{
			ProposalDigest:     domain.Digest("sha256:" + strings.Repeat("8", 64)),
			SourceIssueClosure: &EffectProposalRevisionClosureFacts{Resolves: false},
		}
	}
	return snapshot
}

// followUpFilingFactsFixture builds a fixed, valid follow_up_filing facts
// snapshot: the daemon-derived target, both screened text fields, and the
// source link.
func followUpFilingFactsFixture() EffectProposalFactsSnapshot {
	milestone := "1B"
	return EffectProposalFactsSnapshot{
		AsOfRevision: 27, EntityVersion: 5, ItemVersion: 1,
		ProposalDigest: domain.Digest("sha256:" + strings.Repeat("7", 64)),
		EffectKind:     domain.EffectFollowUpFiling,
		FollowUpFiling: &FollowUpFilingFacts{
			Repository: domain.FollowUpFilingRepository{Repo: "owner/repo", RepositoryID: 123},
			Labels:     []string{"deferral", "lane:spine"},
			Milestone:  &milestone,
			Title: domain.ScreenedIssueText{
				Text:    "Bound the retry budget in the sync client",
				Ruleset: domain.IssueTextRulesetGitHubIssue1, Verdict: domain.ScreeningVerdictPassed,
			},
			Body: domain.ScreenedIssueText{
				Text:    "The sync client retries without a budget.\n\nDeferred from review round 2.",
				Ruleset: domain.IssueTextRulesetGitHubIssue1, Verdict: domain.ScreeningVerdictPassed,
			},
			Source: domain.FollowUpFilingSource{
				FindingID:          "finding-1",
				AdjudicationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
				Kind:               domain.FollowUpSourceDeferredDisposition,
			},
		},
	}
}

// imagePolicyAdoptionFactsFixture builds a fixed, valid adoption-arm
// facts snapshot.
func imagePolicyAdoptionFactsFixture() EffectProposalFactsSnapshot {
	recipe := domain.Digest("sha256:" + strings.Repeat("d", 64))
	return EffectProposalFactsSnapshot{
		AsOfRevision: 27, EntityVersion: 5, ItemVersion: 1,
		ProposalDigest: domain.Digest("sha256:" + strings.Repeat("7", 64)),
		EffectKind:     domain.EffectImagePolicyAdoption,
		ImagePolicyAdoption: &ImagePolicyAdoptionFacts{
			Repository:          domain.FollowUpFilingRepository{Repo: "owner/repo", RepositoryID: 123},
			RefusalRecordID:     domain.Digest("sha256:" + strings.Repeat("e", 64)),
			RecipeDigest:        &recipe,
			RegistryAuthorities: []string{"registry.example.com"},
		},
	}
}

// TestEffectProposalFactsProvenanceGoldensDiffer proves the verified and
// recommended provenance render distinct payloads, so a client can tell the two
// trust levels apart (issue acceptance).
func TestEffectProposalFactsProvenanceGoldensDiffer(t *testing.T) {
	t.Parallel()
	verified, err := json.Marshal(effectProposalFactsFixture(
		domain.ClosureProvenanceVerified, domain.ClosureFlagOriginProposeSite, true, true))
	if err != nil {
		t.Fatal(err)
	}
	recommended, err := json.Marshal(effectProposalFactsFixture(
		domain.ClosureProvenanceRecommended, domain.ClosureFlagOriginProposeSite, true, true))
	if err != nil {
		t.Fatal(err)
	}
	if string(verified) == string(recommended) {
		t.Fatal("verified and recommended closure facts rendered identically")
	}
}

// usagePoolsFixture projects the four pool shapes the bootstrap golden pins,
// as of ten minutes after at with a 30-minute refresh age:
//   - claude-a: readings, a model map, a second enrollment with no
//     availability answer yet, and one window that holds both an older
//     refusal and a newer exhausted reading resetting before the refusal;
//   - claude-c: a reading two hours old with no reset, stale before the
//     fixture clock, kept through a failed refresh;
//   - codex-b: a collector that answered unsupported, so statuses and no
//     windows;
//   - spare: no record and no enrollment, so no collector and version 0.
//
// Each record is a valid domain record, so the fixtures double as
// validation-positive cases.
// bootstrapCatalogFixture is the bootstrap's catalog over tree (nil for no
// committed tree): the tree's enrollments, one with a discovery record and
// one the daemon has not discovered yet, as agentCatalogSnapshot reads them.
func bootstrapCatalogFixture(t *testing.T, tree *AgentCatalog, at time.Time) AgentCatalog {
	t.Helper()
	catalog := AgentCatalog{Lineup: []AgentCatalogLineupLine{}}
	if tree != nil {
		catalog = *tree
	}
	discovered := domain.DiscoveredModelCatalog{
		EnrollmentID: "anthropic-a/claude_code", HarnessBuild: "claude-code 2.1.0", ObservedAt: at,
		Models: []domain.DiscoveredModel{
			{
				ID: "opus", ResolvedModel: new("claude-opus-5-5"), DisplayName: "Opus",
				EffortLevels: []string{"low", "medium", "high", "xhigh", "max"},
			},
			{ID: "haiku", ResolvedModel: nil, DisplayName: "Haiku", EffortLevels: []string{}},
		},
	}
	if err := discovered.Validate(); err != nil {
		t.Fatal(err)
	}
	catalog.Enrollments = []AgentCatalogEnrollment{
		{
			EnrollmentID: discovered.EnrollmentID, HarnessClient: domain.HarnessClientClaudeCode,
			Discovery: &AgentCatalogDiscovery{
				HarnessBuild: discovered.HarnessBuild, ObservedAt: discovered.ObservedAt, Models: discovered.Models,
			},
		},
		{EnrollmentID: "anthropic-b/claude_code", HarnessClient: domain.HarnessClientClaudeCode, Discovery: nil},
	}
	return catalog
}

func usagePoolsFixture(t *testing.T, at time.Time) []UsagePoolSnapshot {
	t.Helper()
	now := at.Add(10 * time.Minute)
	const refreshAge = 30 * time.Minute
	enrollment := func(id domain.ClientEnrollmentID, identity domain.AuthIdentityID, client domain.HarnessClientKind) domain.ClientEnrollment {
		return domain.ClientEnrollment{ID: id, AuthIdentityID: identity, HarnessClient: client}
	}
	exhausted := domain.WindowReading{
		Window: "seven_day_opus", EnrollmentID: "enroll-a", Generation: 2, Status: domain.WindowStatusExhausted,
		UsedPercent: new(100), ResetsAt: new(at.Add(72 * time.Hour)), Length: new(7 * 24 * time.Hour), ObservedAt: at,
	}
	claudeA := domain.PoolUsageObservation{
		Pool: "claude-a", Plan: new("max"), Collector: "enroll-a",
		LastAttempt: &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: at},
		Enrollments: []domain.EnrollmentAvailability{
			{EnrollmentID: "enroll-a", Answer: domain.AvailabilitySupported, ObservedAt: at},
		},
		ModelWindows: []domain.ModelWindow{{RouteModelID: "claude-opus", Window: "seven_day_opus"}},
		Windows: []domain.WindowReading{
			{
				Window: "five_hour", EnrollmentID: "enroll-a", Generation: 2, Status: domain.WindowStatusAllowed,
				UsedPercent: new(40), ResetsAt: new(at.Add(3 * time.Hour)), Length: new(5 * time.Hour), ObservedAt: at,
			},
			exhausted,
		},
		RefusalMarks: []domain.RefusalMark{{
			Window: "seven_day_opus", InvocationID: "inv-refused",
			ResetsAt: new(at.Add(96 * time.Hour)), SetAt: at.Add(-time.Hour),
		}},
	}
	claudeC := domain.PoolUsageObservation{
		Pool: "claude-c", Collector: "enroll-c",
		LastAttempt: &domain.CollectionAttempt{Status: domain.CollectionAttemptFailed, At: at, Detail: "usage endpoint timed out"},
		Enrollments: []domain.EnrollmentAvailability{
			{EnrollmentID: "enroll-c", Answer: domain.AvailabilityFailed, ObservedAt: at},
		},
		ModelWindows: []domain.ModelWindow{},
		Windows: []domain.WindowReading{{
			Window: "tokens", EnrollmentID: "enroll-c", Generation: 1, Status: domain.WindowStatusAllowed,
			Quantity: new(int64(120000)), Unit: new("tokens"), ObservedAt: at.Add(-2 * time.Hour),
		}},
	}
	codexB := domain.PoolUsageObservation{
		Pool: "codex-b", Collector: "enroll-b",
		LastAttempt: &domain.CollectionAttempt{
			Status: domain.CollectionAttemptUnsupported, At: at, Detail: "credential exposes no usage",
		},
		Enrollments: []domain.EnrollmentAvailability{
			{EnrollmentID: "enroll-b", Answer: domain.AvailabilityUnsupported, ObservedAt: at},
		},
	}
	for _, observation := range []domain.PoolUsageObservation{claudeA, claudeC, codexB} {
		if err := observation.Validate(); err != nil {
			t.Fatalf("fixture %s: %v", observation.Pool, err)
		}
	}
	pools := []usagePoolInput{
		{
			pool:   domain.UsagePool{ID: "claude-a", Provider: "anthropic", MaxParallelExecutions: 2},
			record: &store.Snapshotted[domain.PoolUsageObservation]{Value: claudeA, Snapshot: store.Snapshot{AsOfRevision: 17, EntityVersion: 4}},
			enrollments: []domain.ClientEnrollment{
				enrollment("enroll-a", "auth-a", domain.HarnessClientClaudeCode),
				enrollment("enroll-a2", "auth-a2", domain.HarnessClientClaudeCode),
			},
		},
		{
			pool:        domain.UsagePool{ID: "claude-c", Provider: "anthropic", MaxParallelExecutions: 1},
			record:      &store.Snapshotted[domain.PoolUsageObservation]{Value: claudeC, Snapshot: store.Snapshot{AsOfRevision: 15, EntityVersion: 3}},
			enrollments: []domain.ClientEnrollment{enrollment("enroll-c", "auth-c", domain.HarnessClientClaudeCode)},
		},
		{
			pool:        domain.UsagePool{ID: "codex-b", Provider: "openai", MaxParallelExecutions: 1},
			record:      &store.Snapshotted[domain.PoolUsageObservation]{Value: codexB, Snapshot: store.Snapshot{AsOfRevision: 16, EntityVersion: 1}},
			enrollments: []domain.ClientEnrollment{enrollment("enroll-b", "auth-b", domain.HarnessClientCodexCLI)},
		},
		{pool: domain.UsagePool{ID: "spare", Provider: "anthropic", MaxParallelExecutions: 1}},
	}
	snapshots := make([]UsagePoolSnapshot, 0, len(pools))
	for _, pool := range pools {
		snapshots = append(snapshots, projectUsagePool(pool, 23, refreshAge))
	}
	// The goldens' claims, checked here so a regenerated golden can't
	// silently drop one.
	if marks := snapshots[0].Pool.RecordedExhaustedMarks; len(marks) != 2 ||
		marks[0].Source != domain.ExhaustedMarkProviderRefusal || marks[1].Source != domain.ExhaustedMarkUsageReading {
		t.Fatalf("claude-a recorded exhausted marks = %+v, want provider_refusal then usage_reading", marks)
	}
	if staleAt := snapshots[1].Pool.Windows[0].StaleAt; staleAt == nil || !staleAt.Before(now) {
		t.Fatalf("claude-c stale_at = %v, want before %s", staleAt, now)
	}
	if windows := snapshots[2].Pool.Windows; len(windows) != 0 {
		t.Fatalf("codex-b windows = %+v, want none", windows)
	}
	return snapshots
}

// routingPolicyFixture is a client-set policy revision ordering two roles'
// enrollments, which the wire carries as ids.
// workflowPolicyFixture shows each origin: a general value, a project value
// over it, and the defaults.
func workflowPolicyFixture(t *testing.T, at time.Time) WorkflowPolicySnapshot {
	t.Helper()
	project := domain.ProjectID("project-569")
	var revisions []domain.WorkflowPolicyRevision
	for i, in := range []domain.WorkflowPolicyInput{
		{Scope: domain.WorkflowPolicyScopeGeneral, Key: domain.WorkflowGateSourceIssueClosure, Value: new("true"), SetBy: "device:device-569"},
		{Scope: domain.WorkflowPolicyScopeProject, ProjectID: &project, Key: domain.WorkflowFollowUpMaxPerDay, Value: new("3"), SetBy: "device:device-569"},
	} {
		revision, err := domain.NewWorkflowPolicyRevision(in, i+1, at)
		if err != nil {
			t.Fatal(err)
		}
		revisions = append(revisions, revision)
	}
	layers, err := domain.NewWorkflowPolicyLayers(revisions)
	if err != nil {
		t.Fatal(err)
	}
	return renderWorkflowPolicy(layers, []domain.ProjectID{project}, 24)
}

func routingPolicyFixture(t *testing.T, at time.Time) *RoutingPolicySnapshot {
	t.Helper()
	policy, err := domain.NewRoutingPolicy(domain.RoutingPolicyInput{
		Roles: []domain.RoutingPolicyRole{
			{Role: domain.RoleReviewer, Enrollments: []domain.ClientEnrollmentID{"anthropic-b/claude_code"}},
			{Role: domain.RoleImplementer, Enrollments: []domain.ClientEnrollmentID{
				"anthropic-b/claude_code", "anthropic-a/claude_code",
			}},
		},
		Source: domain.RoutingPolicySourceClientSet, SetBy: "device:device-569",
		LineupRevision: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}, 2, at)
	if err != nil {
		t.Fatal(err)
	}
	return &RoutingPolicySnapshot{
		AsOfRevision: 23, EntityVersion: 2, Policy: renderRoutingPolicy(policy),
	}
}
