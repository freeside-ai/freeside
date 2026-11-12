import FreesideAPI
import SwiftUI

/// Settings' Agents pane on both platforms (`SettingsPaneView`), drawn as
/// the 4 Nov 2026 handoff's "Settings Agents" frame: the routing policy
/// (plan §5.4), which enrollment takes a new task for each role when the
/// operator chose none, read from sync and changed as a saved command. A
/// popup per role puts the chosen
/// enrollment first and saves at once; `Change order…` opens
/// `RoutingPolicyEditor` as a sheet. The drain-first popup picks among the
/// pools ordered enrollments sit on. A revision set before policies ordered
/// enrollments reads its agent names, which order nothing since #2599, until
/// the operator changes it. The lineup itself still changes through the
/// committed agent tree.
struct RoutingPolicyView: View {
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    let model: RoutingPolicyModel
    /// Paired device names by device id, so a policy a client set names
    /// that device rather than its id.
    let deviceNames: [String: String]
    var client: SettingsClient = .current
    /// False in the screenshot suite, which can't draw a `Menu` or a
    /// sheet: each popup then draws its label alone.
    var rendersInteractiveControls = true

    var body: some View {
        VStack(alignment: .leading, spacing: SettingsPaneInsets.sectionSpacing(client)) {
            // Plan revision 107: the order is the router's candidate set,
            // ranked by the soonest weekly reset, and the order itself
            // decides only where no reset is known or resets tie.
            SettingsPaneHead(
                title: "Agents",
                line:
                    "A new task goes to the role's agent whose weekly usage resets soonest; the order decides when no reset is known."
            )
            routing
        }
        .sheet(isPresented: editing) {
            ScrollView { RoutingPolicyEditor(model: model) }
                #if os(macOS)
                    .frame(width: 480, height: 560)
                #endif
                .background(Color.ground)
        }
    }

    /// The sheet is open while a draft is; closing it without Save drops
    /// the draft. Save clears the draft itself, so the setter's
    /// `cancelEditing` then finds nothing to drop.
    private var editing: Binding<Bool> {
        Binding(get: { model.draft != nil }, set: { if !$0 { model.cancelEditing() } })
    }

    // MARK: Routing

    private enum RoutingRow: Hashable {
        case popup(RoutingPopup)
        case setBy
        case drainFirst
    }

    private var routingRows: [RoutingRow] {
        RoutingPopup.allCases.map(RoutingRow.popup) + (model.policy == nil ? [] : [.setBy]) + [.drainFirst]
    }

    private var routing: some View {
        SettingsSection(keyword: model.policy.map { "Routing · revision \($0.revision)" } ?? "Routing · no policy") {
            SettingsGroup(routingRows) { row in
                routingRow(row)
            }
            RoutingPolicyNotices(model: model)
            // Why the policy can't change outranks the last refusal, which
            // may predate it.
            if let line = model.unavailableReason ?? model.choiceBlocker {
                Text(line)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    @ViewBuilder private func routingRow(_ row: RoutingRow) -> some View {
        switch row {
        case .popup(let popup):
            let shared = model.sharedOrder(popup)
            let text = popupText(popup, shared: shared)
            SettingsRow(label: popup.title, line: popupLine(popup, shared: shared), warning: missingEntries(popup)) {
                popupControl(text: text, accessibilityLabel: popup == .writer ? "Writer agents" : "Reviewer agent") {
                    ForEach(model.catalog?.enrollments ?? [], id: \.enrollment_id) { enrollment in
                        Button(enrollment.enrollment_id) {
                            Task { await model.choose(enrollment.enrollment_id, for: popup) }
                        }
                    }
                    Divider()
                    Button("Change order…") { model.beginEditing() }
                }
            }
        case .setBy:
            if let policy = model.policy {
                SettingsRow(
                    label: "Set by",
                    line: Self.setBy(policy, deviceNames: deviceNames) + " · "
                        + FreesideFormat.shortTime(
                            policy.set_at, now: pinnedNow ?? .now, locale: locale, timeZone: timeZone)
                )
                .exactInstant(policy.set_at)
            }
        case .drainFirst:
            let account = model.policy?.drain_first_account
            // Without a catalog the app can't tell which pools ordered
            // enrollments sit on, so it doesn't call the account stale.
            let stale =
                account.map { account in
                    model.catalog != nil && !model.drainFirstPools.contains { $0.pool.id == account }
                } ?? false
            SettingsRow(
                label: "Drain first", line: account.flatMap(model.usagePool).map(Self.poolDetail),
                warning: stale ? Self.staleAccount : nil
            ) {
                popupControl(text: account ?? "None", accessibilityLabel: "Drain-first account") {
                    Button("None") { Task { await model.chooseDrainFirst(nil) } }
                    ForEach(model.drainFirstPools, id: \.pool.id) { offered in
                        Button(offered.pool.id) { Task { await model.chooseDrainFirst(offered.pool.id) } }
                    }
                }
            }
        }
    }

    /// The popup's label inside its menu, or the label alone where the
    /// suite can't draw a `Menu`. Disabled while the policy can't change.
    @ViewBuilder private func popupControl(
        text: String, accessibilityLabel: String, @ViewBuilder entries: () -> some View
    ) -> some View {
        let label = SettingsPopupLabel(text: text)
        Group {
            if rendersInteractiveControls {
                Menu {
                    entries()
                } label: {
                    label
                }
                .menuStyle(.button)
                .buttonStyle(.plain)
                .menuIndicator(.hidden)
                .accessibilityLabel(accessibilityLabel)
                .accessibilityValue(text)
            } else {
                label
            }
        }
        .disabled(!model.canEdit)
        // On the Mac the order takes its width before the label's dim line
        // does, as the canvas draws it, up to the label's cap. At phone
        // width that would leave the label a word wide, so the two share.
        .layoutPriority(client == .mac ? 1 : 0)
    }

    /// The trigger: the shared order, the lineup when every role is on it,
    /// or `Differs by role`.
    private func popupText(_ popup: RoutingPopup, shared: [String]?) -> String {
        guard let shared else { return "Differs by role" }
        guard shared.isEmpty else { return Self.orderText(shared) }
        return Self.lineupText(model.sharedLineupSelection(popup))
    }

    /// Writer's dim line names its roles, or each role's order when they
    /// differ; Reviewer has none.
    private func popupLine(_ popup: RoutingPopup, shared: [String]?) -> String? {
        switch popup {
        case .writer:
            guard shared == nil else { return "Specifies, implements and remediates." }
            return popup.roles.map { role in
                let order = model.storedOrder(for: role)
                let text = order.isEmpty ? Self.lineupText(model.lineupSelection(for: role)) : Self.orderText(order)
                return "\(Self.roleName(role)) · \(text)"
            }
            .joined(separator: "\n")
        case .reviewer:
            return nil
        }
    }

    /// The enrollments the popup's roles order that the daemon no longer
    /// holds, and the agent names an older revision orders, which order
    /// nothing since #2599 (a change drops them), each once. Without a
    /// catalog none is called missing.
    private func missingEntries(_ popup: RoutingPopup) -> String? {
        guard let catalog = model.catalog else { return nil }
        let held = Set(catalog.enrollments.map(\.enrollment_id))
        let roles = popup.roles.compactMap { role in model.policy?.roles.first { $0.role == role } }
        func unique(_ entries: [String]) -> [String] {
            var seen = Set<String>()
            return entries.filter { seen.insert($0).inserted }
        }
        let enrollments = unique(roles.flatMap { $0.enrollments ?? [] }.filter { !held.contains($0) })
        let agents = unique(roles.flatMap { $0.agents ?? [] })
        let lines =
            (enrollments.isEmpty ? [] : ["\(enrollments.joined(separator: ", ")) \(Self.notHeld)"])
            + (agents.isEmpty ? [] : ["\(Self.ordersNothing): \(agents.joined(separator: ", "))"])
        return lines.isEmpty ? nil : lines.joined(separator: "\n")
    }

    // MARK: Shared words

    static let staleAccount = "No ordered enrollment is on this account."
    static let notHeld = "not an enrollment the daemon holds"
    static let ordersNothing = "Agent names order nothing; choose enrollments"

    /// The pool's provider and plan, as Account usage heads its card.
    static func poolDetail(_ pool: Components.Schemas.UsagePoolUsage) -> String {
        [pool.provider, pool.plan].compactMap(\.self).joined(separator: " · ")
    }

    /// An order as the canvas reads it: `claude-a/claude_code, then codex`.
    static func orderText(_ entries: [String]) -> String {
        entries.joined(separator: ", then ")
    }

    static func roleName(_ role: RoutingPolicyDraft.Role) -> String {
        role.rawValue.prefix(1).uppercased() + role.rawValue.dropFirst()
    }

    /// A role the policy leaves out runs on the lineup's selection; it reads
    /// `Lineup` alone without a catalog, when the lineup binds nothing to the
    /// role, or when a popup's roles sit on different selections.
    static func lineupText(_ selection: Components.Schemas.TaskLineSelection?) -> String {
        selection.map { "Lineup: \(AgentChoiceDisplay.selectionLabel($0))" } ?? "Lineup"
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
        case .unknown:
            return policy.set_by
        }
    }

    /// One line for a policy the pane doesn't otherwise draw: the one a
    /// refusal carries.
    static func policyLine(_ policy: Components.Schemas.RoutingPolicy) -> String {
        let roles = policy.roles.map {
            "\(roleName($0.role)): \(($0.enrollments ?? $0.agents ?? []).joined(separator: ", "))"
        }
        let drainFirst = policy.drain_first_account.map { ["Drain first: \($0)"] } ?? []
        return (["Revision \(policy.revision)"] + roles + drainFirst).joined(separator: " · ")
    }
}

/// The saved change's R14 notices: a refusal with the policy it carries, an
/// unanswered change with Retry, an accepted one waiting for sync, and one
/// sync recorded. The pane draws them under its Routing group; the editor
/// draws them above its cards.
struct RoutingPolicyNotices: View {
    let model: RoutingPolicyModel

    var body: some View {
        if let failure = model.failure {
            VStack(alignment: .leading, spacing: 8) {
                Notice(tone: .wax, keyword: "Failed", sentence: failure.message)
                    .accessibilityElement(children: .combine)
                if let current = failure.currentPolicy {
                    Text(RoutingPolicyView.policyLine(current))
                        .font(FreesideFont.monoValue)
                        .foregroundStyle(Color.inkDim)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityElement(children: .ignore)
                        .accessibilityAddTraits(.isStaticText)
                        .accessibilityLabel("The daemon holds \(RoutingPolicyView.policyLine(current))")
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
                    : "Revision \(confirmed.policy.revision) already held this policy."
            )
            .accessibilityElement(children: .combine)
        }
    }
}

/// The `Change order…` sheet: the full routing edit, for orders of more than
/// two names. One order per role among the catalog's enrollments, and the
/// drain-first account among the pools they sit on, saved as one command.
struct RoutingPolicyEditor: View {
    let model: RoutingPolicyModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            FreesideSheetHeader(eyebrow: "Routing", ask: "Change the order")
            VStack(alignment: .leading, spacing: 16) {
                RoutingPolicyNotices(model: model)
                if let draft = model.draft {
                    editor(draft)
                }
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    @ViewBuilder private func editor(_ draft: RoutingPolicyDraft) -> some View {
        card(title: "Drain first") {
            ChoiceList(
                accessibilityLabel: "Drain-first account",
                options: drainFirstOptions(current: draft.drainFirstAccount),
                selection: Binding(
                    get: { model.draft?.drainFirstAccount },
                    set: { model.draft?.drainFirstAccount = $0 }))
        }
        ForEach(AgentChoiceDisplay.roleOrder, id: \.self) { role in
            card(title: RoutingPolicyView.roleName(role)) {
                let enrollments = draft.enrollments(for: role)
                if enrollments.isEmpty { lineupLine(role) }
                ForEach(Array(enrollments.enumerated()), id: \.element) { index, enrollment in
                    editableEnrollment(enrollment, role: role, index: index, count: enrollments.count)
                }
                if let catalog = model.catalog {
                    let offered = draft.offered(for: role, catalog: catalog)
                    if !offered.isEmpty {
                        WrappingHStack(horizontalSpacing: 6, verticalSpacing: 6) {
                            ForEach(offered, id: \.self) { enrollment in
                                Button("+ \(enrollment)") { model.draft?.add(enrollment, to: role) }
                                    .buttonStyle(FreesideActionButtonStyle(tone: .tertiary))
                                    .accessibilityLabel("Add \(enrollment) to \(RoutingPolicyView.roleName(role))")
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
                .keyboardShortcut(.cancelAction)
            Button("Save") { Task { await model.save() } }
                .buttonStyle(FreesideActionButtonStyle(tone: .primary))
                .disabled(model.saveBlocker != nil || model.sending)
        }
    }

    private func editableEnrollment(
        _ enrollment: String, role: RoutingPolicyDraft.Role, index: Int, count: Int
    ) -> some View {
        let roleName = RoutingPolicyView.roleName(role)
        let held = model.catalog?.enrollments.contains { $0.enrollment_id == enrollment } ?? false
        return HStack(spacing: 6) {
            orderedEnrollment(index: index, name: enrollment, held: held)
            Spacer(minLength: 0)
            iconButton("↑", label: "Move \(enrollment) up in \(roleName)", enabled: index > 0) {
                model.draft?.move(enrollment, in: role, by: -1)
            }
            iconButton("↓", label: "Move \(enrollment) down in \(roleName)", enabled: index < count - 1) {
                model.draft?.move(enrollment, in: role, by: 1)
            }
            iconButton("✕", label: "Remove \(enrollment) from \(roleName)", enabled: true) {
                model.draft?.remove(enrollment, from: role)
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

    /// None, then each pool the daemon would accept. A current account it
    /// wouldn't stays listed, so the selection and the save blocker that
    /// explains it stay visible.
    private func drainFirstOptions(current: String?) -> [ChoiceList<String?>.Option] {
        let pools = model.drainFirstPools
        var options: [ChoiceList<String?>.Option] = [
            .init(value: nil, label: "None", consequence: "Reset order alone decides.", register: .item)
        ]
        options += pools.map { offered in
            .init(
                value: offered.pool.id, label: offered.pool.id,
                consequence:
                    "\(RoutingPolicyView.poolDetail(offered.pool))\nEnrollments: \(offered.enrollments.joined(separator: ", "))",
                register: .item)
        }
        if let current, !pools.contains(where: { $0.pool.id == current }) {
            options.append(
                .init(value: current, label: current, consequence: RoutingPolicyView.staleAccount, register: .item))
        }
        return options
    }

    private func card(title: String, @ViewBuilder content: () -> some View) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            KeywordLabel(text: title)
                .accessibilityAddTraits(.isHeader)
            content()
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
    }

    private func orderedEnrollment(index: Int, name: String, held: Bool) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text("\(index + 1).")
                .font(FreesideFont.monoValue)
                .foregroundStyle(Color.inkDim)
            Text(name)
                .font(FreesideFont.monoValue)
                .foregroundStyle(Color.ink)
            if !held {
                Text(RoutingPolicyView.notHeld)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.waxText)
            }
        }
        .accessibilityElement(children: .combine)
    }

    private func lineupLine(_ role: RoutingPolicyDraft.Role) -> some View {
        Text(RoutingPolicyView.lineupText(model.lineupSelection(for: role)))
            .font(FreesideFont.monoValue)
            .foregroundStyle(Color.inkDim)
    }
}
