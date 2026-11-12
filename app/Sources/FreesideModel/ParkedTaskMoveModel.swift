import Foundation
import FreesideAPI
import Observation

/// A move of a parked task saved before it was sent: the exact command an
/// explicit Retry resends, and its receipt once the daemon accepted it. Local
/// delivery evidence, never an authority for the task's lines.
public struct PendingParkedTaskMove: Codable, Equatable, Sendable {
    public let command: Components.Schemas.ClientCommand
    /// The epoch the move was made in. A receipt from another epoch can't be
    /// compared with the task sync shows.
    public let syncEpoch: String
    public var receipt: Components.Schemas.CommandResult?

    func isValid(deviceID: String) -> Bool {
        guard case .move_parked_task = command.payload else { return false }
        if let receipt, !CommandResultTrust.accepts(receipt, for: command) { return false }
        return !command.command_id.isEmpty && !deviceID.isEmpty && command.device_id == deviceID
            && !syncEpoch.isEmpty && command.expected_entity_version == nil && command.expected_bindings == nil
    }

    /// The task the move names.
    public var taskID: String? {
        guard case .move_parked_task(let payload) = command.payload else { return nil }
        return payload.task_id
    }
}

/// Why a move failed, on the task it names, so only that task's page says so.
package struct ParkedTaskMoveFailure: Equatable, Sendable {
    package let taskID: String
    package let message: String
}

/// Sends the operator's move of a task parked on its implementer's exhausted
/// quota window to an enrollment the hold offers (`move_parked_task`, issue #869),
/// modeled on `RoutingPolicyModel`: the command persists before it is sent,
/// an unanswered send is retried only by an explicit Retry that resends it
/// unchanged, and the move reads as recorded only once sync shows its
/// revision. The daemon re-derives that the enrollment still qualifies, so the
/// hold's offers are a snapshot, never an authority. The saved command,
/// whether it is being sent, and its refusal live in the coordinator, where a
/// reopened task page finds them.
@MainActor @Observable
package final class ParkedTaskMoveModel {
    private let coordinator: SyncCoordinator

    package init(coordinator: SyncCoordinator) { self.coordinator = coordinator }

    package var sending: Bool { coordinator.parkedTaskMoveSending }
    package var failure: ParkedTaskMoveFailure? {
        get { coordinator.parkedTaskMoveFailure }
        set { coordinator.parkedTaskMoveFailure = newValue }
    }
    package var pending: PendingParkedTaskMove? { coordinator.pendingParkedTaskMove }

    /// Why a task can't be moved now, or nil when it can.
    package var unavailableReason: String? {
        switch coordinator.store.freshness {
        case .fresh, .unvalidated:
            return coordinator.cursors == nil ? "Refresh before moving the task." : nil
        case .unauthenticated: return "Pair this device again before moving the task."
        case .unreachable: return "Offline. Connect to the daemon before moving the task."
        case .syncFailing, .contractMismatch: return "The task isn't current. Refresh before moving it."
        }
    }

    package var canMove: Bool { unavailableReason == nil && pending == nil && !sending }

    package func move(taskID: String, to choice: Components.Schemas.AlternateAgentChoice) async {
        guard canMove, let epoch = coordinator.cursors?.syncEpoch else { return }
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString, device_id: coordinator.store.device.deviceID,
            payload: .move_parked_task(.init(kind: .move_parked_task, task_id: taskID, choice: choice)))
        let entry = PendingParkedTaskMove(command: command, syncEpoch: epoch)
        guard coordinator.retainParkedTaskMove(entry) else {
            failure = .init(
                taskID: taskID, message: "The move couldn't be saved on this device. Nothing was sent. Try again.")
            return
        }
        await send(entry)
    }

    /// Whether Retry can resend the saved move: the daemon hasn't answered
    /// it, it was made in the current epoch (sync settles it in any other),
    /// and the device is still paired.
    package var canRetry: Bool {
        guard let entry = pending else { return false }
        return entry.receipt == nil && entry.syncEpoch == coordinator.cursors?.syncEpoch
            && coordinator.store.freshness != .unauthenticated
    }

    /// Explicit retry only: resends the saved command unchanged.
    package func retry() async {
        guard canRetry, let entry = pending else { return }
        await send(entry)
    }

    private func send(_ entry: PendingParkedTaskMove) async {
        guard !sending else { return }
        coordinator.parkedTaskMoveSending = true
        failure = nil
        defer { coordinator.parkedTaskMoveSending = false }
        do {
            switch try await coordinator.store.client.submitCommand(body: .json(entry.command)) {
            case .ok(let ok):
                let receipt = try ok.body.json
                guard CommandResultTrust.accepts(receipt, for: entry.command) else { return }
                var accepted = entry
                accepted.receipt = receipt
                // If this save fails, the unanswered command stays retryable.
                guard coordinator.retainParkedTaskMove(accepted) else { return }
                await coordinator.refreshAfterCommit()
                coordinator.settleParkedTaskMove()
            case .conflict:
                // No move refuses with a conflict; an unexpected one leaves
                // the saved command unresolved.
                break
            case .badRequest(let refusal):
                // The task is no longer parked, the enrollment no longer
                // qualifies or serves the choice, or this daemon's build
                // doesn't serve the move: the daemon recorded nothing.
                coordinator.finishParkedTaskMove()
                let message = CommandRefusal.message(
                    refusal, otherwise: "The daemon refused the move (status 400). Nothing was changed.")
                failure = entry.taskID.map { .init(taskID: $0, message: message) }
                await coordinator.refreshAfterCommit()
            case .undocumented(let status, _):
                switch status {
                case 404:
                    // The task is no longer parked, or the enrollment no
                    // longer qualifies or serves the choice: the daemon
                    // recorded nothing.
                    coordinator.finishParkedTaskMove()
                    let message = "The daemon refused the move (status \(status)). Nothing was changed."
                    failure = entry.taskID.map { .init(taskID: $0, message: message) }
                    await coordinator.refreshAfterCommit()
                case 401, 403: coordinator.store.freshness = .unauthenticated
                default: break
                }
            }
        } catch {
            // A missing or malformed answer leaves the saved command unresolved.
        }
    }
}
