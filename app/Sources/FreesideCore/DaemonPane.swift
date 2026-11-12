import SwiftUI

/// The Daemon pane of Settings: the daemon this device is paired with, and
/// how sync with it stands. On the Mac it is the menu-bar panel's Daemon
/// section as a pane (the host row with version, start time, state, and Stop
/// or Start, then Open Login Items); the iPhone holds no version or start
/// time, which come from the Mac's menu-bar model, so its host row names the
/// host alone (the 4 Nov 2026 handoff's "Values sync lacks").
struct DaemonPane: View {
    #if os(macOS)
        /// The Mac's supervised daemon, as the menu-bar panel reads it, with
        /// the same actions the panel calls.
        struct MacDaemon {
            let state: DaemonMenuState
            let actionError: String?
            let start: () -> Void
            let stop: () -> Void
            let openApprovalSettings: () -> Void
        }
    #endif

    private enum SyncRow: CaseIterable {
        case state
        case projects
    }

    /// How sync with the daemon stands, once this device is paired.
    struct Sync {
        let freshness: InboxStore.Freshness
        let lastUpdatedAt: Date?
        /// The highest server revision the heartbeat observed.
        let revision: Int64?
        let projects: [String]
    }

    let host: String?
    /// Nil before pairing: the Mac still draws its supervised daemon, so
    /// Start stays reachable as it is in the menu-bar panel.
    let sync: Sync?
    var client: SettingsClient = .current
    #if os(macOS)
        /// Nil for an app with no supervised daemon: no host state, control,
        /// or Login Items link.
        var mac: MacDaemon? = nil
    #endif

    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        VStack(alignment: .leading, spacing: SettingsPaneInsets.sectionSpacing(client)) {
            SettingsPaneHead(title: "Daemon")
            hostRow
            if let sync {
                syncSection(sync)
            } else {
                Text("Pair with this daemon to see sync.")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            }
            #if os(macOS)
                if let mac, mac.state != .unsupervised {
                    HStack {
                        Spacer(minLength: 0)
                        Button(action: mac.openApprovalSettings) {
                            Text("Open Login Items ›")
                                .font(FreesideFont.noticeAction)
                                .foregroundStyle(Color.accentText)
                        }
                        .buttonStyle(.plain)
                        .accessibilityLabel("Open Login Items")
                    }
                }
            #endif
        }
    }

    // MARK: Host

    @ViewBuilder private var hostRow: some View {
        #if os(macOS)
            if let mac {
                macHostRow(mac)
            } else {
                plainHostRow
            }
        #else
            plainHostRow
        #endif
    }

    private var plainHostRow: some View {
        SettingsGroup(["host"]) { _ in
            SettingsRow(label: host ?? "No daemon address")
        }
    }

    #if os(macOS)
        /// The menu-bar panel's Daemon section on one bordered box: the host
        /// over the version and start time or what the state means, and the
        /// state chip and the one lifecycle control trailing. Under it, a
        /// failed action and an app or daemon that needs an update, as the
        /// panel draws them.
        private func macHostRow(_ mac: MacDaemon) -> some View {
            VStack(alignment: .leading, spacing: 12) {
                // At the accessibility sizes the state and control drop under
                // the host, as a row's control does.
                let layout =
                    dynamicTypeSize.isAccessibilitySize
                    ? AnyLayout(VStackLayout(alignment: .leading, spacing: 8))
                    : AnyLayout(HStackLayout(spacing: 16))
                layout {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(host ?? "No daemon address")
                            .font(FreesideFont.factLabel)
                            .foregroundStyle(Color.ink)
                        if case .running(let health, _) = mac.state {
                            Text(
                                "freesided \(health.version) · from "
                                    + FreesideFormat.shortTime(
                                        health.startedAt, now: pinnedNow ?? .now, locale: locale,
                                        timeZone: timeZone)
                            )
                            .font(FreesideFont.trailingSummary)
                            .foregroundStyle(Color.inkDim)
                            .exactInstant(health.startedAt)
                        } else if let explanation = mac.state.explanation {
                            Text(explanation)
                                .font(FreesideFont.cardBody)
                                .foregroundStyle(Color.inkDim)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                    }
                    if !dynamicTypeSize.isAccessibilitySize {
                        Spacer(minLength: 0)
                    }
                    HStack(spacing: 10) {
                        mac.state.chip
                        lifecycleButton(mac)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 14)
                .padding(.vertical, 12)
                .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
                if let actionError = mac.actionError {
                    Notice(tone: .wax, keyword: "Failed", sentence: actionError)
                        .accessibilityElement(children: .combine)
                }
                if case .running(let health, _) = mac.state, let incompatibility = health.incompatibility {
                    Notice(
                        tone: .accent, keyword: "Update needed",
                        sentence: CompatibilityWords(incompatibility: incompatibility, platform: .mac).notice
                    )
                    .accessibilityElement(children: .combine)
                }
            }
        }

        @ViewBuilder
        private func lifecycleButton(_ mac: MacDaemon) -> some View {
            switch mac.state.lifecycleControl {
            case .start:
                Button("Start", action: mac.start)
                    .buttonStyle(FreesideActionButtonStyle(tone: .primary, compact: true, expands: false))
            case .stop:
                Button("Stop", action: mac.stop)
                    .buttonStyle(FreesideActionButtonStyle(tone: .secondary, compact: true, expands: false))
            case nil:
                EmptyView()
            }
        }
    #endif

    // MARK: Sync

    private func syncSection(_ sync: Sync) -> some View {
        SettingsSection(keyword: "Sync") {
            SettingsGroup(SyncRow.allCases) { row in
                switch row {
                case .state:
                    SettingsRow(label: "State", line: stateLine(sync)) {
                        freshnessChip(sync.freshness)
                    }
                    .modifier(LastChangeInstant(date: sync.lastUpdatedAt))
                case .projects:
                    SettingsRow(
                        label: "Projects",
                        line: sync.projects.isEmpty ? "None known" : sync.projects.joined(separator: " · ")
                    ) {
                        if !sync.projects.isEmpty {
                            Text("\(sync.projects.count) known")
                                .font(FreesideFont.trailingSummary)
                                .foregroundStyle(Color.inkDim)
                        }
                    }
                }
            }
        }
    }

    private func stateLine(_ sync: Sync) -> String {
        let parts = [
            sync.lastUpdatedAt.map {
                "Last change "
                    + FreesideFormat.shortTime($0, now: pinnedNow ?? .now, locale: locale, timeZone: timeZone)
            },
            sync.revision.map { "revision \($0.formatted(.number.locale(locale)))" },
        ]
        .compactMap(\.self)
        return parts.isEmpty ? "Not synced yet" : parts.joined(separator: " · ")
    }

    /// The freshness banner's word: ink while connected, faint while the
    /// first check runs, the accent for an app or daemon that needs an
    /// update, and wax for a
    /// sync that isn't reaching the daemon.
    private func freshnessChip(_ freshness: InboxStore.Freshness) -> StateChip {
        let label = SettingsSummary.daemon(freshness)
        return switch freshness {
        case .fresh: StateChip(label: label, cut: .ink)
        case .unvalidated: StateChip(label: label, cut: .faint)
        case .incompatible: StateChip(label: label, cut: .attention)
        case .unreachable, .syncFailing, .unauthenticated: StateChip(label: label, color: .waxText)
        }
    }
}

/// The exact instant of the last change, when there is one, behind the
/// State row's shortened time.
private struct LastChangeInstant: ViewModifier {
    let date: Date?

    func body(content: Content) -> some View {
        if let date {
            content.exactInstant(date)
        } else {
            content
        }
    }
}
