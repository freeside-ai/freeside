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

/// The operator's edit of the routing policy, before it is saved: one order
/// of enrollment ids per role, the drain-first account, and the revision and
/// epoch it started from. The command names that revision as its expected
/// revision, which means nothing in another epoch.
public struct RoutingPolicyDraft: Equatable, Sendable {
    public typealias Role = Components.Schemas.TaskLineRole

    /// The revision the edit started from; nil when no policy was set.
    public let baseRevision: Int?
    /// The usage pool to drain first, or nil for none. It starts as the base
    /// revision's: the command replaces the whole policy, so an edit of the
    /// orders alone resends it, and nil clears it.
    public var drainFirstAccount: String?
    public let syncEpoch: String
    private var orders: [Role: [String]]

    /// A role the policy orders by enrollment starts with those ids. A role
    /// stored before policies ordered enrollments (#2736) holds agent names,
    /// which order nothing since the tree dropped its agents (#2599); the
    /// draft starts it empty, so the operator's first save sends
    /// enrollments. The schema
    /// promises one order per role and no repeated entry, but the policy
    /// comes from the daemon or the cache: the first of a repeated role or
    /// entry wins rather than trapping.
    public init(policy: Components.Schemas.RoutingPolicy?, syncEpoch: String) {
        baseRevision = policy?.revision
        drainFirstAccount = policy?.drain_first_account
        self.syncEpoch = syncEpoch
        var orders: [Role: [String]] = [:]
        for role in policy?.roles ?? [] where orders[role.role] == nil {
            var seen = Set<String>()
            let unique = (role.enrollments ?? []).filter { seen.insert($0).inserted }
            if !unique.isEmpty { orders[role.role] = unique }
        }
        self.orders = orders
    }

    public func enrollments(for role: Role) -> [String] { orders[role] ?? [] }

    /// The catalog's enrollments the role doesn't already order, in catalog
    /// order. The daemon decides whether one runs the role's lineup model and
    /// effort; the app offers them all and shows its refusal.
    public func offered(for role: Role, catalog: Components.Schemas.AgentCatalog) -> [String] {
        let ordered = Set(enrollments(for: role))
        return catalog.enrollments.map(\.enrollment_id).filter { !ordered.contains($0) }
    }

    public mutating func add(_ enrollment: String, to role: Role) {
        guard !enrollments(for: role).contains(enrollment) else { return }
        orders[role, default: []].append(enrollment)
    }

    /// Removing a role's last enrollment leaves the role to the lineup.
    public mutating func remove(_ enrollment: String, from role: Role) {
        orders[role]?.removeAll { $0 == enrollment }
        if orders[role]?.isEmpty == true { orders[role] = nil }
    }

    /// Sets the role's whole order, keeping the first of a repeated
    /// enrollment. An empty list leaves the role to the lineup, as removing
    /// its last enrollment does.
    public mutating func replace(_ enrollments: [String], for role: Role) {
        var seen = Set<String>()
        let unique = enrollments.filter { seen.insert($0).inserted }
        orders[role] = unique.isEmpty ? nil : unique
    }

    /// Moves the enrollment one place earlier (`offset` -1) or later (+1).
    public mutating func move(_ enrollment: String, in role: Role, by offset: Int) {
        guard var list = orders[role], let index = list.firstIndex(of: enrollment),
            list.indices.contains(index + offset)
        else { return }
        list.swapAt(index, index + offset)
        orders[role] = list
    }

    /// The orders as the command sends them: roles with an enrollment, in
    /// TaskLineRole order, the order the policy records them in. A role a
    /// newer daemon added decodes as `unknown` and stays in the command, so
    /// the daemon refuses the save rather than the app silently dropping an
    /// order it can't show.
    public var roles: [Components.Schemas.RoutingPolicyRoleInput] {
        Role.allCases.compactMap { role in
            orders[role].map { .init(role: role, enrollments: $0) }
        }
    }

    /// The orders as a revision recording them would hold them, to compare
    /// with the current policy: an agents-form role never matches.
    var policyRoles: [Components.Schemas.RoutingPolicyRole] {
        roles.map { .init(role: $0.role, enrollments: $0.enrollments, agents: nil) }
    }

    /// Every enrollment some role orders.
    var orderedEnrollments: Set<String> { Set(orders.values.joined()) }
}

/// A usage pool the operator may drain first, with the ordered enrollments
/// on it.
package struct RoutingDrainFirstPool: Equatable {
    package let pool: Components.Schemas.UsagePoolUsage
    package let enrollments: [String]
}

/// One of the Agents pane's routing popups. Writer reads and sets the three
/// roles that write the change together, as the New Task Writer did;
/// Reviewer sets the reviewer.
package enum RoutingPopup: CaseIterable, Hashable {
    case writer
    case reviewer

    package var roles: [RoutingPolicyDraft.Role] {
        switch self {
        case .writer: AgentChoiceDisplay.writerRoles
        case .reviewer: [.reviewer]
        }
    }

    package var title: String {
        switch self {
        case .writer: "Writer"
        case .reviewer: "Reviewer"
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

    /// Why the last popup choice sent nothing, until the next choice or
    /// edit.
    package private(set) var choiceBlocker: String?

    package init(coordinator: SyncCoordinator) { self.coordinator = coordinator }

    package var sending: Bool { coordinator.routingPolicySending }
    package var failure: RoutingPolicyFailure? {
        get { coordinator.routingPolicyFailure }
        set { coordinator.routingPolicyFailure = newValue }
    }

    package var policy: Components.Schemas.RoutingPolicy? { coordinator.routingPolicy?.policy }
    /// The synced catalog when the daemon holds an agent tree, which is what
    /// a policy orders; nil without one, so the policy reads but can't change.
    package var catalog: Components.Schemas.AgentCatalog? {
        coordinator.agentCatalog.flatMap { $0.lineup_revision == nil ? nil : $0 }
    }
    package var pending: PendingRoutingPolicyChange? { coordinator.pendingRoutingPolicyChange }
    /// The change sync confirmed this session, while it is still the policy;
    /// a later revision from anywhere replaces it.
    package var confirmed: Components.Schemas.RoutingPolicyChangeRecord? {
        coordinator.confirmedRoutingPolicyChange.flatMap {
            $0.policy.revision == policy?.revision ? $0 : nil
        }
    }
    package var currentDeviceID: String { coordinator.store.device.deviceID }

    /// The pools the operator may drain first, in sync order: those an
    /// enrollment the draft orders sits on, or, with no draft open, one the
    /// current policy orders.
    package var drainFirstPools: [RoutingDrainFirstPool] {
        guard catalog != nil else { return [] }
        let orders = draft ?? RoutingPolicyDraft(policy: policy, syncEpoch: "")
        return Self.drainFirstPools(coordinator.usagePools, ordered: orders.orderedEnrollments)
    }

    /// The synced usage pool with this id, whether or not it can be drained
    /// first, so the view can name a stale account's provider and plan.
    package func usagePool(_ id: String) -> Components.Schemas.UsagePoolUsage? {
        coordinator.usagePools.first { $0.pool.id == id }?.pool
    }

    /// The pools at least one ordered enrollment sits on, each with those
    /// enrollments. It mirrors the daemon's rule (`checkDrainFirstAccount`
    /// in daemon/internal/engine/routing_policy.go), which refuses any other
    /// pool: the daemon resolves each ordered enrollment to its identity's
    /// pool, and sync lists every pool's enrollments.
    package static func drainFirstPools(
        _ pools: [Components.Schemas.UsagePoolSnapshot], ordered: Set<String>
    ) -> [RoutingDrainFirstPool] {
        pools.compactMap { snapshot in
            let enrollments = snapshot.pool.enrollments.map(\.enrollment_id).filter(ordered.contains)
            return enrollments.isEmpty ? nil : RoutingDrainFirstPool(pool: snapshot.pool, enrollments: enrollments)
        }
    }

    /// The role's order as the policy stores it: its enrollment ids, or the
    /// agent names a revision from before #2736 recorded; `[]` when the
    /// policy leaves the role to the lineup.
    package func storedOrder(for role: RoutingPolicyDraft.Role) -> [String] {
        policy?.roles.first { $0.role == role }.map { $0.enrollments ?? $0.agents ?? [] } ?? []
    }

    /// The one order every role of the popup has in the policy, as stored;
    /// `[]` when all of them are on the lineup, nil when they differ.
    package func sharedOrder(_ popup: RoutingPopup) -> [String]? {
        let orders = popup.roles.map(storedOrder(for:))
        return orders.allSatisfy { $0 == orders.first } ? orders.first : nil
    }

    /// The selection the lineup runs the role with when the policy leaves it
    /// out; nil without a catalog or when the lineup binds nothing to it.
    package func lineupSelection(for role: RoutingPolicyDraft.Role) -> Components.Schemas.TaskLineSelection? {
        catalog?.lineup.first { $0.role == role }?.selection
    }

    /// The one lineup selection every role of the popup sits on, nil when
    /// they differ or any role has none. Roles compare by selection because
    /// a lineup line names no agent.
    package func sharedLineupSelection(_ popup: RoutingPopup) -> Components.Schemas.TaskLineSelection? {
        let selections = popup.roles.map(lineupSelection(for:))
        return Set(selections).count == 1 ? selections.first ?? nil : nil
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
        let held = Set(catalog.enrollments.map(\.enrollment_id))
        if draft.roles.contains(where: { !$0.enrollments.allSatisfy(held.contains) }) {
            return "Remove the enrollments the daemon doesn't hold."
        }
        if let account = draft.drainFirstAccount, !drainFirstPools.contains(where: { $0.pool.id == account }) {
            return "No ordered enrollment is on \(account). Clear the drain-first account or pick another."
        }
        // The schema requires a role, so an account alone can't make a
        // first policy.
        if draft.roles.isEmpty { return "Order at least one enrollment for a role." }
        if draft.policyRoles == policy?.roles && draft.drainFirstAccount == policy?.drain_first_account {
            return "No change to save."
        }
        return nil
    }

    package func beginEditing() {
        guard let fresh = freshDraft() else { return }
        draft = fresh
    }

    /// A popup's choice: the enrollment first, then the order the popup's
    /// roles shared, or the enrollment alone when they differed. It saves at
    /// once.
    package func choose(_ enrollment: String, for popup: RoutingPopup) async {
        await saveChoice { draft in
            let orders = popup.roles.map(draft.enrollments(for:))
            let shared = orders.allSatisfy { $0 == orders.first } ? orders.first ?? [] : []
            let rest = shared.filter { $0 != enrollment }
            for role in popup.roles { draft.replace([enrollment] + rest, for: role) }
        }
    }

    /// The drain-first popup's choice, nil for None, saved at once.
    package func chooseDrainFirst(_ account: String?) async {
        await saveChoice { $0.drainFirstAccount = account }
    }

    /// A choice is a one-shot save: a fresh draft from the current policy
    /// with the change applied, checked as Save checks the editor's. A
    /// refused choice sends nothing and leaves no draft, so the editor
    /// doesn't open; its reason stays in `choiceBlocker`.
    private func saveChoice(_ change: (inout RoutingPolicyDraft) -> Void) async {
        guard var fresh = freshDraft() else { return }
        change(&fresh)
        draft = fresh
        if let blocker = saveBlocker {
            draft = nil
            choiceBlocker = blocker
            return
        }
        await save()
        // Save keeps the draft only when the command couldn't be stored on
        // this device; a choice has no editor to return to, so its failure
        // notice stands alone.
        draft = nil
    }

    /// A draft from the current policy, clearing the last refusal, choice
    /// blocker, and confirmation; nil when the policy can't be changed now.
    private func freshDraft() -> RoutingPolicyDraft? {
        guard canEdit, let epoch = coordinator.cursors?.syncEpoch else { return nil }
        failure = nil
        choiceBlocker = nil
        coordinator.clearConfirmedRoutingPolicyChange()
        return RoutingPolicyDraft(policy: policy, syncEpoch: epoch)
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
            case .badRequest(let refusal):
                // The daemon refused the command before accepting it.
                coordinator.finishRoutingPolicyChange()
                failure = RoutingPolicyFailure(
                    message: CommandRefusal.message(
                        refusal, otherwise: "The daemon rejected the change (status 400). Nothing was changed."))
                await coordinator.refreshAfterCommit()
            case .undocumented(let status, _):
                switch status {
                case 404:
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
