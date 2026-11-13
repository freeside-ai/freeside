import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// #3145: the workflow policy layers read from sync, and a change is a
/// saved command that reads as recorded only once sync shows its revision,
/// as the routing policy's does.
@Suite @MainActor struct WorkflowPolicyModelTests {
    private let project = WorkflowPolicyFixtures.projectID

    private func coordinator(
        _ server: MockServer = MockServer(), cache: any CacheStore = InMemoryCacheStore()
    ) async -> SyncCoordinator {
        let result = SyncCoordinator(
            client: APIClientFactory.mock(server: server), device: DeviceIdentity(deviceID: "device-mock"),
            cache: cache, submissionDaemonID: "mock")
        await result.refresh()
        return result
    }

    private func setting(
        _ model: WorkflowPolicyModel, _ section: WorkflowPolicySection, _ key: Components.Schemas.WorkflowPolicyKey
    ) -> Components.Schemas.WorkflowPolicySetting? {
        model.settings(in: section).first { $0.key == key }
    }

    // MARK: Reading

    @Test func syncAdoptsTheLayersAndARelaunchRestoresThem() async throws {
        let cache = InMemoryCacheStore()
        let first = await coordinator(cache: cache)
        #expect(first.workflowPolicy?.revision == 2)
        #expect(cache.load()?.workflowPolicy == first.workflowPolicy)
        let restored = SyncCoordinator(client: first.store.client, cache: cache)
        #expect(restored.workflowPolicy == first.workflowPolicy)
    }

    @Test func eachSectionShowsTheWinningValueAndItsOrigin() async throws {
        let model = WorkflowPolicyModel(coordinator: await coordinator())
        #expect(model.sections == [.general, .project(project)])
        let generalClosure = try #require(setting(model, .general, .gates_period_source_issue_closure))
        #expect(WorkflowPolicyView.valueText(generalClosure) == "Ask first")
        #expect(WorkflowPolicyView.originText(generalClosure.origin, in: .general) == "set here")
        let projectClosure = try #require(setting(model, .project(project), .gates_period_source_issue_closure))
        #expect(WorkflowPolicyView.originText(projectClosure.origin, in: .project(project)) == "general")
        let perDay = try #require(setting(model, .project(project), .follow_up_filing_period_max_per_day))
        #expect(perDay.value == "3" && perDay.origin == .project)
        let comment = try #require(setting(model, .project(project), .gates_period_issue_comment))
        #expect(WorkflowPolicyView.valueText(comment) == "Autonomous")
        #expect(WorkflowPolicyView.originText(comment.origin, in: .project(project)) == "default")
        // Only a project row its own layer sets offers the reset.
        #expect(WorkflowPolicyModel.canReset(perDay, in: .project(project)))
        #expect(!WorkflowPolicyModel.canReset(projectClosure, in: .project(project)))
        #expect(!WorkflowPolicyModel.canReset(generalClosure, in: .general))
    }

    @Test func theSettingsRowCountsTheGatesThatAskFirst() async throws {
        let model = WorkflowPolicyModel(coordinator: await coordinator())
        #expect(SettingsSummary.workflow(model) == "1 gate asks first")
        await model.set(.gates_period_issue_comment, to: "true", in: .project(project))
        #expect(SettingsSummary.workflow(model) == "2 gates ask first")
        let empty = await coordinator(MockServer(workflowPolicy: WorkflowPolicyLayers()))
        #expect(SettingsSummary.workflow(WorkflowPolicyModel(coordinator: empty)) == "Autonomous")
    }

    @Test func aCapOffersItsLadderAndTheCurrentValue() {
        #expect(WorkflowPolicyView.capChoices(current: "10") == [1, 2, 3, 5, 10, 20, 50])
        #expect(WorkflowPolicyView.capChoices(current: "7") == [1, 2, 3, 5, 7, 10, 20, 50])
    }

    // MARK: Changing

    @Test func aChangeIsConfirmedByTheSyncedRevision() async throws {
        let cache = InMemoryCacheStore()
        let coordinator = await coordinator(cache: cache)
        let model = WorkflowPolicyModel(coordinator: coordinator)
        await model.set(.gates_period_issue_comment, to: "true", in: .general)
        #expect(coordinator.pendingWorkflowPolicyChange == nil)
        #expect(coordinator.workflowPolicy?.revision == 3)
        let comment = try #require(setting(model, .project(project), .gates_period_issue_comment))
        #expect(comment.value == "true" && comment.origin == .general && comment.set_by == "device:device-mock")
        #expect(model.confirmed?.revision == 3)
        #expect(model.confirmed?.recorded == true)
        #expect(cache.load()?.pendingWorkflowPolicyChange == nil)
    }

    @Test func resetReturnsAProjectRowToGeneral() async throws {
        let model = WorkflowPolicyModel(coordinator: await coordinator())
        await model.resetToGeneral(.follow_up_filing_period_max_per_day, projectID: project)
        let perDay = try #require(setting(model, .project(project), .follow_up_filing_period_max_per_day))
        #expect(perDay.value == "10" && perDay.origin == ._default)
        #expect(model.confirmed?.value == nil)
    }

    @Test func aValueTheKeyRefusesSendsNothing() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = WorkflowPolicyModel(coordinator: coordinator)
        await model.set(.follow_up_filing_period_max_per_run, to: "0", in: .general)
        #expect(coordinator.pendingWorkflowPolicyChange == nil)
        #expect(coordinator.workflowPolicy?.revision == 2)
    }

    @Test func theCommandIsSavedBeforeItIsSentAndNotSentWhenSavingFails() async throws {
        let coordinator = await coordinator(cache: RefusingCache())
        let model = WorkflowPolicyModel(coordinator: coordinator)
        await model.set(.gates_period_issue_comment, to: "true", in: .general)
        #expect(model.failure?.message.contains("Nothing was sent") == true)
        #expect(coordinator.pendingWorkflowPolicyChange == nil)
        #expect(coordinator.workflowPolicy?.revision == 2)
    }

    @Test func confirmationWaitsForSyncToShowTheRevision() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = WorkflowPolicyModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "getSyncBootstrap" { throw MockServer.ForcedStatus(503) }
        }
        await model.set(.gates_period_issue_comment, to: "true", in: .general)
        #expect(coordinator.pendingWorkflowPolicyChange?.receipt != nil)
        #expect(model.confirmed == nil)
        #expect(!model.canEdit)

        await server.setBeforeRespond(nil)
        await coordinator.refresh()
        #expect(coordinator.pendingWorkflowPolicyChange == nil)
        #expect(model.confirmed?.revision == 3)
    }

    @Test func aStaleChangeShowsTheRevisionItCarries() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = await coordinator(server, cache: cache)
        let model = WorkflowPolicyModel(coordinator: coordinator)
        let other = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString, device_id: DeviceFixtures.otherDeviceID,
            payload: .set_workflow_policy(
                .init(
                    kind: .set_workflow_policy, scope: .general, project_id: nil,
                    key: .follow_up_filing_period_max_per_run, value: "3", expected_revision: 2)))
        _ = try await coordinator.store.client.submitCommand(body: .json(other)).ok
        await model.set(.gates_period_issue_comment, to: "true", in: .general)
        let failure = try #require(model.failure)
        #expect(failure.currentPolicy?.revision == 3)
        #expect(coordinator.pendingWorkflowPolicyChange == nil)
        #expect(cache.load()?.pendingWorkflowPolicyChange == nil)
        #expect(coordinator.workflowPolicy?.revision == 3)
        #expect(setting(model, .general, .gates_period_issue_comment)?.value == "false")
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
        let firstModel = WorkflowPolicyModel(coordinator: first)
        await firstModel.set(.gates_period_issue_comment, to: "true", in: .general)
        let saved = try #require(first.pendingWorkflowPolicyChange)
        #expect(saved.receipt == nil)
        #expect(SettingsSummary.workflow(firstModel) == "Unconfirmed")

        let restored = SyncCoordinator(
            client: first.store.client, device: DeviceIdentity(deviceID: "device-mock"), cache: cache,
            submissionDaemonID: "mock")
        #expect(restored.pendingWorkflowPolicyChange == saved)
        await restored.refresh()
        // A relaunch never resends on its own.
        #expect(restored.pendingWorkflowPolicyChange == saved)
        let restoredModel = WorkflowPolicyModel(coordinator: restored)
        await restoredModel.retry()
        #expect(restored.pendingWorkflowPolicyChange == nil)
        #expect(restored.workflowPolicy?.revision == 3)
        #expect(restoredModel.confirmed?.command_id == saved.command.command_id)
    }

    @Test func aSavedChangeFromAnotherDeviceOrDaemonIsNotRestored() async throws {
        let cache = InMemoryCacheStore()
        let server = MockServer()
        let first = await coordinator(server, cache: cache)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
        }
        await WorkflowPolicyModel(coordinator: first).set(.gates_period_issue_comment, to: "true", in: .general)
        #expect(first.pendingWorkflowPolicyChange != nil)
        let otherDevice = SyncCoordinator(
            client: first.store.client, device: DeviceIdentity(deviceID: "device-other"), cache: cache,
            submissionDaemonID: "mock")
        #expect(otherDevice.pendingWorkflowPolicyChange == nil)
        let otherDaemon = SyncCoordinator(
            client: first.store.client, device: DeviceIdentity(deviceID: "device-mock"), cache: cache,
            submissionDaemonID: "other")
        #expect(otherDaemon.pendingWorkflowPolicyChange == nil)
    }

    @Test(arguments: [400, 404])
    func aRefusedChangeClearsAndSaysSo(status: Int) async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = WorkflowPolicyModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(status) }
        }
        await model.set(.gates_period_issue_comment, to: "true", in: .general)
        #expect(coordinator.pendingWorkflowPolicyChange == nil)
        #expect(model.failure?.message.contains("status \(status)") == true)
    }

    @Test(arguments: [401, 403, 500, 503])
    func anUnresolvedAnswerKeepsTheSavedCommand(status: Int) async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = WorkflowPolicyModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(status) }
        }
        await model.set(.gates_period_issue_comment, to: "true", in: .general)
        let pending = try #require(coordinator.pendingWorkflowPolicyChange)
        #expect(pending.receipt == nil)
        #expect(!model.canEdit)
        if status == 401 || status == 403 { #expect(coordinator.store.freshness == .unauthenticated) }
    }

    @Test func anUnansweredChangeIsNotResentInANewEpoch() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = WorkflowPolicyModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
        }
        await model.set(.gates_period_issue_comment, to: "true", in: .general)
        #expect(coordinator.pendingWorkflowPolicyChange?.receipt == nil)
        await server.setBeforeRespond(nil)
        await server.rotateEpoch()
        await coordinator.refresh()
        await model.retry()
        #expect(coordinator.pendingWorkflowPolicyChange == nil)
        #expect(coordinator.workflowPolicy?.revision == 2)
    }
}

private struct RefusingCache: CacheStore {
    struct Unavailable: Error {}
    func load() -> CachedState? { nil }
    func save(_ state: CachedState) throws { throw Unavailable() }
    func discard() {}
}
