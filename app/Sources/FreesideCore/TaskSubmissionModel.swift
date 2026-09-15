import Foundation
import FreesideAPI
import Observation

/// Submits a task from source text (plan §5.11) and lands on it. It builds a
/// submit_task ClientCommand with a fresh command id and no decision envelope,
/// accepts the returned result only through the trust gate, then refreshes for
/// read-your-write and reports the created-or-fetched task id. The composer UI
/// is a follow-on unit; this is the client-layer call it will drive.
@MainActor
@Observable
public final class TaskSubmissionModel {
    public enum SubmissionState: Equatable {
        case idle
        case submitting
        case submitted(taskID: String)
        case failed(String)
    }

    public private(set) var state: SubmissionState = .idle

    private let coordinator: SyncCoordinator

    public init(coordinator: SyncCoordinator) {
        self.coordinator = coordinator
    }

    /// Submits `source` under `projectID` with an optional operator name and
    /// returns the created-or-fetched task id, or nil on failure (state carries
    /// the message). A submit_task command carries no decision envelope.
    @discardableResult
    public func submit(projectID: String, source: String, name: String? = nil) async -> String? {
        state = .submitting
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString,
            device_id: coordinator.store.device.deviceID,
            payload: .submit_task(
                .init(kind: .submit_task, project_id: projectID, source: source, name: name))
        )
        do {
            let output = try await coordinator.store.client.submitCommand(body: .json(command))
            guard case .ok(let ok) = output else {
                state = .failed("the task could not be submitted")
                return nil
            }
            let result = try ok.body.json
            // The result is untrusted until the trust gate confirms it is the
            // record for exactly this submission (matching source digest).
            guard CommandResultTrust.accepts(result, for: command),
                case .submit_task(let record) = result.record
            else {
                state = .failed("the daemon returned an invalid task-submission result")
                return nil
            }
            // Read-your-write: the task and its run are visible on the next sync.
            await coordinator.refresh()
            state = .submitted(taskID: record.task_id)
            return record.task_id
        } catch {
            state = .failed("the task could not be submitted")
            return nil
        }
    }
}
