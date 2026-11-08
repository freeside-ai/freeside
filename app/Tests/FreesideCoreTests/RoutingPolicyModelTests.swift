import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideCore
@testable import FreesideModel

private let claudeMac = UsageFixtures.readingCollectorID
private let claudeMini = UsageFixtures.readingSecondEnrollmentID
private let codexMac = UsageFixtures.unsupportedCollectorID

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

    /// Begins an edit that puts the second Claude enrollment first for the
    /// implementer.
    private func editImplementer(_ model: RoutingPolicyModel) {
        model.beginEditing()
        model.draft?.add(claudeMini, to: .implementer)
        model.draft?.move(claudeMini, in: .implementer, by: -1)
        model.draft?.move(claudeMini, in: .implementer, by: -1)
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

    @Test func aRoleThePolicyLeavesOutReadsAsItsLineupSelection() async throws {
        let coordinator = await coordinator()
        let model = RoutingPolicyModel(coordinator: coordinator)
        let codex = UsageFixtures.agentSelection("codex-primary")
        #expect(model.lineupSelection(for: .reviewer) == codex)
        #expect(
            RoutingPolicyView.lineupText(model.lineupSelection(for: .reviewer))
                == "Lineup: \(UsageFixtures.unsupportedCollectorID) · gpt-codex · harness_default")
        #expect(RoutingPolicyView.lineupText(nil) == "Lineup")
        // A popup names the selection its roles share: Reviewer has one
        // role; Writer's remediator sits on claude-secondary while its
        // specifier and implementer sit on claude-primary.
        #expect(model.sharedLineupSelection(.reviewer) == codex)
        #expect(model.sharedLineupSelection(.writer) == nil)
        #expect(RoutingPolicyView.lineupText(model.sharedLineupSelection(.writer)) == "Lineup")
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
        #expect(SettingsSummary.agents(RoutingPolicyModel(coordinator: withPolicy)) == "\(claudeMac) · \(codexMac)")
        let without = await coordinator(MockServer(routingPolicy: nil))
        #expect(SettingsSummary.agents(RoutingPolicyModel(coordinator: without)) == "No policy")
        // Each enrollment once, in role order, however many roles order it.
        var policy = UsageFixtures.defaultRoutingPolicy()
        policy.roles = [
            .init(role: .implementer, enrollments: [codexMac, claudeMac]),
            .init(role: .reviewer, enrollments: [claudeMac]),
        ]
        let shared = await coordinator(MockServer(routingPolicy: policy))
        #expect(
            SettingsSummary.agents(RoutingPolicyModel(coordinator: shared)) == "\(codexMac) · \(claudeMac)")
    }

    // MARK: Offering

    @Test func aRoleOffersOnlyCatalogEnrollmentsItDoesNotOrder() async throws {
        let coordinator = await coordinator()
        let model = RoutingPolicyModel(coordinator: coordinator)
        model.beginEditing()
        let draft = try #require(model.draft)
        let catalog = try #require(model.catalog)
        #expect(draft.offered(for: .implementer, catalog: catalog) == [claudeMini])
        #expect(draft.offered(for: .reviewer, catalog: catalog) == catalog.enrollments.map(\.enrollment_id))
    }

    @Test func withoutATreeThePolicyReadsButCannotChange() async throws {
        var policy = UsageFixtures.defaultRoutingPolicy()
        policy.drain_first_account = UsageFixtures.readingPoolID
        let coordinator = await coordinator(
            MockServer(routingPolicy: policy, agentCatalog: UsageFixtures.treelessAgentCatalog()))
        let model = RoutingPolicyModel(coordinator: coordinator)
        #expect(model.policy == policy)
        #expect(model.policy?.drain_first_account == UsageFixtures.readingPoolID)
        #expect(model.drainFirstPools.isEmpty)
        #expect(model.unavailableReason?.contains("no agent tree") == true)
        #expect(!model.canEdit)
        model.beginEditing()
        #expect(model.draft == nil)
    }

    /// The daemon accepts only a pool an ordered enrollment sits on, so the
    /// edit offers no other; `spare` has no enrollment, and the policy
    /// doesn't order the second Claude enrollment.
    @Test func drainFirstOffersOnlyPoolsAnOrderedEnrollmentIsOn() async throws {
        let model = RoutingPolicyModel(coordinator: await coordinator())
        #expect(
            model.drainFirstPools.map(\.pool.id) == [UsageFixtures.readingPoolID, UsageFixtures.unsupportedPoolID])
        #expect(model.drainFirstPools.first?.enrollments == [claudeMac])
        #expect(model.drainFirstPools.last?.enrollments == [codexMac])
        // An open draft's orders decide, not the stored policy's.
        model.beginEditing()
        model.draft?.add(claudeMini, to: .reviewer)
        model.draft?.remove(codexMac, from: .implementer)
        #expect(model.drainFirstPools.map(\.pool.id) == [UsageFixtures.readingPoolID])
        #expect(model.drainFirstPools.first?.enrollments == [claudeMac, claudeMini])
        #expect(RoutingPolicyModel.drainFirstPools(UsageFixtures.defaultUsagePools(), ordered: []).isEmpty)
        let withoutTree = RoutingPolicyModel(
            coordinator: await coordinator(MockServer(agentCatalog: UsageFixtures.treelessAgentCatalog())))
        #expect(withoutTree.drainFirstPools.isEmpty)
    }

    @Test func draftEditsKeepTheCommandShape() {
        var draft = RoutingPolicyDraft(policy: UsageFixtures.defaultRoutingPolicy(), catalog: nil, syncEpoch: "epoch")
        draft.add(claudeMac, to: .reviewer)
        draft.add(claudeMac, to: .reviewer)
        draft.move(codexMac, in: .implementer, by: -1)
        draft.move(codexMac, in: .implementer, by: -1)
        #expect(
            draft.roles == [
                .init(role: .implementer, enrollments: [codexMac, claudeMac]),
                .init(role: .reviewer, enrollments: [claudeMac]),
            ])
        // Removing a role's last agent returns the role to the lineup.
        draft.remove(claudeMac, from: .reviewer)
        #expect(draft.roles.map(\.role) == [.implementer])
        #expect(draft.baseRevision == 2)
    }

    @Test func aRepeatedRoleOrEnrollmentFromTheDaemonKeepsTheFirst() {
        var policy = UsageFixtures.defaultRoutingPolicy()
        policy.roles = [
            .init(role: .implementer, enrollments: [claudeMac, codexMac, claudeMac]),
            .init(role: .implementer, enrollments: [claudeMini]),
        ]
        let draft = RoutingPolicyDraft(policy: policy, catalog: nil, syncEpoch: "epoch")
        #expect(draft.roles == [.init(role: .implementer, enrollments: [claudeMac, codexMac])])
    }

    /// A revision stored before policies ordered enrollments (#2736) names
    /// agents, which the wire never translates: the pane shows the names,
    /// and an edit starts with the enrollment each name's catalog agent
    /// runs, dropping a name the catalog lacks, so its first save sends
    /// enrollments.
    @Test func anAgentsFormRevisionShowsNamesAndSeedsTheDraftThroughTheCatalog() async throws {
        var policy = UsageFixtures.defaultRoutingPolicy()
        policy.roles = [.init(role: .implementer, agents: ["codex-primary", "retired-agent", "claude-primary"])]
        let coordinator = await coordinator(MockServer(routingPolicy: policy))
        let model = RoutingPolicyModel(coordinator: coordinator)
        #expect(model.storedOrder(for: .implementer) == ["codex-primary", "retired-agent", "claude-primary"])
        #expect(SettingsSummary.agents(model) == "codex-primary · retired-agent · claude-primary")
        #expect(
            RoutingPolicyView.policyLine(policy)
                == "Revision 2 · Implementer: codex-primary, retired-agent, claude-primary")
        model.beginEditing()
        #expect(model.draft?.roles == [.init(role: .implementer, enrollments: [codexMac, claudeMac])])
        // The seeded orders differ from the stored names, so they save.
        #expect(model.saveBlocker == nil)
        await model.save()
        #expect(
            coordinator.routingPolicy?.policy.roles == [.init(role: .implementer, enrollments: [codexMac, claudeMac])])
        #expect(RoutingPolicyDraft(policy: policy, catalog: nil, syncEpoch: "epoch").roles.isEmpty)
    }

    /// A popup choice saves at once, with no editor to review, so over an
    /// agents-form revision it sends every role's order as the catalog maps
    /// it: the other roles convert too, dropping a name the catalog lacks.
    @Test func aPopupChoiceOverAnAgentsFormRevisionConvertsEveryRole() async throws {
        var policy = UsageFixtures.defaultRoutingPolicy()
        policy.roles = [
            .init(role: .implementer, agents: ["codex-primary", "retired-agent"]),
            .init(role: .reviewer, agents: ["claude-primary"]),
        ]
        let coordinator = await coordinator(MockServer(routingPolicy: policy))
        let model = RoutingPolicyModel(coordinator: coordinator)
        #expect(model.sharedOrder(.reviewer) == ["claude-primary"])
        await model.choose(claudeMini, for: .reviewer)
        #expect(model.choiceBlocker == nil)
        #expect(coordinator.routingPolicy?.policy.revision == 3)
        #expect(
            coordinator.routingPolicy?.policy.roles == [
                .init(role: .implementer, enrollments: [codexMac]),
                .init(role: .reviewer, enrollments: [claudeMini, claudeMac]),
            ])
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
        model.draft?.remove(claudeMac, from: .implementer)
        model.draft?.remove(codexMac, from: .implementer)
        #expect(model.saveBlocker == "Order at least one enrollment for a role.")
        model.draft?.add("enrollment-retired", to: .implementer)
        #expect(model.saveBlocker?.contains("doesn't hold") == true)
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
        try await submitFromAnotherDevice(server, coordinator, enrollments: [codexMac], expected: 2)
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
                .init(role: .implementer, enrollments: [claudeMini, claudeMac, codexMac])
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

    /// A pool no ordered enrollment sits on blocks Save. The policy here
    /// drains codex-b first but orders only a Claude enrollment, as one
    /// stored before the daemon checked the account against the orders.
    @Test func aStaleAccountBlocksSave() async throws {
        var policy = UsageFixtures.defaultRoutingPolicy()
        policy.drain_first_account = UsageFixtures.unsupportedPoolID
        policy.roles = [.init(role: .implementer, enrollments: [claudeMac])]
        let model = RoutingPolicyModel(coordinator: await coordinator(MockServer(routingPolicy: policy)))
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
        model.draft?.add(codexMac, to: .reviewer)
        await model.save()
        #expect(coordinator.routingPolicy?.policy.revision == 1)
        #expect(coordinator.routingPolicy?.policy.roles == [.init(role: .reviewer, enrollments: [codexMac])])
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
            server, coordinator, enrollments: [codexMac], drainFirst: UsageFixtures.unsupportedPoolID, expected: 2)
        await model.save()
        let failure = try #require(model.failure)
        #expect(failure.currentPolicy?.revision == 3)
        #expect(failure.currentPolicy?.roles == [.init(role: .implementer, enrollments: [codexMac])])
        #expect(failure.currentPolicy?.drain_first_account == UsageFixtures.unsupportedPoolID)
        #expect(
            RoutingPolicyView.policyLine(try #require(failure.currentPolicy))
                == "Revision 3 · Implementer: \(codexMac) · Drain first: codex-b")
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
        #expect(SettingsSummary.agents(firstModel) == "Unconfirmed")
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

    // MARK: Popups

    /// The canvas's shape: the three writer roles share one order, the
    /// reviewer has its own, and a drain-first account is set.
    private func agreeingPolicy() -> Components.Schemas.RoutingPolicy {
        var policy = UsageFixtures.defaultRoutingPolicy()
        let writers = [claudeMac, codexMac]
        policy.roles = [
            .init(role: .specifier, enrollments: writers), .init(role: .implementer, enrollments: writers),
            .init(role: .remediator, enrollments: writers), .init(role: .reviewer, enrollments: [codexMac]),
        ]
        policy.drain_first_account = UsageFixtures.readingPoolID
        return policy
    }

    @Test func aWriterChoicePutsTheEnrollmentFirstInTheSharedOrder() async throws {
        let coordinator = await coordinator(MockServer(routingPolicy: agreeingPolicy()))
        let model = RoutingPolicyModel(coordinator: coordinator)
        #expect(model.sharedOrder(.writer) == [claudeMac, codexMac])
        await model.choose(claudeMini, for: .writer)
        let policy = try #require(coordinator.routingPolicy?.policy)
        #expect(policy.revision == 3)
        let writers = [claudeMini, claudeMac, codexMac]
        #expect(
            policy.roles == [
                .init(role: .specifier, enrollments: writers), .init(role: .implementer, enrollments: writers),
                .init(role: .remediator, enrollments: writers), .init(role: .reviewer, enrollments: [codexMac]),
            ])
        #expect(policy.drain_first_account == UsageFixtures.readingPoolID)
        #expect(model.confirmed?.policy.revision == 3)
        #expect(model.draft == nil)
    }

    /// The default policy orders only the implementer, so the writer roles
    /// differ and a choice leaves each of them on the enrollment alone.
    @Test func aWriterChoiceOverDifferingOrdersLeavesTheEnrollmentAlone() async throws {
        let coordinator = await coordinator()
        let model = RoutingPolicyModel(coordinator: coordinator)
        #expect(model.sharedOrder(.writer) == nil)
        await model.choose(codexMac, for: .writer)
        #expect(
            coordinator.routingPolicy?.policy.roles == [
                .init(role: .specifier, enrollments: [codexMac]),
                .init(role: .implementer, enrollments: [codexMac]),
                .init(role: .remediator, enrollments: [codexMac]),
            ])
    }

    @Test func aReviewerChoiceSetsOnlyTheReviewer() async throws {
        let coordinator = await coordinator()
        let model = RoutingPolicyModel(coordinator: coordinator)
        #expect(model.sharedOrder(.reviewer) == [])
        await model.choose(claudeMac, for: .reviewer)
        #expect(
            coordinator.routingPolicy?.policy.roles == [
                .init(role: .implementer, enrollments: [claudeMac, codexMac]),
                .init(role: .reviewer, enrollments: [claudeMac]),
            ])
        #expect(coordinator.routingPolicy?.policy.drain_first_account == nil)
    }

    @Test func aDrainFirstChoiceKeepsTheOrders() async throws {
        let coordinator = await coordinator(MockServer(routingPolicy: agreeingPolicy()))
        let model = RoutingPolicyModel(coordinator: coordinator)
        await model.chooseDrainFirst(UsageFixtures.unsupportedPoolID)
        #expect(coordinator.routingPolicy?.policy.drain_first_account == UsageFixtures.unsupportedPoolID)
        #expect(coordinator.routingPolicy?.policy.roles == agreeingPolicy().roles)
        await model.chooseDrainFirst(nil)
        #expect(coordinator.routingPolicy?.policy.revision == 4)
        #expect(coordinator.routingPolicy?.policy.drain_first_account == nil)
    }

    @Test func aChoiceWhileAChangeIsUnansweredSendsNothing() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
        }
        editImplementer(model)
        await model.save()
        let pending = try #require(model.pending)
        let operations = OperationLog()
        await server.setBeforeRespond { await operations.record($0) }
        await model.choose(codexMac, for: .reviewer)
        await model.chooseDrainFirst(UsageFixtures.readingPoolID)
        #expect(await !operations.operations.contains("submitCommand"))
        #expect(model.pending == pending)
        #expect(model.draft == nil)
    }

    @Test func aChoiceWhileTheEditorIsOpenSendsNothing() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = RoutingPolicyModel(coordinator: coordinator)
        editImplementer(model)
        let draft = try #require(model.draft)
        let operations = OperationLog()
        await server.setBeforeRespond { await operations.record($0) }
        await model.choose(codexMac, for: .reviewer)
        #expect(await !operations.operations.contains("submitCommand"))
        #expect(model.draft == draft)
        #expect(model.pending == nil)
    }

    /// The daemon dropped the Codex enrollment while the reviewer still
    /// orders it, so a Writer choice would resend it and Save's check
    /// refuses it.
    @Test func aChoiceSaveWouldRefuseSendsNothingAndSaysWhy() async throws {
        var policy = UsageFixtures.defaultRoutingPolicy()
        policy.roles = [
            .init(role: .implementer, enrollments: [claudeMac]),
            .init(role: .reviewer, enrollments: [codexMac]),
        ]
        var catalog = UsageFixtures.defaultAgentCatalog()
        catalog.enrollments.removeAll { $0.enrollment_id == codexMac }
        catalog.agents.removeAll { $0.enrollment_id == codexMac }
        let server = MockServer(routingPolicy: policy, agentCatalog: catalog)
        let model = RoutingPolicyModel(coordinator: await coordinator(server))
        let operations = OperationLog()
        await server.setBeforeRespond { await operations.record($0) }
        await model.choose(claudeMini, for: .writer)
        #expect(await !operations.operations.contains("submitCommand"))
        #expect(model.draft == nil)
        #expect(model.pending == nil)
        #expect(model.choiceBlocker == "Remove the enrollments the daemon doesn't hold.")
        // The next edit starts clean.
        model.beginEditing()
        #expect(model.choiceBlocker == nil)
    }

    /// A choice whose command can't be stored sends nothing and leaves no
    /// draft behind, so the editor's sheet doesn't open on it.
    @Test func aChoiceThatCannotBeSavedOnTheDeviceLeavesNoDraft() async throws {
        let server = MockServer()
        let model = RoutingPolicyModel(coordinator: await coordinator(server, cache: RefusingCache()))
        let operations = OperationLog()
        await server.setBeforeRespond { await operations.record($0) }
        await model.choose(claudeMac, for: .reviewer)
        #expect(await !operations.operations.contains("submitCommand"))
        #expect(model.failure?.message.contains("couldn't be saved on this device") == true)
        #expect(model.draft == nil)
        #expect(model.pending == nil)
    }

    @Test func eachTreeAgentNamesThePoolItsEnrollmentSitsOn() async throws {
        let model = RoutingPolicyModel(coordinator: await coordinator())
        let agents = try #require(model.catalog?.agents)
        let pools = Dictionary(uniqueKeysWithValues: agents.map { ($0.name, model.pool(for: $0)) })
        #expect(pools["claude-primary"] == UsageFixtures.readingPoolID)
        #expect(pools["claude-secondary"] == UsageFixtures.readingPoolID)
        #expect(pools["codex-primary"] == UsageFixtures.unsupportedPoolID)
    }

    @Test func replaceSetsTheWholeOrderAndEmptyLeavesTheLineup() {
        var draft = RoutingPolicyDraft(policy: UsageFixtures.defaultRoutingPolicy(), catalog: nil, syncEpoch: "epoch")
        draft.replace([codexMac, claudeMac, codexMac], for: .reviewer)
        draft.replace([], for: .implementer)
        #expect(draft.roles == [.init(role: .reviewer, enrollments: [codexMac, claudeMac])])
    }

    // MARK: Helpers

    /// Sets the implementer order as another paired device would.
    private func submitFromAnotherDevice(
        _ server: MockServer, _ coordinator: SyncCoordinator, enrollments: [String], drainFirst: String? = nil,
        expected: Int?
    ) async throws {
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString, device_id: DeviceFixtures.otherDeviceID,
            payload: .set_routing_policy(
                .init(
                    kind: .set_routing_policy, roles: [.init(role: .implementer, enrollments: enrollments)],
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
