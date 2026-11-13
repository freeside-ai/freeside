import FreesideAPI
import SwiftUI

/// Which client draws Settings, as a value so the screenshot suite can
/// render the iPhone's words from a Mac host.
enum SettingsClient {
    case mac
    case iPhone
    case iPad

    @MainActor static var current: SettingsClient {
        #if os(macOS)
            .mac
        #else
            UIDevice.current.userInterfaceIdiom == .pad ? .iPad : .iPhone
        #endif
    }

    var name: String {
        switch self {
        case .mac: "This Mac"
        case .iPhone: "This iPhone"
        case .iPad: "This iPad"
        }
    }
}

/// What the Settings panes and the iPhone list's summaries read from a
/// paired session. The models are built once per coordinator by the window
/// or the root view that holds them, so the list's summaries and the pane
/// it pushes read the same state.
@MainActor
struct SettingsSync {
    let devices: DevicesModel
    let routing: RoutingPolicyModel
    let workflow: WorkflowPolicyModel
    let usage: AccountUsageModel
    let freshness: InboxStore.Freshness
    let lastUpdatedAt: Date?
    /// The highest server revision the heartbeat observed.
    let revision: Int64?
    let projects: [String]
    /// This device's id, for when a notification's link was last opened
    /// here.
    let deviceID: String
    /// Passed to the Devices pane's revoke confirmation.
    let unresolvedActionCount: Int
    /// The synced inbox: the System Health pane lists its findings, and the
    /// Mac seats a finding's card on it.
    let store: InboxStore
    /// The unattended-admission gate as the last bootstrap reported it
    /// (#980), which the standing stopped indicator also reads.
    let unattendedOperation: Components.Schemas.UnattendedOperationSnapshot?

    var findings: SystemHealthFindings {
        SystemHealthFindings(open: store.openSnapshots, closed: store.closedSnapshots)
    }

    /// Paired device names by id, once the Devices list has loaded, so the
    /// Agents pane names the device that set the policy.
    var deviceNames: [String: String] {
        let rows = [devices.currentDevice].compactMap(\.self) + devices.otherDevices + devices.revokedDevices
        return Dictionary(rows.map { ($0.id, $0.name) }, uniquingKeysWith: { first, _ in first })
    }
}

/// The words trailing each row of the iPhone's Settings list: what the pane
/// it opens holds, in a few mono words.
@MainActor
enum SettingsSummary {
    /// General: the one preference the app stores.
    static func general(_ preferences: DecisionFlowPreferences) -> String {
        preferences.advancesToNextItem ? "Advance on" : "Advance off"
    }

    /// Agents: an unanswered change, or the enrollments the policy orders
    /// (the agent names an older revision recorded), each once, in role
    /// order.
    static func agents(_ routing: RoutingPolicyModel) -> String {
        if let pending = routing.pending, pending.receipt == nil { return "Unconfirmed" }
        guard let policy = routing.policy else { return "No policy" }
        var seen = Set<String>()
        let names = policy.roles.flatMap { $0.enrollments ?? $0.agents ?? [] }.filter { seen.insert($0).inserted }
        return names.isEmpty ? "Lineup order" : names.joined(separator: " · ")
    }

    /// Usage: an unanswered command, or how many pools sync holds.
    /// The gates that ask first in any section, or Autonomous when none
    /// does.
    static func workflow(_ workflow: WorkflowPolicyModel) -> String {
        if let pending = workflow.pending, pending.receipt == nil { return "Unconfirmed" }
        guard workflow.policy != nil else { return "Not synced" }
        let asking = Set(
            workflow.sections.flatMap { workflow.settings(in: $0) }
                .filter { WorkflowPolicyView.isGate($0.key) && $0.value == "true" }
                .map(\.key))
        switch asking.count {
        case 0: return "Autonomous"
        case 1: return "1 gate asks first"
        case let count: return "\(count) gates ask first"
        }
    }

    static func usage(_ usage: AccountUsageModel) -> String {
        if let pending = usage.pending, pending.receipt == nil { return "Unconfirmed" }
        switch usage.pools.count {
        case 0: return "No pools"
        case 1: return "1 pool"
        case let count: return "\(count) pools"
        }
    }

    /// Devices: this device, how many others are paired, and how many are
    /// revoked, once the list has loaded.
    static func devices(_ devices: DevicesModel, client: SettingsClient) -> String {
        switch devices.loadState {
        case .loading:
            return "Loading…"
        case .failed:
            return "Unavailable"
        case .loaded:
            let others = devices.otherDevices.count
            let revoked = devices.revokedDevices.count
            return [
                devices.currentDevice.map { _ in client.name },
                others == 0 ? nil : others == 1 ? "1 other" : "\(others) others",
                revoked == 0 ? nil : "\(revoked) revoked",
            ]
            .compactMap(\.self)
            .joined(separator: " · ")
        }
    }

    /// Notifications: this device's switch, once the list has loaded.
    static func notifications(_ devices: DevicesModel) -> String {
        switch devices.loadState {
        case .loading: "Loading…"
        case .failed: "Unavailable"
        case .loaded: devices.currentDeviceNotificationsEnabled ? "On" : "Off"
        }
    }

    /// System Health on the iPhone: how many findings are open, and how
    /// many of those block unattended work.
    static func systemHealth(_ findings: SystemHealthFindings) -> String {
        switch (findings.openCount, findings.blockingCount) {
        case (0, _): "Nothing open"
        case (let open, 0): "\(open) open"
        case (let open, let blocking): "\(open) open · \(blocking) blocking"
        }
    }

    /// System Health in the Mac source list: the open count, or nothing
    /// when none is open.
    static func systemHealthCount(_ findings: SystemHealthFindings) -> String? {
        findings.openCount == 0 ? nil : "\(findings.openCount)"
    }

    /// Daemon, in the words the freshness banner's keyword uses.
    static func daemon(_ freshness: InboxStore.Freshness) -> String {
        switch freshness {
        case .unvalidated: "Checking"
        case .fresh: "Connected"
        case .unreachable: "Unreachable"
        case .syncFailing: "Sync failing"
        case .incompatible: "Update needed"
        case .unauthenticated: "Revoked"
        }
    }
}

/// The daemon's host as Settings names it: the host, with the port when the
/// address carries one, as a development instance's does.
enum SettingsHost {
    static func line(_ url: URL?) -> String? {
        guard let host = url?.host() else { return nil }
        return url?.port.map { "\(host):\($0)" } ?? host
    }
}

// MARK: - Pane grammar

/// The pane's opening: the title in the serif, one dim line under it.
struct SettingsPaneHead: View {
    let title: String
    var line: String? = nil

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title)
                .font(FreesideFont.sheetAsk)
                .foregroundStyle(Color.ink)
                .accessibilityAddTraits(.isHeader)
            if let line {
                Text(line)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.bottom, 4)
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// A section of a pane: its keyword head (R1) over its groups.
struct SettingsSection<Content: View>: View {
    let keyword: String
    @ViewBuilder let content: Content

    var body: some View {
        VStack(alignment: .leading, spacing: 9) {
            KeywordLabel(text: keyword)
                .accessibilityAddTraits(.isHeader)
            content
        }
    }
}

/// Grouped rows on one bordered box with a hairline between each (R3).
struct SettingsGroup<Element, ID: Hashable, Row: View>: View {
    let elements: [Element]
    let id: KeyPath<Element, ID>
    @ViewBuilder let row: (Element) -> Row

    var body: some View {
        VStack(spacing: 0) {
            ForEach(elements, id: id) { element in
                if element[keyPath: id] != elements.first?[keyPath: id] {
                    Rectangle()
                        .fill(Color.rule)
                        .frame(height: 1)
                        .padding(.horizontal, 14)
                        .accessibilityHidden(true)
                }
                row(element)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
    }
}

extension SettingsGroup where Element: Identifiable, ID == Element.ID {
    init(_ elements: [Element], @ViewBuilder row: @escaping (Element) -> Row) {
        self.init(elements: elements, id: \.id, row: row)
    }
}

extension SettingsGroup where Element: Hashable, ID == Element {
    init(_ elements: [Element], @ViewBuilder row: @escaping (Element) -> Row) {
        self.init(elements: elements, id: \.self, row: row)
    }
}

/// One row of a group: the label in the fact-label face with one dim line
/// under it, an optional wax warning under that, and the control at the
/// value. At the accessibility sizes the control drops under the label, so
/// a long label never squeezes it.
struct SettingsRow<Control: View>: View {
    let label: String
    var line: String? = nil
    var warning: String? = nil
    @ViewBuilder let control: Control
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        Group {
            if dynamicTypeSize.isAccessibilitySize {
                VStack(alignment: .leading, spacing: 8) {
                    labels
                    control
                }
            } else {
                HStack(spacing: 16) {
                    labels
                    Spacer(minLength: 0)
                    control
                }
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var labels: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(label)
                .font(FreesideFont.factLabel)
                .foregroundStyle(Color.ink)
            if let line {
                Text(line)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let warning {
                Text(warning)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.waxText)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .accessibilityElement(children: .combine)
    }
}

extension SettingsRow where Control == EmptyView {
    init(label: String, line: String? = nil, warning: String? = nil) {
        self.init(label: label, line: line, warning: warning) { EmptyView() }
    }
}

/// A popup's trigger (the canvas's `popupCtl`): the value in the sans
/// medium, then a dim `⌃⌄`, on ground-2 in a box with the window border. A
/// label, not a control: a pane puts it inside its `Menu`, which
/// `ImageRenderer` can't draw, so the goldens show the label. The value
/// wraps past 280 points, so a long one never takes the whole row.
struct SettingsPopupLabel: View {
    let text: String
    @Environment(\.isEnabled) private var isEnabled

    var body: some View {
        HStack(spacing: 8) {
            CappedWidth(maxWidth: 280) {
                Text(text)
                    .font(FreesideFont.actionLabel)
                    .foregroundStyle(isEnabled ? Color.ink : Color.inkFaint)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Text("⌃⌄")
                .font(FreesideFont.disclosureGlyph)
                .foregroundStyle(Color.inkDim)
                .accessibilityHidden(true)
        }
        .padding(.vertical, 4)
        .padding(.leading, 10)
        .padding(.trailing, 8)
        .background(Color.ground2, in: RoundedRectangle(cornerRadius: 6))
        .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(Color.secondaryBorder, lineWidth: 1))
    }
}

/// A pane's padding inside the window or the pushed page. A hosted view
/// that draws its own 16pt sheet header and margins takes 16 less on each
/// side, so its text lines up with the panes drawn in the grammar.
enum SettingsPaneInsets {
    static func grammar(_ client: SettingsClient) -> EdgeInsets {
        switch client {
        case .mac: EdgeInsets(top: 18, leading: 22, bottom: 22, trailing: 22)
        case .iPhone, .iPad: EdgeInsets(top: 18, leading: 16, bottom: 16, trailing: 16)
        }
    }

    static func hosted(_ client: SettingsClient) -> EdgeInsets {
        let insets = grammar(client)
        return EdgeInsets(
            top: insets.top - 16, leading: insets.leading - 16, bottom: insets.bottom - 16,
            trailing: insets.trailing - 16)
    }

    /// The space between a pane's sections.
    static func sectionSpacing(_ client: SettingsClient) -> CGFloat {
        client == .mac ? 18 : 20
    }
}

/// Proposes at most `maxWidth` to its one subview and takes the subview's
/// own size, so a long text wraps at the cap while a short one keeps its
/// width. A `frame(maxWidth:)` would take the whole cap whenever offered it.
private struct CappedWidth: Layout {
    let maxWidth: CGFloat

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        subviews.first?.sizeThatFits(capped(proposal)) ?? .zero
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        subviews.first?.place(at: bounds.origin, proposal: ProposedViewSize(bounds.size))
    }

    private func capped(_ proposal: ProposedViewSize) -> ProposedViewSize {
        ProposedViewSize(width: min(proposal.width ?? .infinity, maxWidth), height: proposal.height)
    }
}

// MARK: - General

/// The General pane: the one preference the app stores, the Decision Flow
/// toggle (◆1 of the 4 Nov 2026 handoff).
struct GeneralPane: View {
    let preferences: DecisionFlowPreferences
    var client: SettingsClient = .current
    var rendersInteractiveControls = true

    var body: some View {
        VStack(alignment: .leading, spacing: SettingsPaneInsets.sectionSpacing(client)) {
            SettingsPaneHead(title: "General", line: "How this app behaves on this device.")
            DecisionFlowSettingsView(
                preferences: preferences, rendersInteractiveControls: rendersInteractiveControls)
        }
    }
}

/// A drawn switch for the screenshot suite: `ImageRenderer` doesn't draw
/// `Toggle`. The canvas's `toggle`: 34 by 20, the accent with a ground-2
/// knob on the right when on, ground-3 with a bordered knob on the left
/// when off.
struct SettingsToggleStandIn: View {
    let isOn: Bool

    var body: some View {
        ZStack(alignment: isOn ? .trailing : .leading) {
            Capsule()
                .fill(isOn ? Color.accentText : Color.ground3)
                .overlay(Capsule().strokeBorder(isOn ? Color.clear : Color.secondaryBorder, lineWidth: 1))
            Circle()
                .fill(Color.ground2)
                .overlay(Circle().strokeBorder(isOn ? Color.clear : Color.secondaryBorder, lineWidth: 1))
                .frame(width: 16, height: 16)
                .padding(2)
        }
        .frame(width: 34, height: 20)
        .accessibilityElement()
        .accessibilityLabel(isOn ? "On" : "Off")
    }
}

// MARK: - The pane switch

/// One Settings pane, shared by the Mac window and the iPhone's pushed
/// page. `sync` is nil before the device pairs; the panes that read the
/// daemon then say so, and General still works.
struct SettingsPaneView: View {
    let page: NavigationModel.SettingsPage
    let sync: SettingsSync?
    let flowPreferences: DecisionFlowPreferences
    let host: String?
    var client: SettingsClient = .current
    #if os(macOS)
        var mac: DaemonPane.MacDaemon? = nil
    #endif
    /// Opens a System Health finding's card.
    var openFinding: (String) -> Void = { _ in }
    /// Where the System Health pane keeps its History fold's state.
    var listDefaults: UserDefaults = .standard
    var rendersInteractiveControls = true

    var body: some View {
        pane
            .frame(maxWidth: .infinity, alignment: .topLeading)
            #if os(iOS)
                .navigationTitle(page.title)
            #endif
    }

    @ViewBuilder private var pane: some View {
        switch page {
        case .general:
            GeneralPane(
                preferences: flowPreferences, client: client,
                rendersInteractiveControls: rendersInteractiveControls
            )
            .padding(SettingsPaneInsets.grammar(client))
        case .agents:
            if let sync {
                RoutingPolicyView(
                    model: sync.routing, deviceNames: sync.deviceNames, client: client,
                    rendersInteractiveControls: rendersInteractiveControls
                )
                .padding(SettingsPaneInsets.grammar(client))
            } else {
                unpaired
            }
        case .workflow:
            if let sync {
                WorkflowPolicyView(
                    model: sync.workflow, deviceNames: sync.deviceNames, client: client,
                    rendersInteractiveControls: rendersInteractiveControls
                )
                .padding(SettingsPaneInsets.grammar(client))
            } else {
                unpaired
            }
        case .usage:
            if let sync {
                AccountUsageView(model: sync.usage)
                    .padding(SettingsPaneInsets.hosted(client))
            } else {
                unpaired
            }
        case .devices:
            if let sync {
                DevicesView(
                    model: sync.devices, host: host, unresolvedActionCount: sync.unresolvedActionCount,
                    client: client, rendersInteractiveControls: rendersInteractiveControls
                )
                .padding(SettingsPaneInsets.grammar(client))
            } else {
                unpaired
            }
        case .notifications:
            if let sync {
                NotificationsPane(
                    model: sync.devices,
                    openedAt: sync.devices.notificationOpens?.openedAt(deviceID: sync.deviceID),
                    client: client, rendersInteractiveControls: rendersInteractiveControls
                )
                .padding(SettingsPaneInsets.grammar(client))
            } else {
                unpaired
            }
        case .daemon:
            daemonPane
        case .systemHealth:
            if let sync {
                SystemHealthPane(
                    findings: sync.findings, operation: sync.unattendedOperation, freshness: sync.freshness,
                    lastUpdatedAt: sync.lastUpdatedAt, client: client, openFinding: openFinding,
                    preferences: ListSectionPreferences(defaults: listDefaults)
                )
                .padding(SettingsPaneInsets.grammar(client))
            } else {
                unpaired
            }
        }
    }

    /// Unpaired, the Mac still draws its supervised daemon with Start or
    /// Stop, as the menu-bar panel does; with no daemon to draw it says
    /// to pair, as the other panes do.
    @ViewBuilder private var daemonPane: some View {
        let daemonSync = sync.map {
            DaemonPane.Sync(
                freshness: $0.freshness, lastUpdatedAt: $0.lastUpdatedAt, revision: $0.revision,
                projects: $0.projects)
        }
        #if os(macOS)
            if daemonSync != nil || mac != nil {
                DaemonPane(host: host, sync: daemonSync, client: client, mac: mac)
                    .padding(SettingsPaneInsets.grammar(client))
            } else {
                unpaired
            }
        #else
            if let daemonSync {
                DaemonPane(host: host, sync: daemonSync, client: client)
                    .padding(SettingsPaneInsets.grammar(client))
            } else {
                unpaired
            }
        #endif
    }

    private var unpaired: some View {
        VStack(alignment: .leading, spacing: SettingsPaneInsets.sectionSpacing(client)) {
            SettingsPaneHead(title: page.title)
            Text("Pair with a daemon to see this.")
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
        }
        .padding(SettingsPaneInsets.grammar(client))
    }
}
