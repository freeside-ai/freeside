import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// #2394: the routing policy reads from sync, and a change is a saved
/// command that reads as recorded only once sync shows its revision.
@Suite @MainActor struct RoutingPolicyModelTests {
    private func coordinator(
        _ server: MockServer = MockServer(), cache: any CacheStore = InMemoryCacheStore()
    ) async -> SyncCoordinator {
        let result = SyncCoordinator(
            client: APIClientFactory.mock(server: server), device: DeviceIdentity(deviceID: "device-mock"),
            cache: cache, submissionDaemonID: "mock")
        await result.refresh()
        return result
    }

    /// Begins an edit that puts claude-secondary first for the implementer.
    private func editImplementer(_ model: RoutingPolicyModel) {
        model.beginEditing()
        model.draft?.add("claude-secondary", to: .implementer)
        model.draft?.move("claude-secondary", in: .implementer, by: -1)
        model.draft?.move("claude-secondary", in: .implementer, by: -1)
    }

    private func revision(_ coordinator: SyncCoordinator) async throws -> Int64 {
        try await coordinator.store.client.getSyncRevision().ok.body.json.revision
    }

    // MARK: Reading

    @Test func syncAdoptsThePolicyAndARelaunchRestoresIt() async throws {
        let cache = InMemoryCacheStore()
        let first = await coordinator(cache: cache)
        #expect(first.routingPolicy?.policy == UsageFixtures.defaultRoutingPolicy())
        #expect(cache.load()?.routingPolicy?.policy == UsageFixtures.defaultRoutingPolicy())
        let restored = SyncCoordinator(client: first.store.client, cache: cache)
        #expect(restored.routingPolicy == first.routingPolicy)
    }

    @Test func aRoleThePolicyLeavesOutReadsAsItsLineupAgent() async throws {
        let coordinator = await coordinator()
        let model = RoutingPolicyModel(coordinator: coordinator)
        #expect(model.lineupAgent(for: .reviewer) == "codex-primary")
        #expect(RoutingPolicyView.lineupText(model.lineupAgent(for: .reviewer)) == "Lineup: codex-primary")
        #expect(RoutingPolicyView.lineupText(nil) == "Lineup")
    }

    @Test func setByNamesAPairedDeviceOrTheHost() {
        var policy = UsageFixtures.defaultRoutingPolicy()
        let names = [DeviceFixtures.otherDeviceID: "Ben's iPhone"]
        #expect(RoutingPolicyView.setBy(policy, deviceNames: names) == "Ben's iPhone")
        #expect(RoutingPolicyView.setBy(policy, deviceNames: [:]) == policy.set_by)
        policy.source = .cli_routing_set
        policy.set_by = "operator"
        #expect(RoutingPolicyView.setBy(policy, deviceNames: names) == "operator on the daemon host")
    }

    @Test func theSettingsRowSummarizesThePolicy() async throws {
        let withPolicy = await coordinator()
        #expect(SettingsSheet.routingSummary(RoutingPolicyModel(coordinator: withPolicy)) == "Revision 2 · 1 role")
        let without = await coordinator(MockServer(routingPolicy: nil))
        #expect(SettingsSheet.routingSummary(RoutingPolicyModel(coordinator: without)) == "No policy")
    }

    // MARK: Offering

    @Test func aRoleOffersOnlyCatalogAgentsItDoesNotOrder() async throws {
        let coordinator = await coordinator()
        let model = RoutingPolicyModel(coordinator: coordinator)
        model.beginEditing()
        let draft = try #require(model.draft)
        let catalog = try #require(model.catalog)
        #expect(draft.offered(for: .implementer, catalog: catalog) == ["claude-secondary"])
        #expect(draft.offered(for: .reviewer, catalog: catalog) == catalog.agents.map(\.name))
    }

    @Test func withoutACatalogThePolicyReadsButCannotChange() async throws {
        var policy = UsageFixtures.defaultRoutingPolicy()
        policy.drain_first_account = UsageFixtures.readingPoolID
        let coordinator = await coordinator(MockServer(routingPolicy: policy, agentCatalog: nil))
        let model = RoutingPolicyModel(coordinator: coordinator)
        #expect(model.policy == policy)
        #expect(model.policy?.drain_first_account == UsageFixtures.readingPoolID)
        #expect(model.drainFirstPools.isEmpty)
        #expect(model.unavailableReason?.contains("no agent tree") == true)
        #expect(!model.canEdit)
        model.beginEditing()
        #expect(model.draft == nil)
    }

    /// The daemon accepts only a pool a tree agent's enrollment sits on, so
    /// the edit offers no other; `spare` has no enrollment.
    @Test func drainFirstOffersOnlyPoolsACatalogAgentIsOn() async throws {
        let model = RoutingPolicyModel(coordinator: await coordinator())
        #expect(
            model.drainFirstPools.map(\.pool.id) == [UsageFixtures.readingPoolID, UsageFixtures.unsupportedPoolID])
        #expect(model.drainFirstPools.first?.agents == ["claude-primary", "claude-secondary"])
        #expect(model.drainFirstPools.last?.agents == ["codex-primary"])
        #expect(RoutingPolicyModel.drainFirstPools(UsageFixtures.defaultUsagePools(), catalog: nil).isEmpty)
        let withoutTree = RoutingPolicyModel(coordinator: await coordinator(MockServer(agentCatalog: nil)))
        #expect(withoutTree.drainFirstPools.isEmpty)
    }

    @Test func draftEditsKeepTheCommandShape() {
        var draft = RoutingPolicyDraft(policy: UsageFixtures.defaultRoutingPolicy(), syncEpoch: "epoch")
        draft.add("claude-primary", to: .reviewer)
        draft.add("claude-primary", to: .reviewer)
        draft.move("codex-primary", in: .implementer, by: -1)
        draft.move("codex-primary", in: .implementer, by: -1)
        #expect(
            draft.roles == [
                .init(role: .implementer, agents: ["codex-primary", "claude-primary"]),
                .init(role: .reviewer, agents: ["claude-primary"]),
            ])
        // Removing a role's last agent returns the role to the lineup.
        draft.remove("claude-primary", from: .reviewer)
        #expect(draft.roles.map(\.role) == [.implementer])
        #expect(draft.baseRevision == 2)
    }

    @Test func aRepeatedRoleOrAgentFromTheDaemonKeepsTheFirst() {
        var policy = UsageFixtures.defaultRoutingPolicy()
        policy.roles = [
            .init(role: .implementer, agents: ["claude-primary", "codex-primary", "claude-primary"]),
            .init(role: .implementer, agents: ["claude-secondary"]),
        ]
        let draft = RoutingPolicyDraft(policy: policy, syncEpoch: "epoch")
        #expect(draft.roles == [.init(role: .implementer, agents: ["claude-primary", "codex-primary"])])
    }

    @Test func aDraftFromAnotherEpochCannotSave() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        editImplementer(model)
        #expect(model.saveBlocker == nil)
        // A restore that lands on the same policy revision in a new epoch.
        await server.rotateEpoch()
        await coordinator.refresh()
        #expect(coordinator.routingPolicy?.policy.revision == model.draft?.baseRevision)
        #expect(model.saveBlocker?.contains("changed while you were editing") == true)
    }

    @Test func saveIsBlockedUntilTheDraftIsAValidChange() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        model.beginEditing()
        #expect(model.saveBlocker == "No change to save.")
        model.draft?.remove("claude-primary", from: .implementer)
        model.draft?.remove("codex-primary", from: .implementer)
        #expect(model.saveBlocker == "Order at least one agent for a role.")
        model.draft?.add("retired-agent", to: .implementer)
        #expect(model.saveBlocker?.contains("doesn't carry") == true)
        model.cancelEditing()

        // Either half alone is a change; the pair as it was is not.
        model.beginEditing()
        model.draft?.drainFirstAccount = UsageFixtures.readingPoolID
        #expect(model.saveBlocker == nil)
        model.draft?.drainFirstAccount = nil
        #expect(model.saveBlocker == "No change to save.")
        model.cancelEditing()

        // Another operator's change, adopted mid-edit, blocks the stale draft.
        editImplementer(model)
        #expect(model.saveBlocker == nil)
        try await submitFromAnotherDevice(server, coordinator, agents: ["codex-primary"], expected: 2)
        await coordinator.refresh()
        #expect(model.saveBlocker?.contains("changed while you were editing") == true)
    }

    // MARK: Sending

    @Test func aSavedChangeIsConfirmedByTheSyncedRevision() async throws {
        let cache = InMemoryCacheStore()
        let coordinator = await coordinator(cache: cache)
        let model = RoutingPolicyModel(coordinator: coordinator)
        editImplementer(model)
        await model.save()
        #expect(coordinator.pendingRoutingPolicyChange == nil)
        #expect(coordinator.routingPolicy?.policy.revision == 3)
        #expect(
            coordinator.routingPolicy?.policy.roles == [
                .init(role: .implementer, agents: ["claude-secondary", "claude-primary", "codex-primary"])
            ])
        #expect(coordinator.routingPolicy?.policy.set_by == "device:device-mock")
        #expect(model.confirmed?.policy.revision == 3)
        #expect(model.confirmed?.recorded == true)
        #expect(cache.load()?.pendingRoutingPolicyChange == nil)
    }

    /// The sheet edits only the orders: a command without the account would
    /// clear it, so a save keeps the drain-first account it started from.
    @Test func aSavedChangeKeepsTheDrainFirstAccount() async throws {
        var policy = UsageFixtures.defaultRoutingPolicy()
        policy.drain_first_account = UsageFixtures.unsupportedPoolID
        let coordinator = await coordinator(MockServer(routingPolicy: policy))
        let model = RoutingPolicyModel(coordinator: coordinator)
        editImplementer(model)
        #expect(model.draft?.drainFirstAccount == UsageFixtures.unsupportedPoolID)
        await model.save()
        #expect(coordinator.routingPolicy?.policy.revision == 3)
        #expect(coordinator.routingPolicy?.policy.drain_first_account == UsageFixtures.unsupportedPoolID)
    }

    @Test func aSavedAccountIsSetChangedAndCleared() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        let roles = UsageFixtures.defaultRoutingPolicy().roles
        for (account, revision) in [
            (UsageFixtures.readingPoolID, 3), (UsageFixtures.unsupportedPoolID, 4), (nil, 5),
        ] {
            model.beginEditing()
            model.draft?.drainFirstAccount = account
            await model.save()
            #expect(coordinator.routingPolicy?.policy.revision == revision)
            #expect(coordinator.routingPolicy?.policy.drain_first_account == account)
            // An account-only change sends the orders unchanged.
            #expect(coordinator.routingPolicy?.policy.roles == roles)
            #expect(model.confirmed?.recorded == true)
        }
    }

    /// A pool no catalog agent sits on any more blocks Save, as an agent the
    /// tree dropped does. The mock's tree here drops codex-primary, the only
    /// agent on codex-b.
    @Test func aStaleAccountBlocksSave() async throws {
        var policy = UsageFixtures.defaultRoutingPolicy()
        policy.drain_first_account = UsageFixtures.unsupportedPoolID
        var catalog = UsageFixtures.defaultAgentCatalog()
        catalog.agents.removeAll { $0.name == "codex-primary" }
        policy.roles = [.init(role: .implementer, agents: ["claude-primary"])]
        let model = RoutingPolicyModel(
            coordinator: await coordinator(MockServer(routingPolicy: policy, agentCatalog: catalog)))
        #expect(!model.drainFirstPools.contains { $0.pool.id == UsageFixtures.unsupportedPoolID })
        editImplementer(model)
        #expect(model.saveBlocker?.contains("Clear the drain-first account or pick another") == true)
        model.draft?.drainFirstAccount = nil
        #expect(model.saveBlocker == nil)
    }

    @Test func aFirstPolicyExpectsNoRevision() async throws {
        let server = MockServer(routingPolicy: nil)
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        model.beginEditing()
        #expect(model.draft?.baseRevision == nil)
        model.draft?.add("codex-primary", to: .reviewer)
        await model.save()
        #expect(coordinator.routingPolicy?.policy.revision == 1)
        #expect(coordinator.routingPolicy?.policy.roles == [.init(role: .reviewer, agents: ["codex-primary"])])
        #expect(model.confirmed?.policy.revision == 1)
    }

    @Test func theCommandIsSavedBeforeItIsSentAndNotSentWhenSavingFails() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server, cache: RefusingCache())
        let model = RoutingPolicyModel(coordinator: coordinator)
        let before = try await revision(coordinator)
        editImplementer(model)
        await model.save()
        #expect(model.failure?.message.contains("Nothing was sent") == true)
        #expect(coordinator.pendingRoutingPolicyChange == nil)
        #expect(try await revision(coordinator) == before)
        #expect(model.draft != nil)
    }

    @Test func confirmationWaitsForSyncToShowTheRevision() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        editImplementer(model)
        // The daemon accepts the change, but the follow-up bootstrap fails.
        await server.setBeforeRespond { operation in
            if operation == "getSyncBootstrap" { throw MockServer.ForcedStatus(503) }
        }
        await model.save()
        let pending = try #require(coordinator.pendingRoutingPolicyChange)
        #expect(pending.receipt != nil)
        #expect(coordinator.routingPolicy?.policy.revision == 2)
        #expect(model.confirmed == nil)

        await server.setBeforeRespond(nil)
        await coordinator.refresh()
        #expect(coordinator.pendingRoutingPolicyChange == nil)
        #expect(model.confirmed?.policy.revision == 3)
    }

    @Test func aForgedReceiptLeavesTheChangeUnresolved() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        await server.setCommandResultTransform { result in
            guard case .set_routing_policy(var record) = result.record else { return result }
            record.device_id = "foreign"
            return .init(record: .set_routing_policy(record), revision: result.revision)
        }
        editImplementer(model)
        await model.save()
        let pending = try #require(coordinator.pendingRoutingPolicyChange)
        #expect(pending.receipt == nil)
    }

    @Test func aStaleChangeShowsTheCurrentPolicyItCarries() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = await coordinator(server, cache: cache)
        let model = RoutingPolicyModel(coordinator: coordinator)
        editImplementer(model)
        // Another operator changes the policy after this device last synced.
        try await submitFromAnotherDevice(
            server, coordinator, agents: ["codex-primary"], drainFirst: UsageFixtures.unsupportedPoolID, expected: 2)
        await model.save()
        let failure = try #require(model.failure)
        #expect(failure.currentPolicy?.revision == 3)
        #expect(failure.currentPolicy?.roles == [.init(role: .implementer, agents: ["codex-primary"])])
        #expect(failure.currentPolicy?.drain_first_account == UsageFixtures.unsupportedPoolID)
        #expect(coordinator.pendingRoutingPolicyChange == nil)
        #expect(cache.load()?.pendingRoutingPolicyChange == nil)
        // The refusal's policy reaches state only through sync.
        #expect(coordinator.routingPolicy?.policy == failure.currentPolicy)
        // A sheet reopened after the refusal still shows it.
        #expect(RoutingPolicyModel(coordinator: coordinator).failure == failure)
    }

    @Test func aLostAnswerSurvivesRelaunchAndRetryResendsTheSameCommand() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = DiskCacheStore(directory: directory)
        let server = MockServer()
        let first = await coordinator(server, cache: cache)
        let lost = InjectedFailures(times: 1)
        await server.setAfterRespond { operation in
            if operation == "submitCommand" { try await lost.consume() }
        }
        let firstModel = RoutingPolicyModel(coordinator: first)
        editImplementer(firstModel)
        await firstModel.save()
        let saved = try #require(first.pendingRoutingPolicyChange)
        #expect(saved.receipt == nil)
        #expect(SettingsSheet.routingSummary(firstModel) == "Unconfirmed")
        let applied = try await revision(first)

        let restored = SyncCoordinator(
            client: first.store.client, device: DeviceIdentity(deviceID: "device-mock"), cache: cache,
            submissionDaemonID: "mock")
        #expect(restored.pendingRoutingPolicyChange == saved)
        await restored.refresh()
        // A relaunch never resends on its own.
        #expect(restored.pendingRoutingPolicyChange == saved)
        let restoredModel = RoutingPolicyModel(coordinator: restored)
        await restoredModel.retry()
        // The replay returns the recorded receipt and applies nothing twice.
        #expect(restored.pendingRoutingPolicyChange == nil)
        #expect(try await revision(restored) == applied)
        #expect(restoredModel.confirmed?.command_id == saved.command.command_id)
    }

    @Test func aSavedChangeFromAnotherDeviceOrDaemonIsNotRestored() async throws {
        let cache = InMemoryCacheStore()
        let server = MockServer()
        let first = await coordinator(server, cache: cache)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
        }
        let firstModel = RoutingPolicyModel(coordinator: first)
        editImplementer(firstModel)
        await firstModel.save()
        #expect(first.pendingRoutingPolicyChange != nil)
        let otherDevice = SyncCoordinator(
            client: first.store.client, device: DeviceIdentity(deviceID: "device-other"), cache: cache,
            submissionDaemonID: "mock")
        #expect(otherDevice.pendingRoutingPolicyChange == nil)
        let otherDaemon = SyncCoordinator(
            client: first.store.client, device: DeviceIdentity(deviceID: "device-mock"), cache: cache,
            submissionDaemonID: "other")
        #expect(otherDaemon.pendingRoutingPolicyChange == nil)
    }

    @Test(arguments: [400, 404])
    func aRefusedChangeClearsAndSaysSo(status: Int) async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(status) }
        }
        editImplementer(model)
        await model.save()
        #expect(coordinator.pendingRoutingPolicyChange == nil)
        #expect(model.failure?.message.contains("status \(status)") == true)
    }

    @Test(arguments: [401, 403, 500, 503])
    func anUnresolvedAnswerKeepsTheSavedCommand(status: Int) async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(status) }
        }
        editImplementer(model)
        await model.save()
        let pending = try #require(coordinator.pendingRoutingPolicyChange)
        #expect(pending.receipt == nil)
        #expect(!model.canEdit)
        if status == 401 || status == 403 { #expect(coordinator.store.freshness == .unauthenticated) }
    }

    @Test func anAcceptedChangeSettlesWhenTheEpochChanges() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        editImplementer(model)
        await server.setBeforeRespond { operation in
            if operation == "getSyncBootstrap" { throw MockServer.ForcedStatus(503) }
        }
        await model.save()
        #expect(coordinator.pendingRoutingPolicyChange?.receipt != nil)
        await server.setBeforeRespond(nil)
        // A restore: revisions from the old epoch can't be compared, even
        // when the restored policy's revision reaches the receipt's.
        await server.rotateEpoch(revision: 1)
        await coordinator.refresh()
        #expect(coordinator.pendingRoutingPolicyChange == nil)
        #expect(model.confirmed == nil)
    }

    @Test func anUnansweredChangeIsNotResentInANewEpoch() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
        }
        editImplementer(model)
        await model.save()
        #expect(coordinator.pendingRoutingPolicyChange?.receipt == nil)
        await server.setBeforeRespond(nil)
        await server.rotateEpoch()
        let operations = OperationLog()
        await server.setBeforeRespond { await operations.record($0) }
        await coordinator.refresh()
        await model.retry()
        #expect(coordinator.pendingRoutingPolicyChange == nil)
        #expect(await !operations.operations.contains("submitCommand"))
        #expect(coordinator.routingPolicy?.policy == UsageFixtures.defaultRoutingPolicy())
    }

    // MARK: Helpers

    /// Sets the implementer order as another paired device would.
    private func submitFromAnotherDevice(
        _ server: MockServer, _ coordinator: SyncCoordinator, agents: [String], drainFirst: String? = nil,
        expected: Int?
    ) async throws {
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString, device_id: DeviceFixtures.otherDeviceID,
            payload: .set_routing_policy(
                .init(
                    kind: .set_routing_policy, roles: [.init(role: .implementer, agents: agents)],
                    drain_first_account: drainFirst, expected_revision: expected)))
        _ = try await coordinator.store.client.submitCommand(body: .json(command)).ok
    }
}

private struct RefusingCache: CacheStore {
    struct Unavailable: Error {}
    func load() -> CachedState? { nil }
    func save(_ state: CachedState) throws { throw Unavailable() }
    func discard() {}
}

private actor OperationLog {
    private(set) var operations: [String] = []
    func record(_ operation: String) { operations.append(operation) }
}
