import Foundation
import FreesideAPI
import FreesideTestSupport
import Security
import Testing

@testable import FreesideCore
@testable import FreesideModel

private func temporaryStore() -> (DiskCacheStore, URL) {
    let directory = FileManager.default.temporaryDirectory
        .appendingPathComponent("freeside-cache-tests-\(UUID().uuidString)")
    return (DiskCacheStore(directory: directory), directory)
}

private func sampleState(revision: Int64 = 5) -> CachedState {
    CachedState(
        cursors: SyncCursors(
            syncEpoch: "epoch-1",
            lastFullSnapshotRevision: revision,
            highestObservedServerRevision: revision
        ),
        attentionItems: [AttentionFixtures.fixture(type: .spec_approval)],
        conversations: AttentionFixtures.defaultConversations(),
        tasks: TaskFixtures.defaultTasks(),
        taskTimelines: TaskFixtures.defaultTimelines()
    )
}

@Suite struct DiskCacheStoreTests {
    @Test func roundTripsTheCachedState() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }

        #expect(store.load() == nil)
        let state = sampleState()
        try store.save(state)
        #expect(store.load() == state)

        // A later save replaces the earlier state wholesale, as a
        // bootstrap rebuild does.
        let newer = sampleState(revision: 9)
        try store.save(newer)
        #expect(store.load() == newer)
    }

    @Test func preCreationTimestampCacheDecodesAsLegacyNil() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        try store.save(sampleState())

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var state = try #require(object["state"] as? [String: Any])
        var snapshots = try #require(state["attentionItems"] as? [[String: Any]])
        var snapshot = snapshots[0]
        var item = try #require(snapshot["item"] as? [String: Any])
        item.removeValue(forKey: "created_at")
        snapshot["item"] = item
        snapshots[0] = snapshot
        state["attentionItems"] = snapshots
        object["state"] = state
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let legacy = try #require(store.load())
        #expect(legacy.attentionItems[0].item.created_at == nil)
    }

    @Test func anythingUnreadableLoadsAsAbsent() throws {
        // The cache is disposable by design: corruption, a foreign
        // format, or a future incompatible version all mean "bootstrap",
        // never a decode error surfaced to the user.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        try FileManager.default.createDirectory(
            at: directory, withIntermediateDirectories: true)
        let file = directory.appendingPathComponent("cache.json")

        try Data("not json {".utf8).write(to: file)
        #expect(store.load() == nil)

        try Data(#"{"format": 999, "state": {}}"#.utf8).write(to: file)
        #expect(store.load() == nil)

        // A pre-ledger format-1 file is one such foreign format: it
        // loads as absent (one bootstrap; a pre-upgrade unresolved
        // ledger did not exist to lose).
        try Data(#"{"format": 1, "state": {}}"#.utf8).write(to: file)
        #expect(store.load() == nil)
    }

    @Test func theUnattendedOperationStateRoundTrips() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.unattendedOperation = .init(
            admission: .stopped,
            stops: [
                .init(
                    kind: .operator_stop, item_id: "notice", command_id: "cmd-stop",
                    since: Date(timeIntervalSince1970: 1_786_502_645)),
                .init(kind: .blocking_system_health, item_id: "finding", command_id: nil, since: nil),
            ])
        try store.save(state)

        #expect(store.load()?.unattendedOperation == state.unattendedOperation)
    }

    @Test func theAgentCatalogRoundTrips() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.agentCatalog = UsageFixtures.defaultAgentCatalog()
        try store.save(state)

        #expect(store.load()?.agentCatalog == UsageFixtures.defaultAgentCatalog())
    }

    @Test func aCacheWithoutTheAgentCatalogLoadsItsRowsAndNoCatalog() throws {
        // The catalog is additive: an older cache keeps its cursors and rows,
        // and the next heartbeat's lineup revision differs from the absent
        // catalog's, which triggers the bootstrap that fills it.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        let state = sampleState()
        try store.save(state)

        // A state with no catalog writes no key, the shape an older cache has.
        let file = directory.appendingPathComponent("cache.json")
        let object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        let written = try #require(object["state"] as? [String: Any])
        #expect(written["agentCatalog"] == nil)

        let loaded = try #require(store.load())
        #expect(loaded.agentCatalog == nil)
        #expect(loaded.cursors == state.cursors)
        #expect(loaded.attentionItems == state.attentionItems)
    }

    @Test func aCatalogStillCarryingAgentsIsDroppedAndTheRestLoads() throws {
        // Before #2599 every catalog carried `agents`, which the strict
        // decoder now rejects. The load path drops only the catalog, so the
        // ledger, rows, and cursors survive, and the next heartbeat's lineup
        // revision triggers the bootstrap that restores the catalog.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.agentCatalog = UsageFixtures.defaultAgentCatalog()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        var catalog = try #require(legacyState["agentCatalog"] as? [String: Any])
        catalog["agents"] = [["name": "claude-primary", "enrollment": "enrollment-claude-a-mac"]]
        legacyState["agentCatalog"] = catalog
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.agentCatalog == nil)
        #expect(migrated.cursors == state.cursors)
        #expect(migrated.attentionItems == state.attentionItems)
        #expect(migrated.pendingCommands == state.pendingCommands)
    }

    /// The task page's pending line change (#2651) is additive: it round-trips,
    /// and a cache without it, the shape every earlier cache has, loads its
    /// rows and no change.
    @Test func aPendingLineChangeRoundTripsAndAnOlderCacheLoadsWithoutOne() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        let older = sampleState()
        try store.save(older)
        let file = directory.appendingPathComponent("cache.json")
        let object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        let written = try #require(object["state"] as? [String: Any])
        #expect(written["pendingTaskLineChange"] == nil)
        let loadedOlder = try #require(store.load())
        #expect(loadedOlder.pendingTaskLineChange == nil)
        #expect(loadedOlder.cursors == older.cursors)

        var state = sampleState()
        state.pendingTaskLineChange = PendingTaskLineChange(
            command: .init(
                command_id: "line-1", device_id: "device-1",
                payload: .change_task_line(
                    .init(
                        kind: .change_task_line, task_id: "task-1", role: .reviewer,
                        selection: .init(value1: UsageFixtures.agentSelection("codex-primary"))))),
            syncEpoch: "epoch-1")
        state.lineChangeDaemonID = "mock"
        try store.save(state)
        let loaded = try #require(store.load())
        #expect(loaded.pendingTaskLineChange == state.pendingTaskLineChange)
        #expect(loaded.lineChangeDaemonID == "mock")
    }

    @Test func roundTripsTheUsagePools() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.usagePools = UsageFixtures.defaultUsagePools()
        try store.save(state)
        #expect(store.load()?.usagePools == UsageFixtures.defaultUsagePools())
    }

    @Test func aPreUsageFormatSevenCacheForcesABootstrapAndKeepsItsRoutingChange() throws {
        // Format 7 predates the usage pools. Keeping its cursors would let an
        // unchanged server revision skip the bootstrap that supplies them,
        // so the pools would read as none. An unsettled routing change
        // survives, as the other saved commands do.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        state.pendingRoutingPolicyChange = PendingRoutingPolicyChange(
            command: .init(
                command_id: "routing-1", device_id: "device-1",
                payload: .set_routing_policy(
                    .init(
                        kind: .set_routing_policy,
                        roles: [.init(role: .implementer, enrollments: [UsageFixtures.unsupportedCollectorID])],
                        expected_revision: 2))),
            syncEpoch: "epoch-1")
        state.routingDaemonID = "daemon-1"
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        legacyState.removeValue(forKey: "usagePools")
        object["format"] = 7
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.attentionItems.isEmpty)
        #expect(migrated.usagePools.isEmpty)
        #expect(migrated.pendingCommands == state.pendingCommands)
        #expect(migrated.pendingRoutingPolicyChange == state.pendingRoutingPolicyChange)
        #expect(migrated.routingDaemonID == "daemon-1")
    }

    @Test func aPreRoutingFormatSixCacheForcesABootstrapAndKeepsItsCommands() throws {
        // Format 6 predates the routing policy. Keeping its cursors would let
        // an unchanged server revision skip the bootstrap that supplies the
        // policy, so a set policy would read as none. Unsettled stops survive.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        state.stopDaemonID = "daemon-1"
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        object["format"] = 6
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.attentionItems.isEmpty)
        #expect(migrated.routingPolicy == nil)
        #expect(migrated.pendingCommands == state.pendingCommands)
        #expect(migrated.stopDaemonID == "daemon-1")
    }

    @Test func aPreOperatingStateFormatFiveCacheForcesABootstrap() throws {
        // Format 5 predates the unattended-admission state. Keeping its
        // cursors would let an unchanged server revision skip the bootstrap
        // that supplies the state, so a stopped daemon would show no
        // indicator. The unsettled client commands still survive.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        let submission = Components.Schemas.ClientCommand(
            command_id: "submission-1", device_id: "device-1",
            payload: .submit_task(
                .init(kind: .submit_task, project_id: "project-1", source: "Saved work")))
        state.pendingTaskSubmissions = [submission.command_id: submission]
        state.submissionDaemonID = "daemon-1"
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        legacyState.removeValue(forKey: "unattendedOperation")
        object["format"] = 5
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.attentionItems.isEmpty)
        #expect(migrated.tasks.isEmpty)
        #expect(migrated.unattendedOperation == nil)
        #expect(migrated.pendingCommands == state.pendingCommands)
        #expect(migrated.pendingTaskSubmissions == state.pendingTaskSubmissions)
        #expect(migrated.submissionDaemonID == "daemon-1")
    }

    @Test func aPreConversationsFormatThreeCachePreservesOnlyTheCommandLedger() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        legacyState.removeValue(forKey: "conversations")
        object["format"] = 3
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.attentionItems.isEmpty)
        #expect(migrated.conversations.isEmpty)
        #expect(migrated.runs.isEmpty)
        #expect(migrated.schedules.isEmpty)
        #expect(migrated.runTimelines.isEmpty)
        #expect(migrated.pendingCommands == state.pendingCommands)
    }

    @Test func aLegacyLedgerWithoutPayloadKindDecodesAsDecision() throws {
        // Before the client-command payload became a kind-discriminated union
        // (submit_task), a persisted decision command encoded a bare, kind-less
        // payload. The generated decoder now requires the discriminator, so the
        // load path injects it (defaulting to decision) rather than dropping the
        // whole ledger and its retryable command IDs (plan §5.14 sync test 4).
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var persistedState = try #require(object["state"] as? [String: Any])
        var pending = try #require(persistedState["pendingCommands"] as? [String: Any])
        var entry = try #require(pending["item-a"] as? [String: Any])
        var command = try #require(entry["command"] as? [String: Any])
        var payload = try #require(command["payload"] as? [String: Any])
        payload.removeValue(forKey: "kind")
        command["payload"] = payload
        entry["command"] = command
        pending["item-a"] = entry
        persistedState["pendingCommands"] = pending
        object["state"] = persistedState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.pendingCommands == state.pendingCommands)
        guard case .decision(let decoded)? = migrated.pendingCommands?["item-a"]?.command.payload
        else {
            Issue.record("expected a decision command")
            return
        }
        #expect(decoded.kind == .decision)
        #expect(decoded.item_id == "item-a")
    }

    @Test func aLegacyRunProposalSnapshotDecodesAsTaskProposal() throws {
        // The run_proposal -> task_proposal attention-vocabulary rename (#1210)
        // changed a persisted AttentionType value. AttentionType decodes
        // strictly, so a format-5 cache written before the rename would throw
        // on decode and discard the whole cache, including the retryable
        // command ledger. The load path translates the stored type in place.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.attentionItems = [AttentionFixtures.fixture(type: .task_proposal)]
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var persistedState = try #require(object["state"] as? [String: Any])
        var snapshots = try #require(persistedState["attentionItems"] as? [[String: Any]])
        var item = try #require(snapshots[0]["item"] as? [String: Any])
        item["type"] = "run_proposal"
        snapshots[0]["item"] = item
        persistedState["attentionItems"] = snapshots
        object["state"] = persistedState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.attentionItems.count == 1)
        #expect(migrated.attentionItems[0].item._type == .task_proposal)
        #expect(migrated.pendingCommands == state.pendingCommands)
    }

    @Test func aLegacyRunProposalRevisionKeyReloadsAsTaskProposalRevision() throws {
        // The same rename changed the run_proposal_revision decision-payload
        // key. The generated decoder ignores the unknown old key rather than
        // rejecting it, so a committed start_with_changes command persisted
        // before the rename would silently reload without its revision and fail
        // its verbatim retry. The load path renames the key in place.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var command = makeCommand(itemID: "item-a")
        command.payload.asDecision.action = .start_with_changes
        command.payload.asDecision.task_proposal_revision = .init(
            value1: .init(
                intent: .implement_subject, expected_cost_units: 25,
                scope: .init(
                    component_count: 2, declared_path_count: 3, touches_control_plane: true)))
        var state = sampleState()
        state.pendingCommands = ["item-a": .init(command: command, state: .unresolved)]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var persistedState = try #require(object["state"] as? [String: Any])
        var pending = try #require(persistedState["pendingCommands"] as? [String: Any])
        var entry = try #require(pending["item-a"] as? [String: Any])
        var commandJSON = try #require(entry["command"] as? [String: Any])
        var payload = try #require(commandJSON["payload"] as? [String: Any])
        let revision = try #require(payload["task_proposal_revision"])
        payload.removeValue(forKey: "task_proposal_revision")
        payload["run_proposal_revision"] = revision
        commandJSON["payload"] = payload
        entry["command"] = commandJSON
        pending["item-a"] = entry
        persistedState["pendingCommands"] = pending
        object["state"] = persistedState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.pendingCommands == state.pendingCommands)
        guard case .decision(let decoded)? = migrated.pendingCommands?["item-a"]?.command.payload
        else {
            Issue.record("expected a decision command")
            return
        }
        #expect(decoded.action == .start_with_changes)
        #expect(decoded.task_proposal_revision?.value1.expected_cost_units == 25)
    }

    @Test func aCachedHoldUnderTheRetiredAttendedCheckLoadsWithoutItsDetail() throws {
        // Plan revision 97 removed the agent_attended_mark admission check
        // (#2265). AdmissionCheck decodes strictly, so a cache holding a run
        // hold refused under it would throw on decode and discard the whole
        // cache, ledger included. The load path drops that detail in run
        // timelines and in task timelines' run sections.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.runTimelines = RunFixtures.defaultTimelines()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        let retired: [String: Any] = [
            "check": "agent_attended_mark", "agent": "claude-code-specifier",
            "launch_stage": NSNull(), "launch_digest": NSNull(),
        ]
        func retire(_ hold: inout [String: Any]) {
            hold["reason"] = "admission_policy_refused"
            hold["detail"] = retired
        }
        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var persistedState = try #require(object["state"] as? [String: Any])
        var runTimelines = try #require(persistedState["runTimelines"] as? [[String: Any]])
        var retiredRunHolds = 0
        for index in runTimelines.indices {
            guard var hold = runTimelines[index]["hold"] as? [String: Any] else { continue }
            retire(&hold)
            runTimelines[index]["hold"] = hold
            retiredRunHolds += 1
        }
        persistedState["runTimelines"] = runTimelines
        var taskTimelines = try #require(persistedState["taskTimelines"] as? [[String: Any]])
        var retiredTaskHolds = 0
        for timelineIndex in taskTimelines.indices {
            guard var sections = taskTimelines[timelineIndex]["sections"] as? [[String: Any]] else { continue }
            for sectionIndex in sections.indices {
                guard var runs = sections[sectionIndex]["runs"] as? [[String: Any]] else { continue }
                for runIndex in runs.indices {
                    guard var hold = runs[runIndex]["hold"] as? [String: Any] else { continue }
                    retire(&hold)
                    runs[runIndex]["hold"] = hold
                    retiredTaskHolds += 1
                }
                sections[sectionIndex]["runs"] = runs
            }
            taskTimelines[timelineIndex]["sections"] = sections
        }
        persistedState["taskTimelines"] = taskTimelines
        #expect(retiredRunHolds > 0)
        #expect(retiredTaskHolds > 0)
        object["state"] = persistedState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.pendingCommands == state.pendingCommands)
        let runHolds = migrated.runTimelines.compactMap { $0.hold?.value1 }
        #expect(runHolds.count == retiredRunHolds)
        #expect(runHolds.allSatisfy { $0.reason == .admission_policy_refused && $0.detail == nil })
        let taskHolds = migrated.taskTimelines.flatMap { timeline in
            timeline.sections.flatMap { section in section.runs.compactMap { $0.hold?.value1 } }
        }
        #expect(taskHolds.count == retiredTaskHolds)
        #expect(taskHolds.allSatisfy { $0.reason == .admission_policy_refused && $0.detail == nil })
    }

    @Test func aCachedAdmissionHoldWithoutAnUnservedPartLoads() throws {
        // A cache written before #2992 holds admission details with no
        // unserved key; the generated decoder reads the missing key as nil,
        // so the cache loads without a format bump.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.runTimelines = RunFixtures.defaultTimelines()
        for index in state.runTimelines.indices where state.runTimelines[index].hold != nil {
            state.runTimelines[index].hold?.value1.reason = .admission_policy_refused
            state.runTimelines[index].hold?.value1.detail = .init(value1: .init(check: .agent_resolution))
        }
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var persistedState = try #require(object["state"] as? [String: Any])
        var runTimelines = try #require(persistedState["runTimelines"] as? [[String: Any]])
        var stripped = 0
        for index in runTimelines.indices {
            guard var hold = runTimelines[index]["hold"] as? [String: Any],
                var detail = hold["detail"] as? [String: Any]
            else { continue }
            detail.removeValue(forKey: "unserved")
            hold["detail"] = detail
            runTimelines[index]["hold"] = hold
            stripped += 1
        }
        #expect(stripped > 0)
        persistedState["runTimelines"] = runTimelines
        object["state"] = persistedState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let loaded = try #require(store.load())
        let holds = loaded.runTimelines.compactMap { $0.hold?.value1 }
        #expect(holds.count == stripped)
        #expect(holds.allSatisfy { $0.detail?.value1.check == .agent_resolution && $0.detail?.value1.unserved == nil })
    }

    @Test func anArtifactSourcedTaskRoundTripsItsSourceDigest() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        let baseline = try #require(state.tasks.first)
        state.tasks[0] = TaskFixtures.artifactSourced(baseline, artifactID: "artifact-1", digest: "sha256:prompt")
        try store.save(state)
        #expect(store.load()?.tasks == state.tasks)
    }

    @Test func aPreSourceDigestFormatEightCacheForcesABootstrapAndKeepsItsLedger() throws {
        // Format 8 predates the artifact-sourced task's required digest
        // (#2610). Its artifact-sourced task would throw on decode and
        // discard the whole cache, ledger included, so the load path drops
        // it; the format bump discards the remaining snapshots and cursors so
        // the bootstrap restores the task with its digest.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        let baseline = try #require(state.tasks.first)
        state.tasks[0] = TaskFixtures.artifactSourced(baseline, artifactID: "artifact-1", digest: "sha256:prompt")
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        var tasks = try #require(legacyState["tasks"] as? [[String: Any]])
        var task = try #require(tasks[0]["task"] as? [String: Any])
        var source = try #require(task["source"] as? [String: Any])
        #expect(source.removeValue(forKey: "work_item_artifact_digest") != nil)
        task["source"] = source
        tasks[0]["task"] = task
        legacyState["tasks"] = tasks
        object["format"] = 8
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.tasks.isEmpty)
        #expect(migrated.pendingCommands == state.pendingCommands)
    }

    @Test func aPreEnrollmentFormatElevenCacheDropsItsPolicyAndNamedRoutingChangeAndKeepsItsLedger() throws {
        // Format 11 holds a routing policy the interim wire rendered as
        // agent names (#2807). The format bump discards its snapshot and
        // cursor so the bootstrap restores the policy as the daemon stores
        // it, and an unsettled change that ordered names fails its section's
        // decode, since the daemon now refuses names, while the ledger stays.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.routingPolicy = .init(as_of_revision: 4, entity_version: 2, policy: UsageFixtures.defaultRoutingPolicy())
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        state.pendingRoutingPolicyChange = PendingRoutingPolicyChange(
            command: .init(
                command_id: "routing-1", device_id: "device-1",
                payload: .set_routing_policy(
                    .init(
                        kind: .set_routing_policy,
                        roles: [.init(role: .implementer, enrollments: [UsageFixtures.unsupportedCollectorID])],
                        expected_revision: 2))),
            syncEpoch: "epoch-1")
        state.routingDaemonID = "daemon-1"
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        // A format 11 snapshot rendered the policy's order as agent names,
        // which still decodes, so the rest of the file loads.
        var snapshot = try #require(legacyState["routingPolicy"] as? [String: Any])
        var cachedPolicy = try #require(snapshot["policy"] as? [String: Any])
        cachedPolicy["roles"] = [["role": "implementer", "agents": ["claude-primary", "codex-primary"]]]
        snapshot["policy"] = cachedPolicy
        legacyState["routingPolicy"] = snapshot
        // A format 11 change named its agents.
        var change = try #require(legacyState["pendingRoutingPolicyChange"] as? [String: Any])
        var command = try #require(change["command"] as? [String: Any])
        var payload = try #require(command["payload"] as? [String: Any])
        payload["roles"] = [["role": "implementer", "agents": ["codex-primary"]]]
        command["payload"] = payload
        change["command"] = command
        legacyState["pendingRoutingPolicyChange"] = change
        object["format"] = 11
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.routingPolicy == nil)
        #expect(migrated.pendingRoutingPolicyChange == nil)
        #expect(migrated.pendingCommands == state.pendingCommands)
    }

    @Test func aPreSelectionFormatNineCacheDropsItsCatalogAndOnlyItsNamedLines() throws {
        // Format 9 predates the catalog's enrollments and a client's selection
        // lines (#2598). Its catalog, and a saved decision or submission whose
        // lines name an agent, would throw on decode: the catalog would
        // discard the whole cache, a named line its whole section. The load
        // path drops the catalog and only the entries with named lines; the
        // format bump discards the snapshots and cursors so the bootstrap
        // restores the catalog.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.agentCatalog = UsageFixtures.defaultAgentCatalog()
        func lineChange(_ id: String) -> Components.Schemas.ClientCommand {
            .init(
                command_id: id, device_id: "device-1",
                payload: .change_task_line(
                    .init(
                        kind: .change_task_line, task_id: "task-1", role: .implementer,
                        selection: .init(value1: UsageFixtures.agentSelection("claude-primary")))))
        }
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved),
            "item-b": .init(command: makeCommand(itemID: "item-b"), state: .unresolved),
            "line-set": .init(command: lineChange("line-set"), state: .unresolved),
            "line-clear": .init(command: lineChange("line-clear"), state: .unresolved),
        ]
        func submission(_ id: String) -> Components.Schemas.ClientCommand {
            .init(
                command_id: id, device_id: "device-1",
                payload: .submit_task(.init(kind: .submit_task, project_id: "project-1", source: "Saved work")))
        }
        state.pendingTaskSubmissions = ["submission-1": submission("submission-1"), "named": submission("named")]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        // A format 9 catalog: no enrollments, and names where selections are.
        var catalog = try #require(legacyState["agentCatalog"] as? [String: Any])
        #expect(catalog.removeValue(forKey: "enrollments") != nil)
        legacyState["agentCatalog"] = catalog
        let namedLines: [[String: Any]] = [["role": "implementer", "agent": "claude-old"]]
        var ledger = try #require(legacyState["pendingCommands"] as? [String: Any])
        var entry = try #require(ledger["item-b"] as? [String: Any])
        var command = try #require(entry["command"] as? [String: Any])
        var payload = try #require(command["payload"] as? [String: Any])
        payload["task_lines"] = namedLines
        command["payload"] = payload
        entry["command"] = command
        ledger["item-b"] = entry
        // A format 9 line change named its agent, or omitted it to clear.
        for (key, agent) in [("line-set", "claude-old" as String?), ("line-clear", nil)] {
            var entry = try #require(ledger[key] as? [String: Any])
            var command = try #require(entry["command"] as? [String: Any])
            var payload = try #require(command["payload"] as? [String: Any])
            payload.removeValue(forKey: "selection")
            if let agent { payload["agent"] = agent }
            command["payload"] = payload
            entry["command"] = command
            ledger[key] = entry
        }
        legacyState["pendingCommands"] = ledger
        var submissions = try #require(legacyState["pendingTaskSubmissions"] as? [String: Any])
        var named = try #require(submissions["named"] as? [String: Any])
        var namedPayload = try #require(named["payload"] as? [String: Any])
        namedPayload["task_lines"] = namedLines
        named["payload"] = namedPayload
        submissions["named"] = named
        legacyState["pendingTaskSubmissions"] = submissions
        object["format"] = 9
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.agentCatalog == nil)
        #expect(migrated.pendingCommands?.keys.sorted() == ["item-a", "line-clear"])
        #expect(migrated.pendingCommands?["item-a"] == state.pendingCommands?["item-a"])
        guard case .change_task_line(let clear) = migrated.pendingCommands?["line-clear"]?.command.payload else {
            Issue.record("the saved clear did not survive as a line change")
            return
        }
        #expect(clear.selection == nil)
        #expect(migrated.pendingTaskSubmissions == ["submission-1": submission("submission-1")])
    }

    @Test func aPreEnrollmentOfferFormatTenCacheDropsOnlyItsAgentChoices() throws {
        // Format 10 predates offers and choices by enrollment (#2808). A
        // cached offer naming an agent would throw on decode and discard the
        // cache, ledger included, and a saved choice naming an agent its
        // section. The load path nulls such offers and drops only the
        // entries naming an agent; the format bump discards the snapshots and
        // cursors so the bootstrap restores the offers.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.attentionItems = [AttentionFixtures.quotaFailure()]
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved),
            "item-b": .init(command: makeCommand(itemID: "item-b"), state: .unresolved),
        ]
        state.pendingParkedTaskMove = .init(
            command: .init(
                command_id: "move-1", device_id: "device-1",
                payload: .move_parked_task(
                    .init(
                        kind: .move_parked_task, task_id: "task-1",
                        choice: .init(enrollment_id: UsageFixtures.readingCollectorID)))),
            syncEpoch: "epoch-1", receipt: nil)
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        // A format 10 offer named its agent, and a choice was the agent.
        var items = try #require(legacyState["attentionItems"] as? [[String: Any]])
        var item = try #require(items.first?["item"] as? [String: Any])
        var offers = try #require(item["alternate_agents"] as? [[String: Any]])
        offers[0]["agent"] = "claude-team"
        offers[0].removeValue(forKey: "enrollment")
        item["alternate_agents"] = offers
        items[0]["item"] = item
        legacyState["attentionItems"] = items
        var ledger = try #require(legacyState["pendingCommands"] as? [String: Any])
        var entry = try #require(ledger["item-b"] as? [String: Any])
        var command = try #require(entry["command"] as? [String: Any])
        var payload = try #require(command["payload"] as? [String: Any])
        payload["action"] = "retry_with_alternate_agent"
        payload["alternate_agent"] = "claude-team"
        command["payload"] = payload
        entry["command"] = command
        ledger["item-b"] = entry
        legacyState["pendingCommands"] = ledger
        var move = try #require(legacyState["pendingParkedTaskMove"] as? [String: Any])
        var moveCommand = try #require(move["command"] as? [String: Any])
        var movePayload = try #require(moveCommand["payload"] as? [String: Any])
        movePayload.removeValue(forKey: "choice")
        movePayload["agent"] = "claude-team"
        moveCommand["payload"] = movePayload
        move["command"] = moveCommand
        legacyState["pendingParkedTaskMove"] = move
        object["format"] = 10
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.attentionItems.isEmpty)
        #expect(migrated.pendingCommands?.keys.sorted() == ["item-a"])
        #expect(migrated.pendingCommands?["item-a"] == state.pendingCommands?["item-a"])
        #expect(migrated.pendingParkedTaskMove == nil)
    }

    @Test func aCachedBaseAdvanceWatchIsDroppedAndTheLedgerKept() throws {
        // Plan revision 110 retired the base_advance_watch kind (#2645).
        // A cached schedule of that kind would throw on decode and discard
        // the whole cache, ledger included, so the load path drops only that
        // schedule.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.schedules = RunFixtures.defaultSchedules()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var persistedState = try #require(object["state"] as? [String: Any])
        var schedules = try #require(persistedState["schedules"] as? [[String: Any]])
        var retired = schedules[0]
        var schedule = try #require(retired["schedule"] as? [String: Any])
        schedule["id"] = "schedule-base-654"
        schedule["kind"] = "base_advance_watch"
        schedule["base_watch"] = [
            "repo": "freeside-ai/freeside", "base_ref": "main",
            "admitted_base_sha": String(repeating: "a", count: 40),
        ]
        retired["schedule"] = schedule
        schedules.append(retired)
        persistedState["schedules"] = schedules
        object["state"] = persistedState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.schedules == state.schedules)
        #expect(migrated.cursors == state.cursors)
        #expect(migrated.pendingCommands == state.pendingCommands)
    }

    @Test func aCachedRunTimelineWithoutAdmissionsIsDroppedAndTheLedgerKept() throws {
        // Run timelines gained the required admissions list (#979). A cached
        // timeline without it would throw on decode and discard the whole
        // cache, ledger included. The load path drops only that timeline; an
        // empty list would claim nothing was admitted.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.runTimelines = RunFixtures.defaultTimelines()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var persistedState = try #require(object["state"] as? [String: Any])
        var runTimelines = try #require(persistedState["runTimelines"] as? [[String: Any]])
        #expect(runTimelines.count > 1)
        let legacyRunID = try #require(runTimelines[0]["run_id"] as? String)
        runTimelines[0].removeValue(forKey: "admissions")
        persistedState["runTimelines"] = runTimelines
        object["state"] = persistedState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.pendingCommands == state.pendingCommands)
        #expect(migrated.runTimelines == state.runTimelines.filter { $0.run_id != legacyRunID })
    }

    @Test func aPreTasksFormatFourCacheKeepsTheLedgerAndTelemetrySections() throws {
        // Format 4 predates task snapshots and task timelines. Its cursors
        // must not make an upgraded client consider an empty task list
        // current; the ledger and the telemetry sections are epoch-independent
        // client state and survive, so a queued event drains rather than
        // vanishing with the upgrade.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        state.comprehensionQueue = [
            .init(
                eventID: "event-1",
                input: .init(
                    item_id: "item-a", kind: .card_opened,
                    item_decision_surface_digest: "sha256:" + String(repeating: "a", count: 64),
                    occurred_at: Date(timeIntervalSince1970: 1_700_000_000), sequence: 1))
        ]
        state.comprehensionSequence = 1
        state.registeredCapabilityFingerprint = "fingerprint-1"
        state.comprehensionDeviceID = "device-1"
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        legacyState.removeValue(forKey: "tasks")
        legacyState.removeValue(forKey: "taskTimelines")
        object["format"] = 4
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.attentionItems.isEmpty)
        #expect(migrated.conversations.isEmpty)
        #expect(migrated.runs.isEmpty)
        #expect(migrated.runTimelines.isEmpty)
        #expect(migrated.tasks.isEmpty)
        #expect(migrated.taskTimelines.isEmpty)
        #expect(migrated.pendingCommands == state.pendingCommands)
        #expect(migrated.comprehensionQueue == state.comprehensionQueue)
        #expect(migrated.comprehensionSequence == 1)
        #expect(migrated.registeredCapabilityFingerprint == "fingerprint-1")
        #expect(migrated.comprehensionDeviceID == "device-1")
    }

    @Test func aCacheFromAnotherContractKeepsOnlyTheCommandLedger() throws {
        // Responses tolerate additive change (#1266), so a snapshot written
        // under an older contract still decodes, but lacks what the newer
        // contract added. Its cursor would skip the bootstrap that supplies
        // it, so the load keeps only the epoch-independent state.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        state.comprehensionQueue = [
            .init(
                eventID: "event-1",
                input: .init(
                    item_id: "item-a", kind: .card_opened,
                    item_decision_surface_digest: "sha256:" + String(repeating: "a", count: 64),
                    occurred_at: Date(timeIntervalSince1970: 1_700_000_000), sequence: 1))
        ]
        state.comprehensionSequence = 1
        try store.save(state)
        #expect(try #require(store.load()).cursors == state.cursors)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        #expect(object["contract"] as? String == APIContract.digest)
        object["contract"] = "sha256:an-older-contract"
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.attentionItems.isEmpty)
        #expect(migrated.tasks.isEmpty)
        #expect(migrated.pendingCommands == state.pendingCommands)
        #expect(migrated.comprehensionQueue == state.comprehensionQueue)
        #expect(migrated.comprehensionSequence == 1)

        // A snapshot written before this build's contract required a field
        // lacks it, and the ledger still survives: the snapshots of a file
        // from another contract are never decoded.
        var cached = try #require(object["state"] as? [String: Any])
        var items = try #require(cached["attentionItems"] as? [[String: Any]])
        #expect(!items.isEmpty)
        items[0].removeValue(forKey: "item")
        cached["attentionItems"] = items
        object["state"] = cached
        try JSONSerialization.data(withJSONObject: object).write(to: file)
        let reshaped = try #require(store.load())
        #expect(reshaped.attentionItems.isEmpty)
        #expect(reshaped.pendingCommands == state.pendingCommands)
        #expect(reshaped.comprehensionQueue == state.comprehensionQueue)

        object.removeValue(forKey: "contract")
        try JSONSerialization.data(withJSONObject: object).write(to: file)
        let unmarked = try #require(store.load())
        #expect(unmarked.cursors == nil)
        #expect(unmarked.pendingCommands == state.pendingCommands)
    }

    @Test func aPreRunsFormatTwoCachePreservesOnlyTheCommandLedger() throws {
        // Format 2 predates runs and schedules.  Its valid cached cursors
        // must not make an upgraded client consider empty default arrays
        // current, or completed durable runs would stay invisible until a
        // later server revision happens to force a bootstrap.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        legacyState.removeValue(forKey: "runs")
        legacyState.removeValue(forKey: "schedules")
        legacyState.removeValue(forKey: "runTimelines")
        object["format"] = 2
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.attentionItems.isEmpty)
        #expect(migrated.runs.isEmpty)
        #expect(migrated.schedules.isEmpty)
        #expect(migrated.runTimelines.isEmpty)
        #expect(migrated.pendingCommands == state.pendingCommands)
    }

    @Test func roundTripsThePendingCommandLedger() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }

        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .inFlight),
            "item-b": .init(
                command: makeCommand(itemID: "item-b", commandID: "cmd-b"),
                state: .unresolved),
        ]
        try store.save(state)
        #expect(store.load() == state)
    }

    @Test func aLedgerOnlyStateRoundTrips() throws {
        // The post-epoch-discard shape: cursors and rows are dead while
        // an unresolved command still needs its verbatim resend (#115).
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }

        let state = CachedState(
            cursors: nil,
            attentionItems: [],
            pendingCommands: [
                "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
            ])
        try store.save(state)
        #expect(store.load() == state)
    }

    @Test func aCorruptLedgerSectionLoadsAsAbsentWithoutDroppingTheRest() throws {
        // The ledger degrades independently: garbling only the
        // pendingCommands section costs the retry affordance, never the
        // cursors and rows saved beside it.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        let file = directory.appendingPathComponent("cache.json")

        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var inner = try #require(object["state"] as? [String: Any])
        inner["pendingCommands"] = ["item-a": 42]
        object["state"] = inner
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let loaded = try #require(store.load())
        #expect(loaded.pendingCommands == nil)
        #expect(loaded.cursors == state.cursors)
        #expect(loaded.attentionItems == state.attentionItems)
    }

    @Test @MainActor func thePersistedLedgerCarriesNoCredentialMaterial() async throws {
        // #115 acceptance 3: the ledger persists whole ClientCommands,
        // so prove at the byte level that a command minted through the
        // real paired, bearer-authenticated submit path writes no token
        // material to disk — the credential's only sink stays the
        // per-request Authorization header.
        let (cache, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }

        let server = MockServer(authMode: .enforcing)
        await server.seedPairingCode("483911")
        let grant = try await APIClientFactory.mock(server: server).pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Ben's iPhone"))
        ).created.body.json
        guard case .active(let active) = grant.device.device else {
            Issue.record("expected an active device")
            return
        }
        let subscription = try #require(
            DeviceNtfySubscription(
                serverURL: grant.ntfy_subscription.server_url,
                topic: grant.ntfy_subscription.topic))
        let credential = try #require(
            DeviceCredential(
                deviceID: active.id,
                token: grant.device_token,
                ntfySubscription: subscription))
        let client = APIClientFactory.mock(server: server) { credential.token }
        let coordinator = SyncCoordinator(
            client: client, device: DeviceIdentity(deviceID: active.id), cache: cache)
        await coordinator.bootstrap()

        // Lose the response after the mock records it, so the ledger
        // holds the submitted command when it persists.
        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { throw InjectedFailure() }
        }
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        #expect(
            coordinator.store.pendingCommandsByItemID["item-spec_approval"]?.state
                == .unresolved)

        let data = try Data(contentsOf: directory.appendingPathComponent("cache.json"))
        let text = try #require(String(data: data, encoding: .utf8)).lowercased()
        #expect(text.contains("pendingcommands"))
        #expect(!text.contains("authorization"))
        #expect(!text.contains("bearer"))
        #expect(!text.contains(credential.token.lowercased()))
        #expect(!text.contains(credential.ntfySubscription.topic.lowercased()))
        // The token scheme prefix and the token's base64 form: no
        // token-shaped fragment reaches disk.
        #expect(!text.contains("fsd1"))
        #expect(
            !text.contains(
                Data(credential.token.utf8).base64EncodedString().lowercased()))
        #expect(
            !text.contains(
                Data(credential.ntfySubscription.topic.utf8).base64EncodedString().lowercased()))
    }

    @Test func discardDeletesTheFile() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        try store.save(sampleState())
        #expect(store.load() != nil)

        store.discard()

        #expect(store.load() == nil)
        #expect(
            !FileManager.default.fileExists(
                atPath: directory.appendingPathComponent("cache.json").path))
    }
}

private enum FakeKeychainBackend: Hashable {
    case dataProtection
    case legacy
}

private struct FakeKeychainCall: Equatable {
    enum Kind: Equatable {
        case copy
        case add
        case delete
    }

    let kind: Kind
    let backend: FakeKeychainBackend
    let service: String
}

private final class FakeKeychain: @unchecked Sendable {
    var items: [FakeKeychainBackend: [String: Any]] = [:]
    var calls: [FakeKeychainCall] = []
    var addedAttributes: [[String: Any]] = []
    var copyOverrides: [FakeKeychainBackend: [(OSStatus, Any?)]] = [:]
    var addStatuses: [OSStatus] = []
    var deleteStatuses: [FakeKeychainBackend: [OSStatus]] = [:]

    var operations: KeychainSecurityOperations {
        KeychainSecurityOperations(
            copyMatching: { [self] query in
                // Mirrors SecItemMergeResults: the Data Protection result wins
                // when it succeeds, otherwise the file-based result stands.
                var merged: (OSStatus, Any?) = (errSecItemNotFound, nil)
                for backend in targets(for: query) {
                    calls.append(call(.copy, backend: backend, query: query))
                    let result: (OSStatus, Any?)
                    if var overrides = copyOverrides[backend], !overrides.isEmpty {
                        result = overrides.removeFirst()
                        copyOverrides[backend] = overrides
                    } else if let item = items[backend] {
                        result = (errSecSuccess, item)
                    } else {
                        result = (errSecItemNotFound, nil)
                    }
                    if result.0 == errSecSuccess { return result }
                    if result.0 != errSecItemNotFound { merged = result }
                }
                return merged
            },
            add: { [self] attributes in
                // SecItemAdd never targets both: only an explicit `true` adds
                // to the Data Protection Keychain.
                let backend: FakeKeychainBackend =
                    attributes[kSecUseDataProtectionKeychain as String] as? Bool == true
                    ? .dataProtection : .legacy
                calls.append(call(.add, backend: backend, query: attributes))
                addedAttributes.append(attributes)
                if !addStatuses.isEmpty {
                    let status = addStatuses.removeFirst()
                    if status != errSecSuccess { return status }
                }
                if items[backend] != nil { return errSecDuplicateItem }
                items[backend] = [
                    kSecAttrAccount as String: attributes[kSecAttrAccount as String] as Any,
                    kSecValueData as String: attributes[kSecValueData as String] as Any,
                ]
                return errSecSuccess
            },
            delete: { [self] query in
                // A delete runs against every targeted backend; one success is
                // a success, otherwise the first real error is reported.
                var merged = errSecItemNotFound
                for backend in targets(for: query) {
                    calls.append(call(.delete, backend: backend, query: query))
                    var status = errSecSuccess
                    if var statuses = deleteStatuses[backend], !statuses.isEmpty {
                        status = statuses.removeFirst()
                        deleteStatuses[backend] = statuses
                    }
                    if status == errSecSuccess || status == errSecItemNotFound {
                        status =
                            items.removeValue(forKey: backend) != nil
                            ? errSecSuccess : errSecItemNotFound
                    }
                    if status == errSecSuccess {
                        merged = errSecSuccess
                    } else if status != errSecItemNotFound && merged != errSecSuccess {
                        merged = status
                    }
                }
                return merged
            })
    }

    // Mirrors SecItemCategorizeQuery on macOS: an explicit
    // kSecUseDataProtectionKeychain selects exactly one backend, and a query
    // that omits it targets both. The store must never issue the latter (a
    // flag-less delete removed the live credential, #997).
    private func targets(for query: [String: Any]) -> [FakeKeychainBackend] {
        switch query[kSecUseDataProtectionKeychain as String] as? Bool {
        case true?: return [.dataProtection]
        case false?: return [.legacy]
        case nil: return [.dataProtection, .legacy]
        }
    }

    private func call(
        _ kind: FakeKeychainCall.Kind,
        backend: FakeKeychainBackend,
        query: [String: Any]
    ) -> FakeKeychainCall {
        FakeKeychainCall(
            kind: kind,
            backend: backend,
            service: query[kSecAttrService as String] as? String ?? "")
    }
}

private func credential(
    deviceID: String = "device-1",
    secretByte: UInt8 = 1,
    serverURL: String = "https://ntfy.example",
    topic: String = "fs-00000000000000000000000000000000"
) throws -> DeviceCredential {
    let subscription = try #require(
        DeviceNtfySubscription(serverURL: serverURL, topic: topic))
    return try #require(
        DeviceCredential(
            deviceID: deviceID,
            token: testDeviceToken(for: deviceID, secretByte: secretByte),
            ntfySubscription: subscription))
}

private func storedItem(_ credential: DeviceCredential) throws -> [String: Any] {
    let data = try JSONSerialization.data(withJSONObject: [
        "formatVersion": 1,
        "deviceID": credential.deviceID,
        "token": credential.token,
        "ntfyServerURL": credential.ntfySubscription.serverURL,
        "ntfyTopic": credential.ntfySubscription.topic,
    ])
    return [
        kSecAttrAccount as String: credential.deviceID,
        kSecValueData as String: data,
    ]
}

@Suite(.serialized) struct CredentialStoreTests {
    @Test func inMemoryStoreRoundTrips() throws {
        let store = InMemoryCredentialStore()
        #expect(try store.load() == nil)

        let credential = DeviceCredential(
            deviceID: "device-1", token: testDeviceToken(for: "device-1"),
            ntfySubscription: .mock)!
        try store.save(credential)
        #expect(try store.load() == credential)

        try store.delete()
        #expect(try store.load() == nil)
    }

    @Test func dataProtectionSaveAndLoadUseTheScopedBackend() throws {
        let fake = FakeKeychain()
        let store = KeychainCredentialStore(
            service: "ai.freeside.tests.deployment-a", operations: fake.operations)
        let value = try credential()

        try store.save(value)
        #expect(try store.load() == value)
        #expect(fake.items[.legacy] == nil)
        #expect(fake.items[.dataProtection] != nil)
        #expect(
            fake.calls == [
                FakeKeychainCall(
                    kind: .delete, backend: .legacy,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .delete, backend: .dataProtection,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .add, backend: .dataProtection,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .copy, backend: .dataProtection,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .copy, backend: .dataProtection,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .delete, backend: .legacy,
                    service: "ai.freeside.tests.deployment-a"),
            ])
        let added = try #require(fake.addedAttributes.first)
        #expect(added[kSecUseDataProtectionKeychain as String] as? Bool == true)
        #expect(
            added[kSecAttrAccessible as String] as? String
                == kSecAttrAccessibleAfterFirstUnlock as String)
    }

    // Regression for the #997 symptom: the legacy cleanup on a successful
    // read must target only the file-based Keychain. A flag-less delete also
    // reaches the Data Protection Keychain, so the first load after pairing
    // deleted the credential it had just returned.
    @Test func repeatedLoadsKeepTheAuthoritativeItem() throws {
        let fake = FakeKeychain()
        let value = try credential()
        fake.items[.dataProtection] = try storedItem(value)
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        #expect(try store.load() == value)
        #expect(try store.load() == value)
        #expect(fake.items[.dataProtection] != nil)
        #expect(fake.calls.map(\.kind) == [.copy, .delete, .copy, .delete])
        #expect(fake.calls.map(\.backend) == [.dataProtection, .legacy, .dataProtection, .legacy])
    }

    @Test func failedLegacyCleanupBeforeReplacementPreservesAuthoritativeItem() throws {
        let fake = FakeKeychain()
        let existing = try credential()
        let existingItem = try storedItem(existing)
        fake.items[.dataProtection] = existingItem
        fake.items[.legacy] = existingItem
        fake.deleteStatuses[.legacy] = [errSecInteractionNotAllowed]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        do {
            try store.save(try credential(deviceID: "device-2", secretByte: 2))
            Issue.record("expected legacy cleanup to fail")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecInteractionNotAllowed)
        }

        #expect(
            fake.items[.dataProtection]?[kSecValueData as String] as? Data
                == existingItem[kSecValueData as String] as? Data)
        #expect(fake.items[.legacy] != nil)
        #expect(fake.calls.map(\.kind) == [.delete])
        #expect(fake.calls.map(\.backend) == [.legacy])
    }

    @Test func platformWithoutDistinctLegacyBackendNeverConsultsIt() throws {
        let fake = FakeKeychain()
        let value = try credential()
        fake.items[.dataProtection] = try storedItem(value)
        let store = KeychainCredentialStore(
            service: "service",
            operations: fake.operations,
            legacyBackendEnabled: false)

        #expect(try store.load() == value)
        #expect(try store.load() == value)
        try store.delete()

        #expect(fake.items[.dataProtection] == nil)
        #expect(fake.calls.map(\.kind) == [.copy, .copy, .delete])
        #expect(fake.calls.allSatisfy { $0.backend == .dataProtection })
    }

    @Test func exactServiceLegacyItemMigratesCopyVerifyDelete() throws {
        let fake = FakeKeychain()
        let value = try credential()
        let legacy = try storedItem(value)
        fake.items[.legacy] = legacy
        let store = KeychainCredentialStore(
            service: "ai.freeside.tests.encoded%2Fpath", operations: fake.operations)

        #expect(try store.load() == value)
        #expect(fake.items[.legacy] == nil)
        #expect(fake.items[.dataProtection] != nil)
        #expect(fake.calls.map(\.kind) == [.copy, .copy, .add, .copy, .delete])
        #expect(
            fake.calls.map(\.backend) == [
                .dataProtection, .legacy, .dataProtection, .dataProtection, .legacy,
            ])
        #expect(fake.calls.allSatisfy { $0.service == "ai.freeside.tests.encoded%2Fpath" })
        #expect(
            fake.addedAttributes[0][kSecValueData as String] as? Data
                == legacy[kSecValueData as String] as? Data)
    }

    @Test func failedLegacyCopyLeavesTheLegacyItemUntouched() throws {
        let fake = FakeKeychain()
        fake.items[.legacy] = try storedItem(credential())
        fake.addStatuses = [errSecAuthFailed]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        #expect(throws: KeychainCredentialStore.KeychainError.self) {
            try store.load()
        }
        #expect(fake.items[.legacy] != nil)
        #expect(fake.items[.dataProtection] == nil)
        #expect(fake.calls.map(\.kind) == [.copy, .copy, .add])
    }

    @Test func failedLegacyVerificationLeavesTheLegacyItemUntouched() throws {
        let fake = FakeKeychain()
        let legacy = try credential()
        fake.items[.legacy] = try storedItem(legacy)
        fake.copyOverrides[.dataProtection] = [
            (errSecItemNotFound, nil),
            (errSecSuccess, try storedItem(credential(deviceID: "device-2", secretByte: 2))),
        ]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        do {
            _ = try store.load()
            Issue.record("expected read-back mismatch to fail")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecDecode)
        }
        #expect(fake.items[.legacy] != nil)
        #expect(fake.calls.map(\.kind) == [.copy, .copy, .add, .copy])
    }

    @Test func authoritativeCorruptionFailsClosedWithoutLegacyFallback() throws {
        let fake = FakeKeychain()
        fake.items[.dataProtection] = [
            kSecAttrAccount as String: "device-1",
            kSecValueData as String: Data("not-json".utf8),
        ]
        fake.items[.legacy] = try storedItem(credential())
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        do {
            _ = try store.load()
            Issue.record("expected corrupt authoritative item to fail")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecDecode)
        }
        #expect(fake.calls.map(\.kind) == [.copy])
        #expect(fake.calls.map(\.backend) == [.dataProtection])
        #expect(fake.items[.legacy] != nil)
    }

    @Test func authoritativeErrorFailsClosedWithoutLegacyFallback() throws {
        let fake = FakeKeychain()
        fake.items[.legacy] = try storedItem(credential())
        fake.copyOverrides[.dataProtection] = [(errSecInteractionNotAllowed, nil)]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        do {
            _ = try store.load()
            Issue.record("expected authoritative Keychain error to fail")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecInteractionNotAllowed)
        }
        #expect(fake.calls.map(\.kind) == [.copy])
        #expect(fake.calls.map(\.backend) == [.dataProtection])
    }

    @Test func authoritativeDuplicateWinsAndCleansLegacy() throws {
        let fake = FakeKeychain()
        let authoritative = try credential()
        let conflictingLegacy = try credential(
            deviceID: "device-2", secretByte: 2,
            serverURL: "https://other-ntfy.example",
            topic: "fs-11111111111111111111111111111111")
        fake.items[.dataProtection] = try storedItem(authoritative)
        fake.items[.legacy] = try storedItem(conflictingLegacy)
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        #expect(try store.load() == authoritative)
        #expect(fake.items[.legacy] == nil)
        #expect(fake.calls.map(\.kind) == [.copy, .delete])
        #expect(fake.calls.map(\.backend) == [.dataProtection, .legacy])
    }

    @Test func interruptedCleanupFailsLoudAndRetries() throws {
        let fake = FakeKeychain()
        let value = try credential()
        fake.items[.dataProtection] = try storedItem(value)
        fake.items[.legacy] = try storedItem(value)
        fake.deleteStatuses[.legacy] = [errSecInteractionNotAllowed, errSecSuccess]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        #expect(throws: KeychainCredentialStore.KeychainError.self) {
            try store.load()
        }
        #expect(fake.items[.legacy] != nil)
        #expect(try store.load() == value)
        #expect(fake.items[.legacy] == nil)
    }

    @Test func deleteClearsBothBackendsAndPreservesTheFirstError() throws {
        let fake = FakeKeychain()
        let value = try credential()
        fake.items[.dataProtection] = try storedItem(value)
        fake.items[.legacy] = try storedItem(value)
        fake.deleteStatuses[.dataProtection] = [errSecAuthFailed]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        do {
            try store.delete()
            Issue.record("expected the first backend error")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecAuthFailed)
        }
        #expect(fake.items[.dataProtection] != nil)
        #expect(fake.items[.legacy] == nil)
        #expect(fake.calls.map(\.kind) == [.delete, .delete])
        #expect(fake.calls.map(\.backend) == [.dataProtection, .legacy])

        fake.deleteStatuses[.dataProtection] = [errSecSuccess]
        try store.delete()
        #expect(fake.items.isEmpty)
    }
}
