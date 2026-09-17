import Foundation
import FreesideAPI
import Observation

/// Submits a task from source text (plan §5.11) and lands on it, driving the
/// New Task composer and the separate recovery screen. It builds a
/// submit_task ClientCommand with a fresh command id and no decision envelope,
/// accepts the returned result only
/// through the trust gate, then refreshes for read-your-write and reports the
/// submitted task id. A lost response keeps the built command so
/// `retry` can resend the identical command id; the daemon converges a repeat
/// of the same command on one task. A daemon answer that authoritatively
/// rejects the submission is a `.rejected` typed reason, not a retry.
@MainActor
@Observable
public final class TaskSubmissionModel {
    public enum SubmissionState: Equatable {
        case idle
        case submitting
        case submitted(taskID: String)
        /// The request threw, or the daemon answered ambiguously (a 5xx or
        /// an unreadable 200), or authentication prevented replay: the command
        /// may have committed with its response lost, so it stays retryable. Retrying
        /// reuses the command id, so the daemon converges on one task.
        case lost
        /// The daemon answered authoritatively that it did not record the
        /// submission (a non-authentication 4xx). The reason is shown and retry
        /// is not offered.
        case rejected(String)
    }

    public private(set) var state: SubmissionState = .idle

    private let coordinator: SyncCoordinator
    /// The command `submit` last built, kept so `retry` resends the identical
    /// command id after a lost response rather than minting a new one.
    private var lastCommand: Components.Schemas.ClientCommand?

    public init(coordinator: SyncCoordinator) {
        self.coordinator = coordinator
    }

    /// Whether the composer may submit under `freshness`. A submission has no
    /// per-item version to validate, so unlike a decision's `actionsEnabled`
    /// it opens only on a settled sync round-trip (`.fresh`), the "validated
    /// current state" §5.14 requires.
    public static func canCompose(freshness: InboxStore.Freshness) -> Bool {
        freshness == .fresh
    }

    /// The coordinator's current sync freshness, observed so the composer can
    /// keep its submit action gated on `.fresh` for the sheet's whole lifetime,
    /// not only when it opens: a heartbeat or foreground refresh can degrade
    /// freshness while the operator composes, and a stale-epoch submit would
    /// carry an out-of-date project selection.
    public var freshness: InboxStore.Freshness { coordinator.store.freshness }

    public var pendingSubmissions: [Components.Schemas.ClientCommand] {
        coordinator.pendingTaskSubmissions.values.sorted { $0.command_id < $1.command_id }
    }

    /// Restoring only selects saved input. It never starts a network request.
    public func selectPending(_ commandID: String) -> Components.Schemas.SubmitTaskPayload? {
        guard let command = coordinator.pendingTaskSubmissions[commandID],
            case .submit_task(let payload) = command.payload
        else { return nil }
        lastCommand = command
        state = .lost
        return payload
    }

    /// Submits `source` under `projectID` with an optional operator name and
    /// returns the submitted task id, or nil on failure (state carries
    /// the outcome). Each call mints a fresh command id; `retry` reuses the
    /// last one. A submit_task command carries no decision envelope.
    @discardableResult
    public func submit(projectID: String, source: String, name: String? = nil) async -> String? {
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString,
            device_id: coordinator.store.device.deviceID,
            payload: .submit_task(
                .init(kind: .submit_task, project_id: projectID, source: source, name: name))
        )
        lastCommand = command
        return await send(command)
    }

    /// Resends the last built command with its original command id, so a lost
    /// response resolves to the one task the daemon already converged on. A
    /// no-op with nothing yet submitted.
    @discardableResult
    public func retry() async -> String? {
        guard let command = lastCommand else { return nil }
        return await send(command)
    }

    private func send(_ command: Components.Schemas.ClientCommand) async -> String? {
        guard coordinator.retainTaskSubmission(command) else {
            state = .rejected(
                "The submission could not be saved. Nothing was sent. Try again when storage is available.")
            return nil
        }
        state = .submitting
        do {
            let output = try await coordinator.store.client.submitCommand(body: .json(command))
            switch output {
            case .ok(let ok):
                let result = try ok.body.json
                // The result is untrusted until the trust gate confirms it is
                // the record for exactly this submission (matching source
                // digest). A refused result leaves acceptance uncertain.
                guard CommandResultTrust.accepts(result, for: command),
                    case .submit_task(let record) = result.record
                else {
                    state = .lost
                    return nil
                }
                // Read-your-write: the task and its run must be visible before
                // the caller routes to the task detail. `refreshAfterCommit`
                // guarantees a sync round whose first daemon read is issued
                // after this committed submission, so one await observes the
                // task even if a refresh begun before the commit was in flight.
                await coordinator.refreshAfterCommit()
                coordinator.finishTaskSubmission(command.command_id)
                state = .submitted(taskID: record.task_id)
                return record.task_id
            case .conflict:
                coordinator.finishTaskSubmission(command.command_id)
                // A submit_task carries no expected version, so the daemon
                // has no basis to reject it as stale; treat the documented
                // 409 as an authoritative rejection all the same, failing
                // closed rather than offering a retry that would repeat.
                state = .rejected("the daemon rejected the submission as out of date")
                return nil
            case .undocumented(let statusCode, _):
                if (400..<500).contains(statusCode), statusCode != 401, statusCode != 403 {
                    coordinator.finishTaskSubmission(command.command_id)
                }
                switch statusCode {
                case 401, 403:
                    // Authentication can fail before an accepted command's
                    // replay is reached. Keep its saved identity unresolved;
                    // this response proves nothing about the earlier outcome.
                    coordinator.store.freshness = .unauthenticated
                    state = .lost
                case 400..<500:
                    state = .rejected("the daemon rejected the submission (status \(statusCode))")
                default:
                    // A 5xx proves nothing: the command may have committed
                    // with its response lost, so offer the idempotent retry.
                    state = .lost
                }
                return nil
            }
        } catch {
            // No status came back (a transport-level failure or an unreadable
            // body): ambiguous like a 5xx, so the same command is retryable.
            state = .lost
            return nil
        }
    }
}
