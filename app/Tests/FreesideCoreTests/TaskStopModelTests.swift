import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

#if os(macOS)
    import AppKit
    import SwiftUI
#endif

@Suite @MainActor struct TaskStopModelTests {
    private func coordinator(
        _ server: MockServer = MockServer(), cache: any CacheStore = InMemoryCacheStore(),
        device: String = "device-mock", daemon: String = "mock"
    ) async -> SyncCoordinator {
        let result = SyncCoordinator(
            client: APIClientFactory.mock(server: server),
            device: DeviceIdentity(deviceID: device), cache: cache, submissionDaemonID: daemon)
        await result.refresh()
        return result
    }

    private func preparation(_ coordinator: SyncCoordinator) throws -> TaskStopModel.Confirmation {
        let task = try #require(coordinator.tasks.first { $0.task.cancellation == nil })
        return try #require(coordinator.taskStop.prepare(taskID: task.task.id))
    }

    @Test func bindsSnapshotAndAcceptanceDoesNotReleaseWIP() async throws {
        let coordinator = await coordinator()
        let prepared = try preparation(coordinator)
        let before = try #require(coordinator.tasks.first { $0.task.id == prepared.entry.taskID })
        let command = prepared.entry.command
        #expect(command.expected_entity_version == before.entity_version)
        #expect(command.expected_bindings == nil)
        #expect(command.device_id == coordinator.store.device.deviceID)
        guard case .stop_task(let payload) = command.payload else {
            Issue.record("Wrong payload")
            return
        }
        #expect(payload.expected_sync_epoch == coordinator.cursors?.syncEpoch)
        #expect(payload.project_id == before.task.project_id)
        await coordinator.taskStop.confirm(prepared)
        let after = try #require(coordinator.tasks.first { $0.task.id == before.task.id })
        #expect(after.task.cancellation?.value1.state == .requested)
        #expect(after.task.wip == before.task.wip)
        #expect(after.task.lifecycle == before.task.lifecycle)
        #expect(after.task.run_ids == before.task.run_ids)
        #expect(coordinator.taskStop.pending.isEmpty)
        #expect(coordinator.taskStop.prepare(taskID: before.task.id) == nil)
    }

    @Test func diskFailureSendsNothing() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server, cache: RefusingStopCache())
        let prepared = try preparation(coordinator)
        let before = try await coordinator.store.client.getSyncRevision().ok.body.json
        await coordinator.taskStop.confirm(prepared)
        #expect(try await coordinator.store.client.getSyncRevision().ok.body.json == before)
        #expect(coordinator.pendingTaskStops.isEmpty)
        #expect(coordinator.taskStop.messages[prepared.entry.taskID]?.contains("Nothing was sent") == true)
    }

    @Test func lostResponseSurvivesDiskRelaunchAndReadEvictionWithoutResending() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = DiskCacheStore(directory: directory)
        let server = MockServer()
        let first = await coordinator(server, cache: cache)
        let prepared = try preparation(first)
        let lost = InjectedFailures(times: 1)
        await server.setAfterRespond { operation in
            if operation == "submitCommand" { try await lost.consume() }
        }
        await first.taskStop.confirm(prepared)
        #expect(first.pendingTaskStops[prepared.id]?.command == prepared.entry.command)
        let revision = try await first.store.client.getSyncRevision().ok.body.json
        // Model an epoch/read-cache eviction with only the independent ledger retained.
        try cache.save(
            CachedState(
                cursors: nil, attentionItems: [],
                pendingTaskStops: first.pendingTaskStops, stopDaemonID: "mock"))
        let restored = SyncCoordinator(client: first.store.client, cache: cache)
        #expect(restored.tasks.isEmpty)
        #expect(restored.taskStop.pending.first == prepared.entry)
        await restored.refresh()
        #expect(try await restored.store.client.getSyncRevision().ok.body.json == revision)
        #expect(restored.pendingTaskStops[prepared.id]?.command == prepared.entry.command)
        await restored.taskStop.retry(prepared.id)
        #expect(restored.pendingTaskStops.isEmpty)
        #expect(try await restored.store.client.getSyncRevision().ok.body.json == revision)
    }

    @Test func concurrentLocalConfirmAndRetrySendOnlyOnce() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let prepared = try preparation(coordinator)
        let entered = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" {
                await entered.open()
                await release.wait()
            }
        }
        let first = Task { await coordinator.taskStop.confirm(prepared) }
        await entered.wait()
        await coordinator.taskStop.confirm(prepared)
        await coordinator.taskStop.retry(prepared.id)
        #expect(coordinator.taskStop.sending == [prepared.entry.taskID])
        #expect(coordinator.pendingTaskStops.count == 1)
        await release.open()
        await first.value
        #expect(coordinator.pendingTaskStops.isEmpty)
    }

    #if os(macOS)
        @Test func sendingThenAcceptedWithoutSyncRemainDistinct() async throws {
            let server = MockServer()
            let coordinator = await coordinator(server)
            let prepared = try preparation(coordinator)
            let entered = AsyncGate()
            let release = AsyncGate()
            await server.setAfterRespond { operation in
                if operation == "submitCommand" {
                    await entered.open()
                    await release.wait()
                }
            }
            let sending = Task { await coordinator.taskStop.confirm(prepared) }
            await entered.wait()
            #expect(coordinator.taskStop.sending.contains(prepared.entry.taskID))
            try captureStop(coordinator, taskID: prepared.entry.taskID, state: "sending")
            await server.setBeforeRespond { operation in
                if operation != "submitCommand" { throw MockServer.ForcedStatus(503) }
            }
            await release.open()
            await sending.value
            #expect(!coordinator.taskStop.sending.contains(prepared.entry.taskID))
            #expect(coordinator.pendingTaskStops[prepared.id]?.receipt != nil)
            #expect(coordinator.tasks.first(where: { $0.task.id == prepared.entry.taskID })?.task.cancellation == nil)
            try captureStop(coordinator, taskID: prepared.entry.taskID, state: "accepted-without-sync")
        }

        private func captureStop(_ coordinator: SyncCoordinator, taskID: String, state: String) throws {
            guard let path = ProcessInfo.processInfo.environment["FREESIDE_STOP_CAPTURE"] else { return }
            _ = FreesideFont.registration
            for scheme in [ColorScheme.light, .dark] {
                let view = TaskStopView(coordinator: coordinator, taskID: taskID)
                    .padding(24).frame(width: 390).background(Color.ground)
                    .environment(\.colorScheme, scheme).environment(\.dynamicTypeSize, .large)
                let renderer = ImageRenderer(content: view)
                renderer.scale = 2
                let image = try #require(renderer.cgImage)
                let png = try #require(NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:]))
                try png.write(to: URL(fileURLWithPath: path).appendingPathComponent("stop-\(state)-\(scheme).png"))
            }
        }
    #endif

    @Test func staleOpenConfirmationSendsWithoutRebinding() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let prepared = try preparation(coordinator)
        let preparedVersion = prepared.entry.command.expected_entity_version
        // An unrelated write advances the revision while the confirmation sheet is
        // open. A revision change alone no longer refuses the Stop: it sends.
        _ = await TaskSubmissionModel(coordinator: coordinator).submit(projectID: "project-1", source: "Other work")
        await coordinator.taskStop.confirm(prepared)
        let task = try #require(coordinator.tasks.first { $0.task.id == prepared.entry.taskID })
        #expect(task.task.cancellation?.value1.state == .requested)
        #expect(coordinator.taskStop.messages[prepared.entry.taskID] == nil)
        // The sent command is never rebound: the daemon records the prepared
        // version, recovered here by replaying the same command_id.
        let replay = try await coordinator.store.client.submitCommand(body: .json(prepared.entry.command)).ok.body.json
        guard case .stop_task(let record) = replay.record else {
            Issue.record("Wrong record")
            return
        }
        #expect(record.expected_entity_version == preparedVersion)
    }

    @Test func changedEpochInvalidatesConfirmation() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let prepared = try preparation(coordinator)
        await server.restoreAttentionState(items: [], revision: 1)
        await coordinator.refresh()
        await coordinator.taskStop.confirm(prepared)
        #expect(coordinator.pendingTaskStops.isEmpty)
        #expect(coordinator.taskStop.messages[prepared.entry.taskID]?.contains("confirm Stop again") == true)
    }

    @Test func acceptedRecoverySettlesOnUnchangedHeartbeatAfterRelaunch() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let first = await coordinator(server, cache: cache)
        let prepared = try preparation(first)
        let result = try await first.store.client.submitCommand(body: .json(prepared.entry.command)).ok.body.json
        await first.refresh()
        var accepted = prepared.entry
        accepted.receipt = result
        #expect(first.retainTaskStop(accepted))
        let restored = SyncCoordinator(client: first.store.client, cache: cache)
        #expect(restored.pendingTaskStops.count == 1)
        await restored.heartbeat()
        #expect(restored.pendingTaskStops.isEmpty)
        #expect(cache.load()?.pendingTaskStops?.isEmpty == true)
    }

    @Test func lateReceiptCannotReplaceNewerFailedOrConfirmedSync() async throws {
        for state: Components.Schemas.TaskCancellationState in [.failed_to_stop, .confirmed] {
            let server = MockServer()
            let coordinator = await coordinator(server)
            let prepared = try preparation(coordinator)
            let entered = AsyncGate()
            let release = AsyncGate()
            await server.setAfterRespond { operation in
                if operation == "submitCommand" {
                    await entered.open()
                    await release.wait()
                }
            }
            let sending = Task { await coordinator.taskStop.confirm(prepared) }
            await entered.wait()
            await server.setBootstrapTransform { bootstrap in
                var result = bootstrap
                for index in result.tasks.indices where result.tasks[index].task.id == prepared.entry.taskID {
                    guard var cancellation = result.tasks[index].task.cancellation?.value1 else { continue }
                    cancellation.state = state
                    cancellation.acknowledgement = .init(
                        value1: .init(
                            id: "ack", request_id: cancellation.request_id,
                            target_digest: cancellation.target_digest, state: state,
                            evidence_digest: cancellation.target_digest, recorded_at: cancellation.requested_at))
                    result.tasks[index].task.cancellation = .init(value1: cancellation)
                }
                return result
            }
            await coordinator.bootstrap()
            await server.setBeforeRespond { operation in
                if operation != "submitCommand" { throw MockServer.ForcedStatus(503) }
            }
            await release.open()
            await sending.value
            let task = try #require(coordinator.tasks.first { $0.task.id == prepared.entry.taskID })
            #expect(task.task.cancellation?.value1.state == state)
            #expect(coordinator.taskStop.prepare(taskID: task.task.id) == nil)
        }
    }

    @Test func serverSideEpochConflictRefreshesAndRequiresNewConfirmation() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let prepared = try preparation(coordinator)
        guard case .stop_task(let payload) = prepared.entry.command.payload else {
            Issue.record("Wrong payload")
            return
        }
        // The daemon's sync epoch rotates after the client prepared but before it
        // confirms. The local check passes on the client's cached epoch, so the
        // Stop sends and the daemon returns a stale-epoch 409 (a too-great version
        // is unreachable from a well-behaved client, which never over-sends).
        await server.rotateEpoch()
        await coordinator.taskStop.confirm(prepared)
        #expect(coordinator.pendingTaskStops.isEmpty)
        #expect(coordinator.taskStop.messages[prepared.entry.taskID]?.contains("confirm Stop again") == true)
        #expect(coordinator.tasks.first { $0.task.id == prepared.entry.taskID }?.task.cancellation == nil)
        let next = try #require(coordinator.taskStop.prepare(taskID: prepared.entry.taskID))
        #expect(next.id != prepared.id)
        guard case .stop_task(let nextPayload) = next.entry.command.payload else {
            Issue.record("Wrong payload")
            return
        }
        #expect(nextPayload.expected_sync_epoch != payload.expected_sync_epoch)
    }

    @Test func confirmMovesOffTheBareStopButtonThroughSendingAndAccepted() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let prepared = try preparation(coordinator)
        let taskID = prepared.entry.taskID
        // Before confirm the control is the bare Stop button.
        #expect(TaskStopView(coordinator: coordinator, taskID: taskID).showsOnlyTheStopButton)
        let entered = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" {
                await entered.open()
                await release.wait()
            }
        }
        let sending = Task { await coordinator.taskStop.confirm(prepared) }
        await entered.wait()
        // Sending: the control speaks, never the bare button.
        #expect(coordinator.taskStop.sending.contains(taskID))
        #expect(!TaskStopView(coordinator: coordinator, taskID: taskID).showsOnlyTheStopButton)
        await release.open()
        await sending.value
        // Accepted: the synced cancellation keeps the control off the bare button.
        #expect(coordinator.tasks.first { $0.task.id == taskID }?.task.cancellation?.value1.state == .requested)
        #expect(!TaskStopView(coordinator: coordinator, taskID: taskID).showsOnlyTheStopButton)
    }

    @Test(arguments: [(503, false), (404, true)])
    func uncertainOrRejectedConfirmNeverLeavesTheBareStopButton(status: Int, definitive: Bool) async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let prepared = try preparation(coordinator)
        let taskID = prepared.entry.taskID
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(status) }
        }
        await coordinator.taskStop.confirm(prepared)
        if definitive {
            // Rejected: the request is cleared but a message replaces the bare button.
            #expect(coordinator.pendingTaskStops.isEmpty)
            #expect(coordinator.taskStop.messages[taskID] != nil)
        } else {
            // Uncertain: the retryable pending entry replaces the bare button.
            #expect(coordinator.pendingTaskStops[prepared.id]?.receipt == nil)
        }
        #expect(!TaskStopView(coordinator: coordinator, taskID: taskID).showsOnlyTheStopButton)
    }

    @Test(arguments: [400, 404], [false, true])
    func definitiveRejectionClearsDurableRequest(status: Int, retry: Bool) async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let first = await coordinator(server, cache: cache)
        let prepared = try preparation(first)
        var current = first
        if retry {
            await server.setBeforeRespond { operation in
                if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
            }
            await first.taskStop.confirm(prepared)
            current = await coordinator(server, cache: cache)
            #expect(current.pendingTaskStops[prepared.id] == prepared.entry)
        }
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(status) }
        }
        if retry {
            await current.taskStop.retry(prepared.id)
        } else {
            await current.taskStop.confirm(prepared)
        }
        #expect(current.pendingTaskStops.isEmpty)
        #expect(current.taskStop.messages[prepared.entry.taskID]?.contains("rejected by the daemon") == true)
        let restored = await coordinator(server, cache: cache)
        #expect(restored.pendingTaskStops.isEmpty)
        let next = try #require(restored.taskStop.prepare(taskID: prepared.entry.taskID))
        #expect(next.id != prepared.id)
    }

    @Test(arguments: [401, 403, 408, 429, 500, 200, 409])
    func ambiguousOrAuthenticationResponseKeepsIdentity(status: Int) async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let prepared = try preparation(coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(status) }
        }
        await coordinator.taskStop.confirm(prepared)
        #expect(coordinator.pendingTaskStops[prepared.id] == prepared.entry)
        if status == 401 || status == 403 { #expect(coordinator.store.freshness == .unauthenticated) }
    }

    @Test func forgedReceiptCannotResolveSavedRequest() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let prepared = try preparation(coordinator)
        await server.setCommandResultTransform { result in
            guard case .stop_task(var record) = result.record else { return result }
            record.device_id = "foreign"
            return .init(record: .stop_task(record), revision: result.revision)
        }
        await coordinator.taskStop.confirm(prepared)
        #expect(coordinator.pendingTaskStops[prepared.id]?.receipt == nil)
        #expect(coordinator.pendingTaskStops[prepared.id]?.command == prepared.entry.command)
    }

    @Test func wrongDeviceDaemonAndMalformedCacheCannotReplay() async throws {
        let first = await coordinator()
        let prepared = try preparation(first)
        for change in 0..<7 {
            let cache = InMemoryCacheStore()
            var command = prepared.entry.command
            switch change {
            case 2: command.expected_entity_version = 0
            case 3: command.expected_bindings = .init()
            case 4: command.command_id = "wrong-key"
            case 5: command.payload = .submit_task(.init(kind: .submit_task, project_id: "p", source: "x"))
            case 6:
                command.payload = .stop_task(
                    .init(kind: .stop_task, task_id: "", project_id: "p", expected_sync_epoch: "e"))
            default: break
            }
            let entry = PendingTaskStop(command: command, taskName: "Task", projectName: "Project")
            try cache.save(
                .init(cursors: nil, attentionItems: [], pendingTaskStops: [prepared.id: entry], stopDaemonID: "mock"))
            let restored = SyncCoordinator(
                client: first.store.client,
                device: .init(deviceID: change == 0 ? "other" : "device-mock"), cache: cache,
                submissionDaemonID: change == 1 ? "other" : "mock")
            #expect(restored.pendingTaskStops.isEmpty)
        }
    }

    @Test func twoDevicesConvergeOnOneFence() async throws {
        let server = MockServer()
        let first = await coordinator(server)
        let prepared = try preparation(first)
        await first.taskStop.confirm(prepared)
        let second = await coordinator(server, device: "another-device")
        let task = try #require(second.tasks.first { $0.task.id == prepared.entry.taskID })
        #expect(second.taskStop.prepare(taskID: task.task.id) == nil)
        // A previously prepared device may still submit its distinct command; API regression tests
        // cover fence idempotence. Both clients render the same daemon state.
        let observed = first.tasks.first { $0.task.id == task.task.id }
        #expect(task.task.cancellation == observed?.task.cancellation)
    }

    @Test func finishedAndAbandonedWithoutFenceStillOfferStop() async throws {
        let finished = try #require(TaskFixtures.defaultTasks().first(where: { $0.task.lifecycle == .finished }))
        var held = finished
        held.task.wip = true
        for snapshot in [held, TaskFixtures.explicitlyAbandoned(finished)] {
            let coordinator = await coordinator(MockServer(tasks: [snapshot]))
            #expect(coordinator.taskStop.prepare(taskID: snapshot.task.id) != nil)
        }
        let stopped = TaskFixtures.confirmedStopped(finished)
        let coordinator = await coordinator(MockServer(tasks: [stopped]))
        #expect(coordinator.taskStop.prepare(taskID: stopped.task.id) == nil)
    }

    @Test func nonFreshStatesCannotPrepare() async throws {
        let coordinator = await coordinator()
        let taskID = try preparation(coordinator).entry.taskID
        for freshness: InboxStore.Freshness in [.unvalidated, .unreachable, .syncFailing, .unauthenticated] {
            coordinator.store.freshness = freshness
            #expect(coordinator.taskStop.prepare(taskID: taskID) == nil)
            #expect(coordinator.taskStop.unavailableReason != nil)
        }
        #expect(TaskStopView.cancellationText(.failed_to_stop).contains("Execution may continue"))
        #expect(TaskStopView.cancellationText(.confirmed).contains("confirmed by the daemon"))
    }
}

private struct RefusingStopCache: CacheStore {
    struct Unavailable: Error {}
    func load() -> CachedState? { nil }
    func save(_ state: CachedState) throws { throw Unavailable() }
    func discard() {}
}
