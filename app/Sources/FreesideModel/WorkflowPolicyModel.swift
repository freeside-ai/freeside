import Foundation
import FreesideAPI
import Observation

/// A workflow policy change saved before it was sent: the exact command an
/// explicit Retry resends, and its receipt once the daemon accepted it.
/// Local delivery evidence, never an authority for the current layers, as
/// `PendingRoutingPolicyChange` is.
public struct PendingWorkflowPolicyChange: Codable, Equatable, Sendable {
    public let command: Components.Schemas.ClientCommand
    /// The epoch the change was made in. A receipt from another epoch can't
    /// be compared with the layers sync shows.
    public let syncEpoch: String
    public var receipt: Components.Schemas.CommandResult?

    func isValid(deviceID: String) -> Bool {
        guard case .set_workflow_policy = command.payload else { return false }
        if let receipt, !CommandResultTrust.accepts(receipt, for: command) { return false }
        return !command.command_id.isEmpty && !deviceID.isEmpty && command.device_id == deviceID
            && !syncEpoch.isEmpty && command.expected_entity_version == nil && command.expected_bindings == nil
    }
}

/// A workflow policy change the daemon refused. A stale expected revision
/// carries the layers the daemon held instead.
package struct WorkflowPolicyFailure: Equatable {
    package let message: String
    package var currentPolicy: Components.Schemas.WorkflowPolicySnapshot? = nil
}

/// One section of the Workflow pane: the general layer every project falls
/// back to, or one served project's layer. The pane stacks them; the daemon
/// serves one repository today, so there is no picker.
package enum WorkflowPolicySection: Hashable {
    case general
    case project(String)

    package var projectID: String? {
        if case .project(let id) = self { id } else { nil }
    }
}

/// Reads the workflow policy layers sync adopted and sends the operator's
/// change to one layer of one key as a saved command, the way
/// `RoutingPolicyModel` sends a routing change: the command persists before
/// it is sent, an unanswered send is retried only by an explicit Retry that
/// resends it unchanged, and the change reads as recorded only once sync
/// shows its revision. Each control saves at once, so there is no draft.
@MainActor @Observable
package final class WorkflowPolicyModel {
    private let coordinator: SyncCoordinator

    package init(coordinator: SyncCoordinator) { self.coordinator = coordinator }

    package var sending: Bool { coordinator.workflowPolicySending }
    package var failure: WorkflowPolicyFailure? {
        get { coordinator.workflowPolicyFailure }
        set { coordinator.workflowPolicyFailure = newValue }
    }

    package var policy: Components.Schemas.WorkflowPolicySnapshot? { coordinator.workflowPolicy }
    package var pending: PendingWorkflowPolicyChange? { coordinator.pendingWorkflowPolicyChange }
    /// The change sync confirmed this session, while its revision is still
    /// current; a later change from anywhere replaces it.
    package var confirmed: Components.Schemas.WorkflowPolicyChangeRecord? {
        coordinator.confirmedWorkflowPolicyChange.flatMap { $0.revision == policy?.revision ? $0 : nil }
    }

    /// General, then each project the daemon serves, in sync order.
    package var sections: [WorkflowPolicySection] {
        [.general] + (policy?.projects.map { .project($0.project_id) } ?? [])
    }

    /// The section's effective settings in sync order; empty before sync
    /// holds the layers.
    package func settings(in section: WorkflowPolicySection) -> [Components.Schemas.WorkflowPolicySetting] {
        switch section {
        case .general: policy?.general ?? []
        case .project(let id): policy?.projects.first { $0.project_id == id }?.settings ?? []
        }
    }

    /// Whether Reset can return the row to general: only a project row
    /// whose own layer sets the key.
    package static func canReset(
        _ setting: Components.Schemas.WorkflowPolicySetting, in section: WorkflowPolicySection
    )
        -> Bool
    {
        // An unknown key is one this app can neither set nor clear.
        section.projectID != nil && setting.origin == .project && setting.key != .unknown
    }

    /// Why the layers can't be changed now, or nil when they can.
    package var unavailableReason: String? {
        switch coordinator.store.freshness {
        case .fresh, .unvalidated:
            return coordinator.cursors == nil || policy == nil
                ? "Refresh before changing the workflow policy." : nil
        case .unauthenticated: return "Pair this device again before changing the workflow policy."
        case .unreachable: return "Offline. Connect to the daemon before changing the workflow policy."
        case .syncFailing: return "The workflow policy isn't current. Refresh before changing it."
        case .incompatible: return "Update the app or the daemon before changing the workflow policy."
        }
    }

    package var canEdit: Bool { unavailableReason == nil && pending == nil && !sending }

    /// Sets the section's layer of the key to value, saved at once.
    package func set(
        _ key: Components.Schemas.WorkflowPolicyKey, to value: String, in section: WorkflowPolicySection
    )
        async
    {
        guard key.accepts(value) else { return }
        await save(key: key, value: value, in: section)
    }

    /// Clears the project's layer of the key, so it takes general's value,
    /// saved at once.
    package func resetToGeneral(_ key: Components.Schemas.WorkflowPolicyKey, projectID: String) async {
        await save(key: key, value: nil, in: .project(projectID))
    }

    private func save(
        key: Components.Schemas.WorkflowPolicyKey, value: String?, in section: WorkflowPolicySection
    )
        async
    {
        guard canEdit, let policy, let epoch = coordinator.cursors?.syncEpoch else { return }
        failure = nil
        coordinator.clearConfirmedWorkflowPolicyChange()
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString, device_id: coordinator.store.device.deviceID,
            payload: .set_workflow_policy(
                .init(
                    kind: .set_workflow_policy, scope: section.projectID == nil ? .general : .project,
                    project_id: section.projectID, key: key, value: value,
                    // The schema's minimum is 1: a daemon that never
                    // recorded a change expects none.
                    expected_revision: policy.revision == 0 ? nil : policy.revision)))
        let entry = PendingWorkflowPolicyChange(command: command, syncEpoch: epoch)
        guard coordinator.retainWorkflowPolicyChange(entry) else {
            failure = WorkflowPolicyFailure(
                message: "The change couldn't be saved on this device. Nothing was sent. Try again.")
            return
        }
        await send(entry)
    }

    /// Explicit retry only: resends the saved command unchanged, and only in
    /// the epoch it was made in, where its expected revision still means
    /// what the operator saw (sync settles it in any other).
    package func retry() async {
        guard let entry = pending, entry.receipt == nil, entry.syncEpoch == coordinator.cursors?.syncEpoch,
            coordinator.store.freshness != .unauthenticated
        else { return }
        await send(entry)
    }

    private func send(_ entry: PendingWorkflowPolicyChange) async {
        guard !sending else { return }
        coordinator.workflowPolicySending = true
        failure = nil
        defer { coordinator.workflowPolicySending = false }
        do {
            switch try await coordinator.store.client.submitCommand(body: .json(entry.command)) {
            case .ok(let ok):
                let receipt = try ok.body.json
                guard CommandResultTrust.accepts(receipt, for: entry.command) else { return }
                var accepted = entry
                accepted.receipt = receipt
                // If this save fails, the unanswered command stays retryable.
                _ = coordinator.retainWorkflowPolicyChange(accepted)
                await coordinator.refreshAfterCommit()
                coordinator.settleWorkflowPolicyChange()
            case .conflict(let conflict):
                guard case .WorkflowPolicyRejection(let rejection) = try conflict.body.json,
                    case .set_workflow_policy(let payload) = entry.command.payload,
                    rejection.current_workflow_policy.revision != (payload.expected_revision ?? 0)
                else { return }
                // Show the layers the refusal carries, but adopt state only
                // through sync.
                coordinator.finishWorkflowPolicyChange()
                failure = WorkflowPolicyFailure(
                    message: "The workflow policy changed before your change reached the daemon. Nothing was changed.",
                    currentPolicy: rejection.current_workflow_policy)
                await coordinator.refreshAfterCommit()
            case .badRequest(let refusal):
                // The daemon refused the command before accepting it.
                coordinator.finishWorkflowPolicyChange()
                failure = WorkflowPolicyFailure(
                    message: CommandRefusal.message(
                        refusal, otherwise: "The daemon rejected the change (status 400). Nothing was changed."))
                await coordinator.refreshAfterCommit()
            case .undocumented(let status, _):
                switch status {
                case 404:
                    coordinator.finishWorkflowPolicyChange()
                    failure = WorkflowPolicyFailure(
                        message: "The daemon rejected the change (status \(status)). Nothing was changed.")
                    await coordinator.refreshAfterCommit()
                case 401, 403: coordinator.store.publish(.unauthenticated)
                default: break
                }
            }
        } catch {
            // A missing or malformed answer leaves the saved command unresolved.
        }
    }
}
