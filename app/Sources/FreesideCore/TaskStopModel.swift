import Foundation
import FreesideAPI
import Observation

/// Local delivery evidence, never an authority for current runtime state.
public struct PendingTaskStop: Codable, Equatable, Sendable {
    public let command: Components.Schemas.ClientCommand
    public let taskName: String
    public let projectName: String
    public var receipt: Components.Schemas.CommandResult?

    var taskID: String {
        guard case .stop_task(let payload) = command.payload else { return "" }
        return payload.task_id
    }

    func isValid(commandID: String, deviceID: String) -> Bool {
        guard case .stop_task(let payload) = command.payload else { return false }
        if let receipt, !CommandResultTrust.accepts(receipt, for: command) { return false }
        return !commandID.isEmpty && command.command_id == commandID && command.device_id == deviceID
            && !deviceID.isEmpty && !payload.task_id.isEmpty && !payload.project_id.isEmpty
            && !payload.expected_sync_epoch.isEmpty && (command.expected_entity_version ?? 0) > 0
            && command.expected_bindings == nil
    }
}

@MainActor @Observable
final class TaskStopModel {
    struct Confirmation: Identifiable {
        let entry: PendingTaskStop
        var id: String { entry.command.command_id }
    }

    private unowned let coordinator: SyncCoordinator
    private(set) var sending: Set<String> = []
    private(set) var messages: [String: String] = [:]

    init(coordinator: SyncCoordinator) { self.coordinator = coordinator }

    var pending: [PendingTaskStop] {
        coordinator.pendingTaskStops.values.sorted { $0.command.command_id < $1.command.command_id }
    }

    func pending(for taskID: String) -> PendingTaskStop? {
        pending.first { $0.taskID == taskID }
    }

    var unavailableReason: String? {
        switch coordinator.store.freshness {
        case .fresh: return coordinator.cursors == nil ? "Refresh task state before sending Stop." : nil
        case .unauthenticated: return "Pair this device again before sending Stop."
        case .unreachable: return "Offline. Connect to the daemon before sending Stop."
        default: return "Task state is not current. Refresh before sending Stop."
        }
    }

    func prepare(taskID: String) -> Confirmation? {
        guard unavailableReason == nil, pending(for: taskID) == nil,
            let snapshot = coordinator.tasks.first(where: { $0.task.id == taskID }),
            snapshot.task.cancellation == nil, snapshot.entity_version > 0,
            let epoch = coordinator.cursors?.syncEpoch
        else { return nil }
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString, device_id: coordinator.store.device.deviceID,
            expected_entity_version: snapshot.entity_version,
            payload: .stop_task(
                .init(
                    kind: .stop_task, task_id: taskID,
                    project_id: snapshot.task.project_id, expected_sync_epoch: epoch)))
        return Confirmation(
            entry: PendingTaskStop(
                command: command,
                taskName: snapshot.task.display_names.task.text, projectName: TaskDisplay.projectName(snapshot.task)))
    }

    func confirm(_ confirmation: Confirmation) async {
        let entry = confirmation.entry
        guard case .stop_task(let payload) = entry.command.payload,
            unavailableReason == nil, pending(for: entry.taskID) == nil,
            coordinator.cursors?.syncEpoch == payload.expected_sync_epoch,
            let snapshot = coordinator.tasks.first(where: { $0.task.id == entry.taskID }),
            snapshot.task.project_id == payload.project_id,
            snapshot.task.cancellation == nil
        else {
            messages[entry.taskID] = "Task state changed. Refresh and confirm Stop again."
            return
        }
        await send(entry)
    }

    /// Explicit retry only. Epoch/version/target are never rewritten.
    func retry(_ commandID: String) async {
        guard let entry = coordinator.pendingTaskStops[commandID], entry.receipt == nil else { return }
        guard coordinator.store.freshness != .unauthenticated else { return }
        await send(entry)
    }

    func refresh() async { await coordinator.refreshAfterCommit() }

    private func send(_ entry: PendingTaskStop) async {
        let taskID = entry.taskID
        guard !sending.contains(taskID) else { return }
        guard coordinator.retainTaskStop(entry) else {
            messages[taskID] = "Stop could not be saved. Nothing was sent. Try again when storage is available."
            return
        }
        sending.insert(taskID)
        messages[taskID] = nil
        defer { sending.remove(taskID) }
        do {
            switch try await coordinator.store.client.submitCommand(body: .json(entry.command)) {
            case .ok(let ok):
                let receipt = try ok.body.json
                guard CommandResultTrust.accepts(receipt, for: entry.command) else { return }
                var accepted = entry
                accepted.receipt = receipt
                // If this save fails, keep the original uncertain request retryable.
                _ = coordinator.retainTaskStop(accepted)
                await coordinator.refreshAfterCommit()
                coordinator.settleTaskStops()
            case .conflict(let conflict):
                guard case .StaleTaskRejection(let rejection) = try conflict.body.json,
                    case .stop_task(let payload) = entry.command.payload,
                    rejection.replacement_task.task.id == payload.task_id,
                    rejection.replacement_task.task.project_id == payload.project_id,
                    !rejection.sync_epoch.isEmpty, rejection.replacement_task.entity_version > 0,
                    rejection.replacement_task.entity_version == rejection.replacement_task.as_of_revision,
                    rejection.sync_epoch != payload.expected_sync_epoch
                        || rejection.replacement_task.entity_version != entry.command.expected_entity_version
                else { return }
                // Refresh through canonical sync; never install a conflict's rows.
                coordinator.finishTaskStop(entry.command.command_id)
                messages[taskID] = "Task state changed. Review the refreshed task and confirm Stop again."
                await coordinator.refreshAfterCommit()
            case .undocumented(let status, _):
                switch status {
                case 400, 404:
                    // These daemon responses reject the command before acceptance.
                    // Other statuses may leave an earlier attempt's outcome unknown.
                    coordinator.finishTaskStop(entry.command.command_id)
                    messages[taskID] =
                        "Stop was rejected by the daemon (status \(status)). Review the refreshed task before confirming again."
                    await coordinator.refreshAfterCommit()
                case 401, 403: coordinator.store.freshness = .unauthenticated
                default: break
                }
            }
        } catch {
            // Missing or malformed answers leave the saved command unresolved.
        }
    }
}
