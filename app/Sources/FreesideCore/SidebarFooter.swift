#if os(macOS)
    import SwiftUI

    /// The foot of the Mac sidebar: the Settings gear at the leading edge
    /// and the sync status dot at the trailing one. It rides the sidebar
    /// column, so it hides with a collapsed sidebar; Settings stays on ⌘,
    /// and the standing notices still name a failing sync.
    struct SidebarFooter: View {
        let freshness: InboxStore.Freshness
        let lastUpdatedAt: Date?
        let onOpenSettings: () -> Void

        var body: some View {
            HStack(spacing: 8) {
                Button(action: onOpenSettings) {
                    Image(systemName: "gearshape")
                        .font(.system(size: 15, weight: .medium))
                        .foregroundStyle(Color.inkDim)
                        .frame(width: 28, height: 28)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .help("Settings")
                .accessibilityLabel("Settings")
                .accessibilityIdentifier("settings-gear")
                Spacer(minLength: 8)
                SyncStatusDot(freshness: freshness, lastUpdatedAt: lastUpdatedAt)
            }
            .padding(.horizontal, 10)
            .padding(.vertical, 6)
            .background(Color.sidebarGround)
            // The rows scroll under the foot; the rule marks where they stop.
            .overlay(alignment: .top) {
                Rectangle().fill(Color.rule).frame(height: 1)
            }
        }
    }

    /// The last refresh as one dot, its words in the hover text and the
    /// accessibility value. Colour marks only what needs attention, as the
    /// menu-bar badge does: a faint dot while current, the accent once no
    /// refresh has landed within the staleness threshold, and wax while sync
    /// fails. Under Differentiate Without Color the two attention states
    /// also change shape.
    struct SyncStatusDot: View {
        let freshness: InboxStore.Freshness
        let lastUpdatedAt: Date?

        @Environment(\.accessibilityDifferentiateWithoutColor) private var differentiateWithoutColor

        enum Status: Equatable {
            case current
            case stale
            case failing
        }

        var body: some View {
            // The banner's schedule: entries at `lastUpdatedAt + k ×
            // threshold` are the instants the stale comparison can flip
            // (see `FreshnessBanner.body` for why it is periodic).
            TimelineView(
                .periodic(from: lastUpdatedAt ?? .now, by: SyncCoordinator.stalenessThreshold)
            ) { context in
                let status = Self.status(
                    freshness: freshness, lastUpdatedAt: lastUpdatedAt, at: context.date)
                let summary = Self.summary(
                    freshness: freshness, lastUpdatedAt: lastUpdatedAt, at: context.date)
                mark(status)
                    .frame(width: 28, height: 28)
                    .contentShape(Rectangle())
                    .help(summary)
                    .accessibilityElement()
                    .accessibilityLabel("Sync")
                    .accessibilityValue(summary)
            }
        }

        @ViewBuilder
        private func mark(_ status: Status) -> some View {
            if differentiateWithoutColor, let symbol = Self.shapeSymbol(status) {
                Image(systemName: symbol)
                    .font(.system(size: 11, weight: .semibold))
                    .foregroundStyle(Self.tint(status))
            } else {
                Circle()
                    .fill(Self.tint(status))
                    .frame(width: 8, height: 8)
            }
        }

        static func status(
            freshness: InboxStore.Freshness, lastUpdatedAt: Date?, at now: Date
        ) -> Status {
            if freshness.isFailing { return .failing }
            guard let lastUpdatedAt,
                now.timeIntervalSince(lastUpdatedAt) < SyncCoordinator.stalenessThreshold
            else { return .stale }
            return .current
        }

        static func tint(_ status: Status) -> Color {
            switch status {
            case .current: .inkFaint
            case .stale: .accentText
            case .failing: .waxText
            }
        }

        /// The glyph an attention state takes under Differentiate Without
        /// Color; the current state keeps the plain dot.
        static func shapeSymbol(_ status: Status) -> String? {
            switch status {
            case .current: nil
            case .stale: "clock.fill"
            case .failing: "exclamationmark.triangle.fill"
            }
        }

        /// The hover text: what is wrong, when sync fails, then the last
        /// refresh in the words `LastUpdatedLabel` uses.
        static func summary(
            freshness: InboxStore.Freshness, lastUpdatedAt: Date?, at now: Date
        ) -> String {
            let updated = LastUpdatedLabel.text(for: lastUpdatedAt, at: now)
            guard let problem = problem(freshness) else { return updated }
            return "\(problem) · \(updated)"
        }

        private static func problem(_ freshness: InboxStore.Freshness) -> String? {
            switch freshness {
            case .fresh, .unvalidated: nil
            case .unreachable: "Daemon unreachable"
            case .syncFailing: "Sync failing"
            case .incompatible(.appBelowFloor): "App needs an update"
            case .incompatible(.daemonBelowLevel): "Daemon needs an update"
            case .unauthenticated: "Device access revoked"
            }
        }
    }
#endif
