import FreesideAPI
import SwiftUI

/// The routing policy (plan §5.4): which agent takes a new task for each role
/// when the operator chose none, read from sync and changed as a saved
/// command. `SettingsSheet` hosts it on both platforms, as it hosts Devices.
/// It edits only the order among agents the daemon's catalog lists; the
/// lineup itself still changes through the committed agent tree.
struct RoutingPolicyView: View {
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    let model: RoutingPolicyModel
    /// Paired device names by device id, so a policy a client set names
    /// that device rather than its id.
    let deviceNames: [String: String]

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            FreesideSheetHeader(
                eyebrow: "Routing",
                ask: "Which agent takes new tasks",
                // Plan revision 107: the order is the router's candidate set,
                // ranked by the soonest weekly reset, and the order itself
                // decides only where no reset is known or resets tie.
                consequence:
                    "A new task goes to the role's agent whose weekly usage resets soonest, or to the first in this order when no reset is known. Used-up agents are skipped, and running tasks stay where they are.",
                askLineLimit: nil)
            VStack(alignment: .leading, spacing: 16) {
                notices
                if let draft = model.draft {
                    editor(draft)
                } else {
                    reading
                }
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
        }
    }

    // MARK: Notices

    @ViewBuilder private var notices: some View {
        if let failure = model.failure {
            VStack(alignment: .leading, spacing: 8) {
                Notice(tone: .wax, keyword: "Failed", sentence: failure.message)
                    .accessibilityElement(children: .combine)
                if let current = failure.currentPolicy {
                    Text(Self.policyLine(current))
                        .font(FreesideFont.monoValue)
                        .foregroundStyle(Color.inkDim)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityLabel("The daemon holds \(Self.policyLine(current))")
                }
            }
        }
        if let pending = model.pending {
            if pending.receipt == nil {
                Notice(
                    tone: .accent, keyword: "Unconfirmed",
                    sentence: model.sending
                        ? "Sending the change…"
                        : "The daemon hasn't answered. Retry sends the same change to recover its result.",
                    action: .init(
                        label: "Retry", accessibilityLabel: "Retry sending the routing change",
                        isEnabled: !model.sending
                    ) { Task { await model.retry() } })
            } else {
                Notice(tone: .neutral, keyword: "Accepted", sentence: "Waiting for sync to show the change.")
                    .accessibilityElement(children: .combine)
            }
        } else if let confirmed = model.confirmed {
            Notice(
                tone: .neutral, keyword: "Recorded",
                sentence: confirmed.recorded
                    ? "Revision \(confirmed.policy.revision) is the policy now."
                    : "Revision \(confirmed.policy.revision) already held this order."
            )
            .accessibilityElement(children: .combine)
        }
    }

    // MARK: Reading

    @ViewBuilder private var reading: some View {
        facts
        ForEach(RoutingPolicyDraft.Role.allCases, id: \.self) { role in
            roleCard(role) {
                let agents = model.policy?.roles.first { $0.role == role }?.agents ?? []
                if agents.isEmpty {
                    lineupLine(role)
                } else {
                    ForEach(Array(agents.enumerated()), id: \.offset) { index, agent in
                        orderedAgent(index: index, name: agent)
                    }
                }
            }
        }
        if let reason = model.unavailableReason {
            Text(reason)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
        } else {
            Button("Change Order") { model.beginEditing() }
                .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
                .disabled(!model.canEdit)
        }
    }

    @ViewBuilder private var facts: some View {
        if let policy = model.policy {
            VStack(alignment: .leading, spacing: 8) {
                FactRow(label: "Revision", value: "\(policy.revision)")
                FactRow(label: "Set by", value: Self.setBy(policy, deviceNames: deviceNames))
                FactRow(
                    label: "Set",
                    value: FreesideFormat.shortTime(
                        policy.set_at, now: pinnedNow ?? .now, locale: locale, timeZone: timeZone)
                )
                .exactInstant(policy.set_at)
            }
            .accessibilityElement(children: .combine)
        } else {
            Text("No policy. Every role uses its lineup agent.")
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
        }
    }

    // MARK: Editing

    @ViewBuilder private func editor(_ draft: RoutingPolicyDraft) -> some View {
        ForEach(RoutingPolicyDraft.Role.allCases, id: \.self) { role in
            roleCard(role) {
                let agents = draft.agents(for: role)
                if agents.isEmpty { lineupLine(role) }
                ForEach(Array(agents.enumerated()), id: \.element) { index, agent in
                    editableAgent(agent, role: role, index: index, count: agents.count)
                }
                if let catalog = model.catalog {
                    let offered = draft.offered(for: role, catalog: catalog)
                    if !offered.isEmpty {
                        WrappingHStack(horizontalSpacing: 6, verticalSpacing: 6) {
                            ForEach(offered, id: \.self) { agent in
                                Button("+ \(agent)") { model.draft?.add(agent, to: role) }
                                    .buttonStyle(FreesideActionButtonStyle(tone: .tertiary))
                                    .accessibilityLabel("Add \(agent) to \(Self.roleName(role))")
                            }
                        }
                    }
                }
            }
        }
        if let blocker = model.saveBlocker {
            Text(blocker)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
        }
        HStack(spacing: 8) {
            Button("Cancel") { model.cancelEditing() }
                .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
            Button("Save") { Task { await model.save() } }
                .buttonStyle(FreesideActionButtonStyle(tone: .primary))
                .disabled(model.saveBlocker != nil || model.sending)
        }
    }

    private func editableAgent(_ agent: String, role: RoutingPolicyDraft.Role, index: Int, count: Int) -> some View {
        let roleName = Self.roleName(role)
        let known = model.catalog?.agents.contains { $0.name == agent } ?? false
        return HStack(spacing: 6) {
            orderedAgent(index: index, name: agent, known: known)
            Spacer(minLength: 0)
            iconButton("↑", label: "Move \(agent) up in \(roleName)", enabled: index > 0) {
                model.draft?.move(agent, in: role, by: -1)
            }
            iconButton("↓", label: "Move \(agent) down in \(roleName)", enabled: index < count - 1) {
                model.draft?.move(agent, in: role, by: 1)
            }
            iconButton("✕", label: "Remove \(agent) from \(roleName)", enabled: true) {
                model.draft?.remove(agent, from: role)
            }
        }
    }

    private func iconButton(
        _ glyph: String, label: String, enabled: Bool, action: @escaping () -> Void
    ) -> some View {
        Button(action: action) {
            Text(glyph)
                .font(FreesideFont.noticeAction)
                .foregroundStyle(enabled ? Color.accentText : Color.inkDim)
                .frame(minWidth: 28, minHeight: 28)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .disabled(!enabled)
        .accessibilityLabel(label)
        .help(label)
    }

    // MARK: Shared pieces

    private func roleCard(
        _ role: RoutingPolicyDraft.Role, @ViewBuilder content: () -> some View
    ) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            KeywordLabel(text: Self.roleName(role))
                .accessibilityAddTraits(.isHeader)
            content()
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
    }

    private func orderedAgent(index: Int, name: String, known: Bool = true) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text("\(index + 1).")
                .font(FreesideFont.monoValue)
                .foregroundStyle(Color.inkDim)
            Text(name)
                .font(FreesideFont.monoValue)
                .foregroundStyle(Color.ink)
            if !known {
                Text("not in the agent tree")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.waxText)
            }
        }
        .accessibilityElement(children: .combine)
    }

    private func lineupLine(_ role: RoutingPolicyDraft.Role) -> some View {
        Text(Self.lineupText(model.lineupAgent(for: role)))
            .font(FreesideFont.monoValue)
            .foregroundStyle(Color.inkDim)
    }

    static func roleName(_ role: RoutingPolicyDraft.Role) -> String {
        role.rawValue.prefix(1).uppercased() + role.rawValue.dropFirst()
    }

    /// A role the policy leaves out runs on the lineup's agent; without a
    /// catalog, or when the lineup binds no agent to it, the name is unknown.
    static func lineupText(_ agent: String?) -> String {
        agent.map { "Lineup: \($0)" } ?? "Lineup"
    }

    /// Who set the policy: a paired device by its name, `freesided routing
    /// set` by the host identity it records.
    static func setBy(_ policy: Components.Schemas.RoutingPolicy, deviceNames: [String: String]) -> String {
        switch policy.source {
        case .client_routing_set:
            let id = policy.set_by.hasPrefix("device:") ? String(policy.set_by.dropFirst(7)) : policy.set_by
            return deviceNames[id] ?? policy.set_by
        case .cli_routing_set:
            return "\(policy.set_by) on the daemon host"
        }
    }

    /// One line for a policy the view doesn't otherwise draw: the one a
    /// refusal carries.
    static func policyLine(_ policy: Components.Schemas.RoutingPolicy) -> String {
        let roles = policy.roles.map { "\(roleName($0.role)): \($0.agents.joined(separator: ", "))" }
        return (["Revision \(policy.revision)"] + roles).joined(separator: " · ")
    }
}
