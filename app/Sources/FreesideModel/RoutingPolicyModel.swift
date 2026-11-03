import Foundation
import FreesideAPI
import Observation

/// A routing policy change saved before it was sent: the exact command an
/// explicit Retry resends, and its receipt once the daemon accepted it. Local
/// delivery evidence, never an authority for the current policy.
public struct PendingRoutingPolicyChange: Codable, Equatable, Sendable {
    public let command: Components.Schemas.ClientCommand
    /// The epoch the change was made in. A receipt from another epoch can't
    /// be compared with the policy sync shows.
    public let syncEpoch: String
    public var receipt: Components.Schemas.CommandResult?

    func isValid(deviceID: String) -> Bool {
        guard case .set_routing_policy = command.payload else { return false }
        if let receipt, !CommandResultTrust.accepts(receipt, for: command) { return false }
        return !command.command_id.isEmpty && !deviceID.isEmpty && command.device_id == deviceID
            && !syncEpoch.isEmpty && command.expected_entity_version == nil && command.expected_bindings == nil
    }
}

/// A routing policy change the daemon refused. A stale expected revision
/// carries the policy the daemon held instead.
package struct RoutingPolicyFailure: Equatable {
    package let message: String
    package var currentPolicy: Components.Schemas.RoutingPolicy? = nil
}

/// The operator's edit of the routing policy, before it is saved: one agent
/// order per role, and the revision and epoch it started from. The command
/// names that revision as its expected revision, which means nothing in
/// another epoch, and carries that revision's drain-first account.
public struct RoutingPolicyDraft: Equatable, Sendable {
    public typealias Role = Components.Schemas.TaskLineRole

    /// The revision the edit started from; nil when no policy was set.
    public let baseRevision: Int?
    /// The base revision's drain-first account. The sheet edits only the
    /// orders, and a command without an account clears it, so a save
    /// resends the one it started from.
    public let drainFirstAccount: String?
    public let syncEpoch: String
    private var orders: [Role: [String]]

    /// The schema promises one order per role and no repeated agent, but
    /// the policy comes from the daemon or the cache: the first of a
    /// repeated role or agent wins rather than trapping.
    public init(policy: Components.Schemas.RoutingPolicy?, syncEpoch: String) {
        baseRevision = policy?.revision
        drainFirstAccount = policy?.drain_first_account
        self.syncEpoch = syncEpoch
        var orders: [Role: [String]] = [:]
        for role in policy?.roles ?? [] where orders[role.role] == nil {
            var seen = Set<String>()
            orders[role.role] = role.agents.filter { seen.insert($0).inserted }
        }
        self.orders = orders
    }

    public func agents(for role: Role) -> [String] { orders[role] ?? [] }

    /// The catalog's agents the role doesn't already order, in catalog order.
    public func offered(for role: Role, catalog: Components.Schemas.AgentCatalog) -> [String] {
        let ordered = Set(agents(for: role))
        return catalog.agents.map(\.name).filter { !ordered.contains($0) }
    }

    public mutating func add(_ agent: String, to role: Role) {
        guard !agents(for: role).contains(agent) else { return }
        orders[role, default: []].append(agent)
    }

    /// Removing a role's last agent leaves the role to the lineup.
    public mutating func remove(_ agent: String, from role: Role) {
        orders[role]?.removeAll { $0 == agent }
        if orders[role]?.isEmpty == true { orders[role] = nil }
    }

    /// Moves the agent one place earlier (`offset` -1) or later (+1).
    public mutating func move(_ agent: String, in role: Role, by offset: Int) {
        guard var list = orders[role], let index = list.firstIndex(of: agent),
            list.indices.contains(index + offset)
        else { return }
        list.swapAt(index, index + offset)
        orders[role] = list
    }

    /// The orders as the command sends them: roles with an agent, in
    /// TaskLineRole order, the order the policy records them in.
    public var roles: [Components.Schemas.RoutingPolicyRole] {
        Role.allCases.compactMap { role in
            orders[role].map { .init(role: role, agents: $0) }
        }
    }
}

/// Reads the routing policy sync adopted and sends the operator's change as a
/// saved command (plan §5.4, routing policy), modeled on `TaskStopModel`: the
/// command persists before it is sent, an unanswered send is retried only by
/// an explicit Retry that resends it unchanged, and the change reads as
/// recorded only once sync shows its revision. The view owns it, as it owns
/// `DevicesModel`, so only the draft ends with the sheet: the saved command,
/// whether it is being sent, and its refusal live in the coordinator, where a
/// reopened sheet finds them.
@MainActor @Observable
package final class RoutingPolicyModel {
    private let coordinator: SyncCoordinator
    package var draft: RoutingPolicyDraft?

    package init(coordinator: SyncCoordinator) { self.coordinator = coordinator }

    package var sending: Bool { coordinator.routingPolicySending }
    package var failure: RoutingPolicyFailure? {
        get { coordinator.routingPolicyFailure }
        set { coordinator.routingPolicyFailure = newValue }
    }

    package var policy: Components.Schemas.RoutingPolicy? { coordinator.routingPolicy?.policy }
    package var catalog: Components.Schemas.AgentCatalog? { coordinator.agentCatalog }
    package var pending: PendingRoutingPolicyChange? { coordinator.pendingRoutingPolicyChange }
    /// The change sync confirmed this session, while it is still the policy;
    /// a later revision from anywhere replaces it.
    package var confirmed: Components.Schemas.RoutingPolicyChangeRecord? {
        coordinator.confirmedRoutingPolicyChange.flatMap {
            $0.policy.revision == policy?.revision ? $0 : nil
        }
    }
    package var currentDeviceID: String { coordinator.store.device.deviceID }

    /// The agent the lineup runs the role with when the policy leaves it out.
    package func lineupAgent(for role: RoutingPolicyDraft.Role) -> String? {
        catalog?.lineup.first { $0.role == role }?.agent
    }

    /// Why the policy can't be changed now, or nil when it can.
    package var unavailableReason: String? {
        guard catalog != nil else {
            return "The daemon has no agent tree, so the routing policy can't be changed."
        }
        switch coordinator.store.freshness {
        case .fresh, .unvalidated:
            return coordinator.cursors == nil ? "Refresh before changing the routing policy." : nil
        case .unauthenticated: return "Pair this device again before changing the routing policy."
        case .unreachable: return "Offline. Connect to the daemon before changing the routing policy."
        case .syncFailing, .contractMismatch: return "The routing policy isn't current. Refresh before changing it."
        }
    }

    package var canEdit: Bool { unavailableReason == nil && pending == nil && draft == nil && !sending }

    /// Why the draft can't be saved, or nil when it can.
    package var saveBlocker: String? {
        guard let draft, let catalog else { return nil }
        if draft.baseRevision != policy?.revision || draft.syncEpoch != coordinator.cursors?.syncEpoch {
            return "The policy changed while you were editing. Cancel and edit it again."
        }
        let known = Set(catalog.agents.map(\.name))
        if draft.roles.contains(where: { !$0.agents.allSatisfy(known.contains) }) {
            return "Remove the agents the daemon's agent tree doesn't carry."
        }
        if draft.roles.isEmpty { return "Order at least one agent for a role." }
        if draft.roles == policy?.roles { return "No change to save." }
        return nil
    }

    package func beginEditing() {
        guard canEdit, let epoch = coordinator.cursors?.syncEpoch else { return }
        failure = nil
        coordinator.clearConfirmedRoutingPolicyChange()
        draft = RoutingPolicyDraft(policy: policy, syncEpoch: epoch)
    }

    package func cancelEditing() { draft = nil }

    package func save() async {
        guard let draft, saveBlocker == nil, unavailableReason == nil, pending == nil, !sending else { return }
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString, device_id: coordinator.store.device.deviceID,
            payload: .set_routing_policy(
                .init(
                    kind: .set_routing_policy, roles: draft.roles, drain_first_account: draft.drainFirstAccount,
                    expected_revision: draft.baseRevision)))
        let entry = PendingRoutingPolicyChange(command: command, syncEpoch: draft.syncEpoch)
        guard coordinator.retainRoutingPolicyChange(entry) else {
            failure = RoutingPolicyFailure(
                message: "The change couldn't be saved on this device. Nothing was sent. Try again.")
            return
        }
        self.draft = nil
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

    private func send(_ entry: PendingRoutingPolicyChange) async {
        guard !sending else { return }
        coordinator.routingPolicySending = true
        failure = nil
        defer { coordinator.routingPolicySending = false }
        do {
            switch try await coordinator.store.client.submitCommand(body: .json(entry.command)) {
            case .ok(let ok):
                let receipt = try ok.body.json
                guard CommandResultTrust.accepts(receipt, for: entry.command) else { return }
                var accepted = entry
                accepted.receipt = receipt
                // If this save fails, the unanswered command stays retryable.
                _ = coordinator.retainRoutingPolicyChange(accepted)
                await coordinator.refreshAfterCommit()
                coordinator.settleRoutingPolicyChange()
            case .conflict(let conflict):
                guard case .RoutingPolicyRejection(let rejection) = try conflict.body.json,
                    case .set_routing_policy(let payload) = entry.command.payload,
                    rejection.current_policy?.value1.revision != payload.expected_revision
                else { return }
                // Show the policy the refusal carries, but adopt state only
                // through sync.
                coordinator.finishRoutingPolicyChange()
                failure = RoutingPolicyFailure(
                    message: "The policy changed before your change reached the daemon. Nothing was changed.",
                    currentPolicy: rejection.current_policy?.value1)
                await coordinator.refreshAfterCommit()
            case .undocumented(let status, _):
                switch status {
                case 400, 404:
                    // The daemon refused the command before accepting it.
                    coordinator.finishRoutingPolicyChange()
                    failure = RoutingPolicyFailure(
                        message: "The daemon rejected the change (status \(status)). Nothing was changed.")
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
