import SwiftUI

/// The sheet the toolbar's Settings slot opens on both platforms (R11), where
/// Devices now lives. Each row states what the app already holds; only
/// Devices opens anything, because the view's own design is a later round
/// (`devlog/2026-10-30-2010-iphone-stack-settings-sheet.md`).
struct SettingsSheet: View {
    /// Which client draws the sheet, as a value so the screenshot suite can
    /// render the iPhone's words from a Mac host.
    enum Client {
        case mac
        case iPhone
        case iPad

        @MainActor static var current: Client {
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

    /// The Devices row: this device, how many others are paired, and how
    /// many are revoked, once the list has loaded.
    static func devicesSummary(_ devices: DevicesModel, client: Client) -> String {
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

    /// The Notifications row. Only iOS holds a subscription, so the Mac names
    /// where the setup lives (`phoneNotificationSubscription`).
    static func notificationsSummary(_ subscription: DeviceNtfySubscription?, client: Client) -> String {
        switch client {
        case .mac: "On iPhone"
        case .iPhone, .iPad: subscription == nil ? "Not set up" : "ntfy · topic set"
        }
    }

    /// The Projects row: the projects the filter offers, and its selection
    /// when one is made.
    static func projectsSummary(known: [String], filter: String?) -> String {
        let count = known.isEmpty ? "None known" : "\(known.count) known"
        guard filter != nil else { return count }
        return "\(count) · \(UnifiedListView.filterState(projectID: filter)) selected"
    }

    /// The Daemon row, in the words the freshness banner's keyword uses.
    static func daemonSummary(_ freshness: InboxStore.Freshness) -> String {
        switch freshness {
        case .unvalidated: "Checking"
        case .fresh: "Connected"
        case .unreachable: "Unreachable"
        case .syncFailing: "Sync failing"
        case .contractMismatch: "Mismatch"
        case .unauthenticated: "Revoked"
        }
    }

    @Environment(\.dismiss) private var dismiss
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @State private var devices: DevicesModel
    @State private var showsDevices: Bool
    private let unresolvedActionCount: Int
    private let projects: [String]
    private let projectFilter: String?
    private let freshness: InboxStore.Freshness
    private let client: Client
    private let rendersInteractiveControls: Bool

    /// `opensDevices` starts on Devices, for `-FreesideDevices` and its
    /// screenshots. `unresolvedActionCount` passes to `DevicesView`.
    init(
        devices: DevicesModel, opensDevices: Bool = false, unresolvedActionCount: Int = 0,
        projects: [String], projectFilter: String?, freshness: InboxStore.Freshness,
        client: Client = .current, rendersInteractiveControls: Bool = true
    ) {
        _devices = State(initialValue: devices)
        _showsDevices = State(initialValue: opensDevices)
        self.unresolvedActionCount = unresolvedActionCount
        self.projects = projects
        self.projectFilter = projectFilter
        self.freshness = freshness
        self.client = client
        self.rendersInteractiveControls = rendersInteractiveControls
    }

    var body: some View {
        VStack(spacing: 0) {
            if rendersInteractiveControls {
                ScrollView { content }
            } else {
                content
            }
            FreesideSheetActionRow.done { dismiss() }
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
        .frame(minWidth: 380, minHeight: 320)
        // Here, not in `DevicesView`, because the Devices row states the
        // list. On appear, and again when the heartbeat observes a newer
        // revision.
        .task(id: devices.observedRevision) {
            guard rendersInteractiveControls else { return }
            await devices.loadIfStale()
        }
    }

    @ViewBuilder private var content: some View {
        if showsDevices {
            VStack(alignment: .leading, spacing: 0) {
                backRow
                DevicesView(model: devices, unresolvedActionCount: unresolvedActionCount)
            }
        } else {
            rows
        }
    }

    private var backRow: some View {
        Button {
            showsDevices = false
        } label: {
            Label("Settings", systemImage: "chevron.left")
                .font(FreesideFont.callout)
                .foregroundStyle(Color.accentText)
        }
        .buttonStyle(.plain)
        .accessibilityLabel("Back to Settings")
        .help("Back to Settings")
        .padding(.horizontal, 16)
        .padding(.top, 16)
    }

    private var rows: some View {
        VStack(alignment: .leading, spacing: 0) {
            FreesideSheetHeader(
                eyebrow: "Settings", ask: "\(client.name) and the daemon it is paired with", askLineLimit: nil)
            VStack(alignment: .leading, spacing: 6) {
                Button {
                    showsDevices = true
                } label: {
                    row("Devices", summary: Self.devicesSummary(devices, client: client), opens: true)
                }
                .buttonStyle(.plain)
                .freesideHover(cornerRadius: 8)
                row(
                    "Notifications",
                    summary: Self.notificationsSummary(devices.notificationSubscription, client: client))
                row("Projects", summary: Self.projectsSummary(known: projects, filter: projectFilter))
                row("Daemon", summary: Self.daemonSummary(freshness))
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
        }
    }

    /// A bordered item with a mono trailing summary (R31, R8). `opens` adds
    /// the link's chevron; a row without one states and opens nothing.
    private func row(_ label: String, summary: String, opens: Bool = false) -> some View {
        Group {
            if dynamicTypeSize.isAccessibilitySize {
                VStack(alignment: .leading, spacing: 3) {
                    rowLabel(label)
                    HStack(spacing: 10) {
                        rowSummary(summary)
                        Spacer(minLength: 0)
                        if opens { chevron }
                    }
                }
            } else {
                HStack(spacing: 12) {
                    rowLabel(label)
                    Spacer(minLength: 0)
                    rowSummary(summary)
                        .multilineTextAlignment(.trailing)
                    if opens { chevron }
                }
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
        .accessibilityElement(children: .combine)
    }

    private func rowLabel(_ label: String) -> some View {
        Text(label)
            .font(FreesideFont.factLabel)
            .foregroundStyle(Color.ink)
    }

    private func rowSummary(_ summary: String) -> some View {
        Text(summary)
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
    }

    private var chevron: some View {
        Text("›")
            .font(FreesideFont.noticeAction)
            .foregroundStyle(Color.accentText)
            .accessibilityHidden(true)
    }
}
