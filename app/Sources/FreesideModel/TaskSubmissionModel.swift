import Foundation
import FreesideAPI
import Observation
import OpenAPIRuntime

/// Submits a task from source text (plan §5.11) and lands on it, driving the
/// New Task composer and the separate recovery screen. It builds a
/// submit_task ClientCommand with a fresh command id and no decision envelope,
/// accepts the returned result only
/// through the trust gate, then refreshes for read-your-write and reports the
/// submitted task id. Before a new submission is built, one sync round
/// issued after the tap must end `.fresh`, so the command is only ever sent
/// against validated current state (§5.14). A lost response keeps the built
/// command so
/// `retry` can resend the identical command id; the daemon converges a repeat
/// of the same command on one task. A daemon answer that authoritatively
/// rejects the submission is a `.rejected` typed reason, not a retry.
///
/// Attachments (#2590) are saved beside the command, then uploaded by digest
/// before the command is sent, on submit and on retry alike: the command
/// names only digests, so the daemon must hold every one first. A re-upload
/// of a stored digest is the daemon's no-op.
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
        /// Nothing was recorded and retry is not offered: the daemon answered
        /// authoritatively that it did not record the submission (a
        /// non-authentication 4xx), or the submission was stopped before
        /// sending (no validated state, or no durable retention). The reason
        /// is shown.
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

    /// Whether the composer may open and submit under `freshness`. A
    /// submission has no per-item version to validate, so it needs the
    /// "validated current state" §5.14 requires; `submit` gets that by
    /// running its own round before sending. The composer therefore stays
    /// available while the cache is merely `.unvalidated`, which a busy
    /// daemon makes the steady state (#1554), and closes only when sync is
    /// failing, where a validation round could not succeed.
    public static func canCompose(freshness: InboxStore.Freshness) -> Bool {
        composeBlockedReason(freshness: freshness) == nil
    }

    /// Why the composer is closed under `freshness`, or nil when it is open.
    /// Shown beside the disabled New Task action so the operator is not left
    /// guessing.
    public static func composeBlockedReason(freshness: InboxStore.Freshness) -> String? {
        switch freshness {
        case .fresh, .unvalidated:
            return nil
        case .unreachable:
            return "New tasks need the daemon, which is unreachable."
        case .syncFailing:
            return "New tasks need a working sync with the daemon."
        case .incompatible:
            return "New tasks need an app and daemon that can talk to each other."
        case .unauthenticated:
            return "New tasks need this device to be paired again."
        }
    }

    /// The coordinator's current sync freshness, observed so the composer can
    /// keep its submit action gated by `canCompose` for the sheet's whole
    /// lifetime, not only when it opens: sync can fail while the operator
    /// composes.
    public var freshness: InboxStore.Freshness { coordinator.store.freshness }
    public var promptHistory: TaskPromptHistory { coordinator.promptHistory }
    /// The agents the form offers, observed so a newer catalog reaches an
    /// open sheet; nil when the daemon holds no agent tree.
    public var agentCatalog: Components.Schemas.AgentCatalog? { coordinator.agentCatalog }
    /// The routing policy, which picks a role's agent when the operator
    /// chooses none and the policy lists the role.
    public var routingPolicy: Components.Schemas.RoutingPolicy? { coordinator.routingPolicy?.policy }
    /// The usage pools whose readings the agent choices show, observed so a
    /// new reading reaches an open sheet.
    public var usagePools: [Components.Schemas.UsagePoolSnapshot] { coordinator.usagePools }

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

    /// Submits `source` under `projectID` with an optional operator name,
    /// agent lines, and attachments, and returns the submitted task id, or
    /// nil on failure (state carries the outcome). Each call mints a fresh
    /// command id; `retry` reuses the last one, lines and attachments
    /// included. A submit_task command carries no decision envelope.
    /// `taskLines` is nil for no choice: the daemon rejects an empty list.
    @discardableResult
    public func submit(
        projectID: String, source: String, name: String? = nil,
        taskLines: [Components.Schemas.TaskLineInput]? = nil,
        attachments: TaskAttachmentDraft = TaskAttachmentDraft()
    ) async -> String? {
        state = .submitting
        // The operator composed against the cache as it stood, and a stale
        // epoch would carry an out-of-date project selection. A round whose
        // first read is issued after the tap validates that state; send only
        // if it ended `.fresh`.
        guard await coordinator.refreshAfterCommit() else {
            state = .rejected(
                "Freeside couldn't confirm the daemon's current state. Nothing was sent. Try again.")
            return nil
        }
        // The round runs in its own task, so cancelling the caller (the sheet
        // dismissed mid-validation) does not stop it. Honor the cancel here,
        // before anything is retained or sent.
        guard !Task.isCancelled else {
            state = .idle
            return nil
        }
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString,
            device_id: coordinator.store.device.deviceID,
            payload: .submit_task(
                .init(
                    kind: .submit_task, project_id: projectID, source: source, name: name, task_lines: taskLines,
                    attachments: attachments.inputs))
        )
        lastCommand = command
        return await send(command, attachmentBytes: attachments.bytesByDigest)
    }

    /// Resends the last built command with its original command id, so a lost
    /// response resolves to the one task the daemon already converged on. A
    /// no-op with nothing yet submitted.
    @discardableResult
    public func retry() async -> String? {
        guard let command = lastCommand else { return nil }
        return await send(command)
    }

    private func send(
        _ command: Components.Schemas.ClientCommand, attachmentBytes: [String: Data] = [:]
    ) async -> String? {
        guard coordinator.retainTaskSubmission(command, attachmentBytes: attachmentBytes) else {
            state = .rejected(
                "The submission could not be saved. Nothing was sent. Try again when storage is available.")
            return nil
        }
        state = .submitting
        do {
            guard try await uploadAttachments(of: command) else { return nil }
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
                if case .submit_task(let payload) = command.payload {
                    coordinator.promptHistory.record(
                        projectID: payload.project_id, source: payload.source, taskID: record.task_id)
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
            case .badRequest(let refusal):
                coordinator.finishTaskSubmission(command.command_id)
                state = .rejected(
                    CommandRefusal.message(refusal, otherwise: "the daemon rejected the submission (status 400)"))
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
                    coordinator.store.publish(.unauthenticated)
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

    /// Uploads, in order, the saved bytes of every attachment `command`
    /// names, and says whether the command may be sent. When it may not, it
    /// has set `state`: a 422 (the bytes no longer hash to the digest) or
    /// another non-authentication 4xx is final, so the submission finishes
    /// as rejected; authentication or a 5xx leaves it saved and lost. A
    /// throw is a transport failure, which the caller treats as lost. A
    /// digest with no saved bytes is skipped: the daemon answers the command
    /// with a 400 if it doesn't hold the digest, which already finishes the
    /// submission as rejected.
    private func uploadAttachments(of command: Components.Schemas.ClientCommand) async throws -> Bool {
        guard case .submit_task(let payload) = command.payload else { return true }
        for attachment in payload.attachments ?? [] {
            guard let bytes = coordinator.submissionAttachmentBytes(digest: attachment.digest) else { continue }
            let output = try await coordinator.store.client.uploadAttachment(
                path: .init(digest: attachment.digest), body: .binary(HTTPBody(bytes)))
            switch output {
            case .created, .ok:
                continue
            case .unprocessableContent:
                coordinator.finishTaskSubmission(command.command_id)
                state = .rejected("\(attachment.name) changed after it was saved, so nothing was submitted.")
                return false
            case .undocumented(let statusCode, _):
                switch statusCode {
                case 401, 403:
                    coordinator.store.publish(.unauthenticated)
                    state = .lost
                case 400..<500:
                    coordinator.finishTaskSubmission(command.command_id)
                    state = .rejected(
                        "the daemon refused \(attachment.name) (status \(statusCode)), so nothing was submitted")
                default:
                    state = .lost
                }
                return false
            }
        }
        return true
    }
}
