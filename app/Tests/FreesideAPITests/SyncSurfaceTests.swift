import Foundation
import FreesideAPI
import HTTPTypes
import OpenAPIRuntime
import Testing

/// The mock's sync envelope (plan §5.14): bootstrap is the one canonical
/// full-cache read, the heartbeat is the loss detector, and an epoch
/// rotation simulates a daemon restore.
@Suite struct SyncSurfaceTests {
    @Test func taskTimelineRoutePreservesGroupingAndObservations() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let task = try #require(TaskFixtures.defaultTasks().first { $0.task.run_ids.contains(RunFixtures.activeRunID) })
            .task
        let timeline = try await client.getTaskTimeline(path: .init(task_id: task.id)).ok.body.json
        #expect(timeline.task_id == task.id)
        #expect(timeline.name == task.display_names.task)
        #expect(timeline.events.map(\.kind) == [.task_created])
        #expect(Set(timeline.sections.flatMap { $0.runs.map(\.run_id) }) == Set(task.run_ids))
        let run = try #require(timeline.sections.flatMap(\.runs).first { $0.run_id == RunFixtures.activeRunID })
        #expect(run.role?.value1 == .implementation)
        #expect(run.attempt_number == 2)
        #expect(run.parent_run_id == "run-freeside-656")
        #expect(run.hold?.value1.reason == .verification_findings)
        let source = try #require(RunFixtures.defaultTimelines().first { $0.run_id == run.run_id })
        #expect(run.milestones == source.milestones.sorted { $0.recorded_at > $1.recorded_at })
        let missing = try await client.getTaskTimeline(path: .init(task_id: "missing"))
        guard case .notFound = missing else {
            Issue.record("Unknown task must return 404")
            return
        }
        await server.rotateEpoch(revision: 2)
        let restored = try await client.getTaskTimeline(path: .init(task_id: task.id)).ok.body.json
        #expect(restored.as_of_revision == 2)
    }

    @Test func taskTimelineReflectsCurrentRunObservations() async throws {
        let task = try #require(TaskFixtures.defaultTasks().first { $0.task.run_ids.contains(RunFixtures.activeRunID) })
        var source = try #require(RunFixtures.defaultTimelines().first { $0.run_id == RunFixtures.activeRunID })
        source.milestones = source.milestones.filter { $0.kind == .run_submitted }
        source.hold = nil
        let timelines = RunFixtures.defaultTimelines().map { $0.run_id == source.run_id ? source : $0 }
        let server = MockServer(tasks: [task], timelines: timelines)
        let client = APIClientFactory.mock(server: server)
        let before = try await client.getTaskTimeline(path: .init(task_id: task.task.id)).ok.body.json
        let seededRun = try #require(before.sections.flatMap(\.runs).first { $0.run_id == source.run_id })
        #expect(seededRun.milestones.map(\.kind) == [.run_submitted])
        #expect(seededRun.hold == nil)

        let recordedAt = RunFixtures.screenshotInstant.addingTimeInterval(60)
        await server.advanceTime(to: recordedAt)
        await server.recordMilestone(runID: source.run_id, kind: .invocation_started)
        let after = try await client.getTaskTimeline(path: .init(task_id: task.task.id)).ok.body.json
        let current = try await client.getRunTimeline(path: .init(run_id: source.run_id)).ok.body.json
        let changedRun = try #require(after.sections.flatMap(\.runs).first { $0.run_id == source.run_id })
        #expect(after.as_of_revision == current.as_of_revision)
        #expect(after.as_of_revision > before.as_of_revision)
        #expect(after.as_of == recordedAt)
        #expect(changedRun.milestones == current.milestones.sorted { $0.recorded_at > $1.recorded_at })
        #expect(changedRun.milestones.map(\.kind) == [.invocation_started, .run_submitted])
        #expect(changedRun.milestones.first?.recorded_at == recordedAt)

        let emptyClient = APIClientFactory.mock(server: MockServer(tasks: []))
        guard case .notFound = try await emptyClient.getTaskTimeline(path: .init(task_id: task.task.id)) else {
            Issue.record("An unseeded task must return 404")
            return
        }
    }

    @Test func taskTimelineWireIncludesRequiredNulls() async throws {
        var timelines = RunFixtures.defaultTimelines()
        let heldIndex = try #require(timelines.firstIndex { $0.run_id == RunFixtures.activeRunID })
        try #require(timelines[heldIndex].hold != nil)
        timelines[heldIndex].hold?.value1.invocation_id = nil
        let transport = MockServerTransport(server: MockServer(timelines: timelines))
        let eventNullableKeys = [
            "campaign_id", "run_id", "approved_spec_digest", "specification_run_id", "pr_number", "merge_commit_sha",
        ]
        let runNullableKeys = ["role", "attempt_number", "attempt_reason", "parent_run_id", "superseded_by", "hold"]
        var sawLegacy = false
        var sawNullHold = false
        var sawHoldWithoutInvocation = false
        var sawMerge = false
        for task in TaskFixtures.defaultTasks() {
            let request = HTTPRequest(
                method: .get, scheme: "https", authority: "freeside.invalid",
                path: "/tasks/\(task.task.id)/timeline")
            let (response, body) = try await transport.send(
                request, body: nil, baseURL: try #require(URL(string: "https://freeside.invalid")),
                operationID: "getTaskTimeline")
            #expect(response.status == .ok)
            let data = try await Data(collecting: #require(body), upTo: 1 << 20)
            let root = try #require(JSONSerialization.jsonObject(with: data) as? [String: Any])
            var events = try #require(root["events"] as? [[String: Any]])
            for section in try #require(root["sections"] as? [[String: Any]]) {
                #expect(section.keys.contains("campaign_id"))
                let legacy = section["campaign_id"] is NSNull
                sawLegacy = sawLegacy || legacy
                events += try #require(section["events"] as? [[String: Any]])
                for run in try #require(section["runs"] as? [[String: Any]]) {
                    #expect(Set(runNullableKeys).isSubset(of: Set(run.keys)))
                    if legacy {
                        #expect(runNullableKeys.allSatisfy { run[$0] is NSNull })
                    }
                    sawNullHold = sawNullHold || run["hold"] is NSNull
                    if let hold = run["hold"] as? [String: Any] {
                        #expect(hold.keys.contains("invocation_id"))
                        if run["run_id"] as? String == RunFixtures.activeRunID {
                            #expect(hold["invocation_id"] is NSNull)
                            #expect(hold["reason"] as? String == "verification_findings")
                            #expect(run["parent_run_id"] as? String == "run-freeside-656")
                            sawHoldWithoutInvocation = true
                        }
                    }
                    for milestone in try #require(run["milestones"] as? [[String: Any]]) {
                        #expect(
                            Set(["invocation_id", "terminal", "outcome", "reason"]).isSubset(of: Set(milestone.keys)))
                    }
                    events += try #require(run["events"] as? [[String: Any]])
                }
            }
            for event in events {
                #expect(Set(eventNullableKeys).isSubset(of: Set(event.keys)))
                if event["kind"] as? String == "task_created" {
                    #expect(eventNullableKeys.allSatisfy { event[$0] is NSNull })
                }
                if event["kind"] as? String == "pr_merged" {
                    let completion = try #require(
                        timelines.first { $0.run_id == event["run_id"] as? String }?.completion?.value1)
                    #expect(event["pr_number"] as? Int == completion.pr_number)
                    #expect(event["merge_commit_sha"] as? String == completion.merge_commit_sha)
                    sawMerge = true
                }
            }
        }
        #expect(sawLegacy && sawNullHold && sawHoldWithoutInvocation && sawMerge)
    }

    @Test func taskTimelineOrdersNewlySubmittedRunsFirst() async throws {
        var task = try #require(TaskFixtures.defaultTasks().first { $0.task.run_ids.contains(RunFixtures.activeRunID) })
        let runs = RunFixtures.defaultRuns().filter { $0.run.task_id == task.task.id }.map {
            var snapshot = $0
            snapshot.run.campaign_id = nil
            snapshot.run.attempt_number = nil
            snapshot.run.attempt_reason = nil
            snapshot.run.parent_run_id = nil
            snapshot.run.superseded_by = nil
            return snapshot
        }
        task.task.campaign_ids = []
        let timelines = RunFixtures.defaultTimelines().filter { task.task.run_ids.contains($0.run_id) }.map {
            var timeline = $0
            timeline.milestones = []
            timeline.hold = nil
            timeline.completion = nil
            return timeline
        }
        let server = MockServer(runs: runs, tasks: [task], timelines: timelines)
        let client = APIClientFactory.mock(server: server)
        let before = try await client.getTaskTimeline(path: .init(task_id: task.task.id)).ok.body.json
        let section = try #require(before.sections.first { $0.runs.count > 1 })
        let runID = try #require(section.runs.last).run_id
        #expect(section.runs.first?.run_id != runID)

        await server.recordMilestone(runID: runID, kind: .run_submitted)
        let after = try await client.getTaskTimeline(path: .init(task_id: task.task.id)).ok.body.json
        #expect(after.sections.first?.campaign_id == section.campaign_id)
        #expect(after.sections.first?.runs.first?.run_id == runID)
    }

    @Test func taskTimelineOrdersLegacySectionByNewestSubmission() async throws {
        var task = try #require(TaskFixtures.defaultTasks().first)
        var runs = Array(RunFixtures.defaultRuns().prefix(3))
        try #require(runs.count == 3)
        let at = RunFixtures.screenshotInstant
        var timelines: [Components.Schemas.RunTimeline] = []
        for index in runs.indices {
            runs[index].run.task_id = task.task.id
            runs[index].run.project_id = task.task.project_id
            runs[index].run.campaign_id = index == 2 ? "campaign-current" : nil
            runs[index].run.attempt_number = index == 2 ? 1 : nil
            runs[index].run.attempt_reason = nil
            runs[index].run.parent_run_id = nil
            let runID = runs[index].run.id
            let hour = [0, 2, 1][index]
            var timeline = try #require(RunFixtures.defaultTimelines().first { $0.run_id == runID })
            timeline.milestones = [
                .init(
                    run_id: runID, kind: .run_submitted, invocation_id: "inv-\(runID)",
                    recorded_at: at.addingTimeInterval(Double(hour) * 3600))
            ]
            timelines.append(timeline)
        }
        task.task.created_at = at
        task.task.last_activity_at = at.addingTimeInterval(7200)
        task.task.run_ids = [runs[0].run.id, runs[2].run.id, runs[1].run.id]
        task.task.current_position?.value1.run_id = runs[1].run.id
        task.task.campaign_ids = ["campaign-current"]
        let client = APIClientFactory.mock(server: MockServer(runs: runs, tasks: [task], timelines: timelines))
        let timeline = try await client.getTaskTimeline(path: .init(task_id: task.task.id)).ok.body.json
        #expect(timeline.sections.count == 2)
        #expect(timeline.sections.first?.campaign_id == nil)
        #expect(timeline.sections.first?.runs.map(\.run_id) == [runs[1].run.id, runs[0].run.id])
        #expect(timeline.sections.last?.campaign_id == "campaign-current")
    }

    @Test func taskTimelineRejectsMalformedTaskSeeds() async throws {
        let task = try #require(TaskFixtures.defaultTasks().first { $0.task.run_ids.contains(RunFixtures.activeRunID) })
        var zeroVersion = task
        zeroVersion.entity_version = 0
        var duplicateCampaign = task
        duplicateCampaign.task.campaign_ids.append(try #require(task.task.campaign_ids.first))
        var invalidName = task
        invalidName.task.display_names.task.source = .name
        for invalid in [zeroVersion, duplicateCampaign, invalidName] {
            let client = APIClientFactory.mock(server: MockServer(tasks: [invalid]))
            let output = try await client.getTaskTimeline(path: .init(task_id: task.task.id))
            guard case .undocumented(let status, _) = output else {
                Issue.record("Malformed task must fail reconstruction")
                continue
            }
            #expect(status == 500)
        }
    }

    @Test func taskTimelineRejectsMalformedRunSeeds() async throws {
        let run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID })
        var zeroVersion = run
        zeroVersion.entity_version = 0
        var invalidLineage = run
        invalidLineage.run.attempt_reason = " "
        for invalid in [zeroVersion, invalidLineage] {
            let client = APIClientFactory.mock(server: MockServer(runs: [invalid]))
            let output = try await client.getTaskTimeline(path: .init(task_id: run.run.task_id))
            guard case .undocumented(let status, _) = output else {
                Issue.record("Malformed task run must fail reconstruction")
                continue
            }
            #expect(status == 500)
        }
    }

    @Test func taskTimelineRejectsInconsistentMembership() async throws {
        let task = try #require(TaskFixtures.defaultTasks().first { $0.task.run_ids.contains(RunFixtures.activeRunID) })
        let runs = RunFixtures.defaultRuns().filter { $0.run.task_id == task.task.id }
        try #require(runs.count > 1)
        let validClient = APIClientFactory.mock(server: MockServer(runs: runs, tasks: [task]))
        _ = try await validClient.getTaskTimeline(path: .init(task_id: task.task.id)).ok.body.json

        var foreignRun = runs
        foreignRun[0].run.task_id = "task-elsewhere"
        var foreignProject = runs
        foreignProject[0].run.project_id = "project-elsewhere"
        var omittedRun = task
        omittedRun.task.run_ids.removeFirst()
        var missingCampaign = task
        missingCampaign.task.campaign_ids = []
        for (name, seededTask, seededRuns) in [
            ("missing run", task, Array(runs.dropLast())),
            ("foreign run", task, foreignRun),
            ("unlisted run", omittedRun, runs),
            ("foreign project", task, foreignProject),
            ("missing campaign", missingCampaign, runs),
        ] {
            let client = APIClientFactory.mock(server: MockServer(runs: seededRuns, tasks: [seededTask]))
            let output = try await client.getTaskTimeline(path: .init(task_id: task.task.id))
            guard case .undocumented(let status, _) = output else {
                Issue.record("\(name) must fail reconstruction")
                continue
            }
            #expect(status == 500, "\(name)")
        }
    }

    @Test func taskTimelineChecksCampaignsInRunHistoryOrder() async throws {
        var task = try #require(TaskFixtures.defaultTasks().first)
        var runs = Array(RunFixtures.defaultRuns().prefix(2)).sorted { $0.run.id > $1.run.id }
        try #require(runs.count == 2)
        let campaigns = ["campaign-z", "campaign-a"]
        for index in runs.indices {
            runs[index].run.task_id = task.task.id
            runs[index].run.project_id = task.task.project_id
            runs[index].run.campaign_id = campaigns[index]
            runs[index].run.attempt_number = 1
            runs[index].run.attempt_reason = nil
            runs[index].run.parent_run_id = nil
        }
        task.task.run_ids = runs.map(\.run.id)
        task.task.current_position?.value1.run_id = runs[1].run.id
        task.task.campaign_ids = campaigns
        let validClient = APIClientFactory.mock(server: MockServer(runs: runs, tasks: [task]))
        _ = try await validClient.getTaskTimeline(path: .init(task_id: task.task.id)).ok.body.json

        task.task.campaign_ids.reverse()
        let invalidClient = APIClientFactory.mock(server: MockServer(runs: runs, tasks: [task]))
        let output = try await invalidClient.getTaskTimeline(path: .init(task_id: task.task.id))
        guard case .undocumented(let status, _) = output else {
            Issue.record("Campaign order must agree with first occurrences in run history")
            return
        }
        #expect(status == 500)
    }

    @Test func taskTimelineRequiresCampaignSubmissionEvidence() async throws {
        let task = try #require(TaskFixtures.defaultTasks().first { $0.task.run_ids.contains(RunFixtures.activeRunID) })
        let timelines = RunFixtures.defaultTimelines()
        var missingSubmission = try #require(timelines.first { $0.run_id == RunFixtures.activeRunID })
        missingSubmission.milestones.removeAll { $0.kind == .run_submitted }
        for (name, seeds) in [
            ("missing timeline", timelines.filter { $0.run_id != RunFixtures.activeRunID }),
            ("missing submission", timelines.map { $0.run_id == RunFixtures.activeRunID ? missingSubmission : $0 }),
        ] {
            let client = APIClientFactory.mock(server: MockServer(tasks: [task], timelines: seeds))
            let output = try await client.getTaskTimeline(path: .init(task_id: task.task.id))
            guard case .undocumented(let status, _) = output else {
                Issue.record("Campaign run with \(name) must fail reconstruction")
                continue
            }
            #expect(status == 500, "\(name)")
        }
    }

    @Test func taskTimelineRejectsMalformedObservationSeeds() async throws {
        let active = try #require(RunFixtures.defaultTimelines().first { $0.run_id == RunFixtures.activeRunID })
        var foreignMilestone = active
        foreignMilestone.milestones[0].run_id = "another-run"
        var foreignHold = active
        foreignHold.hold?.value1.run_id = "another-run"
        var unrecordedCompletion = try #require(RunFixtures.defaultTimelines().first { $0.completion != nil })
        unrecordedCompletion.milestones.removeAll { $0.kind == .work_unit_completed }
        for source in [foreignMilestone, foreignHold, unrecordedCompletion] {
            let task = try #require(TaskFixtures.defaultTasks().first { $0.task.run_ids.contains(source.run_id) })
            let timelines = RunFixtures.defaultTimelines().map { $0.run_id == source.run_id ? source : $0 }
            let client = APIClientFactory.mock(server: MockServer(timelines: timelines))
            let output = try await client.getTaskTimeline(path: .init(task_id: task.task.id))
            guard case .undocumented(let status, _) = output else {
                Issue.record("Malformed task observations must fail reconstruction")
                continue
            }
            #expect(status == 500)
        }
    }

    @Test func reviewEvidenceJSONPreservesInvalidUTF8() throws {
        let round = RunFixtures.reviewRound(.completed, availability: .available)
        var evidence = RunFixtures.reviewEvidence(runID: RunFixtures.activeRunID, round: round)
        let events: [UInt8] = [0x65, 0xff, 0x00, 0x0a]
        let result: [UInt8] = [0x72, 0xfe, 0xc0]
        evidence.events = .init(events)
        evidence.result = .init(result)
        evidence.collection_evidence = .init(value1: "sha256:" + String(repeating: "e", count: 64))
        let body = try JSONEncoder().encode(evidence)
        let decoded = try JSONDecoder().decode(Components.Schemas.ReviewEvidence.self, from: body)
        #expect(Array(try #require(decoded.events).data) == events)
        #expect(Array(try #require(decoded.result).data) == result)
        #expect(decoded.collection_evidence == evidence.collection_evidence)
    }

    @Test func reviewEvidenceRoutePreservesRoundHeadAndClaimLabels() async throws {
        var timeline = try #require(RunFixtures.defaultTimelines().first { $0.run_id == RunFixtures.activeRunID })
        let round = RunFixtures.reviewRound(.completed, findings: true, availability: .available)
        timeline.review = .init(value1: .init(rounds: [round]))
        let client = APIClientFactory.mock(server: MockServer(timelines: [timeline]))
        let projected = try await client.getRunTimeline(path: .init(run_id: timeline.run_id)).ok.body.json
        #expect(projected.review?.value1.rounds == [round])
        let evidence = try await client.getReviewEvidence(path: .init(run_id: timeline.run_id, round: 1)).ok.body.json
        #expect(evidence.invocation_id == round.invocation_id)
        #expect(evidence.source_head_sha == round.head_sha)
        #expect(evidence.source == round.source)
        #expect(evidence.content_kind == .reviewer_output)
        #expect(!evidence.publish_eligible)
        #expect(evidence.availability == .available)
        #expect(evidence.events != nil)
        let missing = try await client.getReviewEvidence(path: .init(run_id: timeline.run_id, round: 2))
        guard case .notFound = missing else {
            Issue.record("A different round must not receive retained output")
            return
        }
    }

    @Test func bootstrapCarriesTheCursorAndTheWholeInbox() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let bootstrap = try await client.getSyncBootstrap().ok.body.json
        let heartbeat = try await client.getSyncRevision().ok.body.json
        let listed = try await client.listAttentionItems().ok.body.json
        let runs = try await client.listRuns().ok.body.json
        let schedules = try await client.listSchedules().ok.body.json

        // The full-cache cursor pair matches the heartbeat's ServerState
        // read, and the item collection is the same canonical list the
        // list endpoint serves.
        #expect(bootstrap.sync_epoch == heartbeat.sync_epoch)
        #expect(bootstrap.revision == heartbeat.revision)
        #expect(bootstrap.attention_items == listed)
        #expect(bootstrap.attention_deliveries.isEmpty)
        #expect(bootstrap.runs == runs)
        #expect(bootstrap.tasks == TaskFixtures.defaultTasks())
        #expect(Set(bootstrap.tasks.flatMap { $0.task.run_ids }) == Set(runs.map { $0.run.id }))
        #expect(bootstrap.schedules == schedules)
        #expect(bootstrap.conversations == AttentionFixtures.defaultConversations())

        let timeline = try await client.getRunTimeline(
            path: .init(run_id: RunFixtures.activeRunID)
        ).ok.body.json
        #expect(timeline.run_id == RunFixtures.activeRunID)
        #expect(timeline.as_of_revision == heartbeat.revision)
        #expect(!timeline.milestones.isEmpty)
    }

    @Test func runReadsDeriveTimestampsFromTimelineFacts() async throws {
        let observedID = RunFixtures.activeRunID
        let legacyID = RunFixtures.legacyRunID
        let submittedAt = Date(timeIntervalSince1970: 1_786_600_000)
        let milestoneAt = submittedAt.addingTimeInterval(60)
        let holdAt = submittedAt.addingTimeInterval(120)
        let invocationAt = submittedAt.addingTimeInterval(180)
        let forgedAt = submittedAt.addingTimeInterval(3_600)

        var observed = try #require(
            RunFixtures.defaultRuns().first { $0.run.id == observedID })
        observed.run.created_at = forgedAt
        observed.run.last_activity_at = forgedAt
        var legacy = try #require(
            RunFixtures.defaultRuns().first { $0.run.id == legacyID })
        legacy.run.created_at = forgedAt
        legacy.run.last_activity_at = forgedAt

        let observedTimeline = Components.Schemas.RunTimeline(
            as_of_revision: 12,
            as_of: invocationAt,
            run_id: observedID,
            milestones: [
                .init(
                    run_id: observedID,
                    kind: .run_submitted,
                    invocation_id: "inv-observed-1",
                    recorded_at: submittedAt),
                .init(
                    run_id: observedID,
                    kind: .invocation_started,
                    invocation_id: "inv-observed-1",
                    recorded_at: milestoneAt),
            ],
            hold: .init(
                value1: .init(
                    run_id: observedID,
                    invocation_id: "inv-observed-1",
                    reason: .verification_findings,
                    first_observed_at: holdAt,
                    last_observed_at: holdAt)),
            invocations: [
                .init(
                    invocation_id: "inv-observed-1",
                    run_id: observedID,
                    status: .running,
                    live: true,
                    observed_at: invocationAt)
            ],
            completion: nil,
            billable_cost_so_far: nil)
        let legacyTimeline = Components.Schemas.RunTimeline(
            as_of_revision: 12,
            as_of: submittedAt,
            run_id: legacyID,
            milestones: [],
            invocations: [],
            completion: nil,
            billable_cost_so_far: nil)
        let server = MockServer(
            runs: [observed, legacy],
            timelines: [observedTimeline, legacyTimeline])
        let client = APIClientFactory.mock(server: server)

        let listed = try await client.listRuns().ok.body.json
        let fetched = try await client.getRun(path: .init(run_id: observedID)).ok.body.json
        let bootstrap = try await client.getSyncBootstrap().ok.body.json

        for runs in [listed, bootstrap.runs] {
            let projected = try #require(runs.first { $0.run.id == observedID })
            #expect(projected.run.created_at == submittedAt)
            #expect(projected.run.last_activity_at == invocationAt)
            let unobserved = try #require(runs.first { $0.run.id == legacyID })
            #expect(unobserved.run.created_at == nil)
            #expect(unobserved.run.last_activity_at == nil)
        }
        #expect(fetched.run.created_at == submittedAt)
        #expect(fetched.run.last_activity_at == invocationAt)
    }

    @Test func legacyRunTimestampsArePresentAsExplicitNullsOnTheMockWire() async throws {
        let transport = MockServerTransport(server: MockServer())
        let request = HTTPRequest(
            method: .get,
            scheme: "https",
            authority: "freeside.invalid",
            path: "/runs")
        let (_, body) = try await transport.send(
            request,
            body: nil,
            baseURL: try #require(URL(string: "https://freeside.invalid")),
            operationID: "listRuns")
        let data = try await Data(collecting: #require(body), upTo: 1 << 20)
        let rows = try #require(
            JSONSerialization.jsonObject(with: data) as? [[String: Any]])
        let snapshot = try #require(
            rows.first { ($0["run"] as? [String: Any])?["id"] as? String == RunFixtures.legacyRunID })
        let run = try #require(snapshot["run"] as? [String: Any])

        #expect(run.keys.contains("created_at"))
        #expect(run["created_at"] is NSNull)
        #expect(run.keys.contains("last_activity_at"))
        #expect(run["last_activity_at"] is NSNull)
        for key in ["campaign_id", "attempt_number", "attempt_reason", "parent_run_id"] {
            #expect(run.keys.contains(key))
            #expect(run[key] is NSNull)
        }
    }

    @Test func legacyReadinessIsPresentAsExplicitNullOnTheMockWire() async throws {
        var legacy = AttentionFixtures.fixture(type: .ready_for_final_review)
        legacy.item.readiness = nil
        legacy.item.readiness_detail = nil
        legacy.item.yield_history = nil
        let transport = MockServerTransport(server: MockServer(items: [legacy]))
        let request = HTTPRequest(
            method: .get,
            scheme: "https",
            authority: "freeside.invalid",
            path: "/attention/items")
        let (_, body) = try await transport.send(
            request,
            body: nil,
            baseURL: try #require(URL(string: "https://freeside.invalid")),
            operationID: "listAttentionItems")
        let data = try await Data(collecting: #require(body), upTo: 1 << 20)
        let rows = try #require(
            JSONSerialization.jsonObject(with: data) as? [[String: Any]])
        let row = try #require(rows.first)
        let item = try #require(row["item"] as? [String: Any])

        #expect(item.keys.contains("readiness"))
        #expect(item["readiness"] is NSNull)
        #expect(item.keys.contains("readiness_detail"))
        #expect(item["readiness_detail"] is NSNull)
        #expect(item.keys.contains("yield_history"))
        #expect(item["yield_history"] is NSNull)
    }

    @Test func bootstrapFailsClosedOnOneInvalidRow() async throws {
        // One row the daemon could never serve fails the whole bootstrap
        // (the single-read gate), never a partial snapshot that would
        // advance a client's full-cache cursor over a hole.
        var forged = AttentionFixtures.fixture(type: .spec_approval)
        forged.item.artifact_digests.removeLast()
        let valid = AttentionFixtures.fixture(type: .agent_question)
        let client = APIClientFactory.mock(server: MockServer(items: [forged, valid]))

        let output = try await client.getSyncBootstrap()
        guard case .undocumented(let statusCode, _) = output else {
            Issue.record("expected a failed bootstrap, got \(output)")
            return
        }
        #expect(statusCode == 500)
    }

    @Test func invalidRunReadsAreAuthoritativeServerFailures() async throws {
        var forged = try #require(
            RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID })
        forged.run.attempt_reason = nil
        let client = APIClientFactory.mock(server: MockServer(runs: [forged]))

        let list = try await client.listRuns()
        guard case .undocumented(let listStatus, _) = list else {
            Issue.record("expected list reconstruction failure, got \(list)")
            return
        }
        #expect(listStatus == 500)
        let get = try await client.getRun(path: .init(run_id: forged.run.id))
        guard case .undocumented(let getStatus, _) = get else {
            Issue.record("expected get reconstruction failure, got \(get)")
            return
        }
        #expect(getStatus == 500)
        let bootstrap = try await client.getSyncBootstrap()
        guard case .undocumented(let bootstrapStatus, _) = bootstrap else {
            Issue.record("expected bootstrap reconstruction failure, got \(bootstrap)")
            return
        }
        #expect(bootstrapStatus == 500)
    }

    @Test func rotatedEpochReachesBothSyncReadsWithoutTouchingRows() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before = try await client.getSyncRevision().ok.body.json
        let rowsBefore = try await client.listAttentionItems().ok.body.json
        let tasksBefore = try await client.getSyncBootstrap().ok.body.json.tasks

        await server.rotateEpoch()

        let heartbeat = try await client.getSyncRevision().ok.body.json
        let bootstrap = try await client.getSyncBootstrap().ok.body.json
        #expect(heartbeat.sync_epoch != before.sync_epoch)
        #expect(bootstrap.sync_epoch == heartbeat.sync_epoch)
        // A restore replaces the epoch, not the data a client refetches.
        #expect(bootstrap.attention_items == rowsBefore)
        #expect(bootstrap.tasks == tasksBefore)
    }

    @Test func restoreCanRewindTheRevisionUnderTheNewEpoch() async throws {
        // A restored daemon resumes from the restored state's revision,
        // which may sit behind a client's cached cursors (test 8's
        // "discard newer cursors" half); the mock can express that.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        await server.advance(itemID: "item-spec_approval")
        await server.advance(itemID: "item-spec_approval")
        let advanced = try await client.getSyncRevision().ok.body.json

        await server.rotateEpoch(revision: 1)

        let restored = try await client.getSyncRevision().ok.body.json
        #expect(restored.sync_epoch != advanced.sync_epoch)
        #expect(restored.revision < advanced.revision)
        let bootstrap = try await client.getSyncBootstrap().ok.body.json
        #expect(!bootstrap.tasks.isEmpty)
        #expect(bootstrap.tasks.allSatisfy { $0.as_of_revision == restored.revision })
    }

    @Test func advanceOpensAGapBetweenHeartbeatAndAFullSnapshot() async throws {
        // The raw material of the revision-gap rule (test 11): after a
        // full snapshot, a concurrent write moves the heartbeat past the
        // client's last_full_snapshot_revision.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let bootstrap = try await client.getSyncBootstrap().ok.body.json

        await server.advance(itemID: "item-spec_approval")

        let heartbeat = try await client.getSyncRevision().ok.body.json
        #expect(heartbeat.sync_epoch == bootstrap.sync_epoch)
        #expect(heartbeat.revision > bootstrap.revision)
    }
}
