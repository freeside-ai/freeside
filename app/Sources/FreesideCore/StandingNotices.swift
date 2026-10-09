import FreesideAPI
import SwiftUI

/// Where a standing notice draws.
enum StandingNoticePlacement {
    /// The full-width slot above the list: an iPhone, and a Mac window
    /// whose sidebar is collapsed.
    case window
    /// The top of the regular-width detail column, at the card's x and
    /// width, so the sidebar is never under a notice.
    case detailColumn
}

extension View {
    /// The inset a standing notice takes. Each notice carries its own
    /// because they come and go independently, and a container's padding
    /// would outlive them. In the window's slot that is 16pt from each side
    /// and 4pt above and below, so two notices sit 8pt apart. In the detail
    /// column it is the pane's side margin and the card's width cap, with a
    /// module gap above, so the notices stack a module gap apart;
    /// `StandingDetailColumn` supplies the rest of the column's top margin.
    @ViewBuilder
    func standingNoticeInset(_ placement: StandingNoticePlacement = .window) -> some View {
        switch placement {
        case .window:
            padding(.horizontal, 16).padding(.vertical, 4)
        case .detailColumn:
            let margin = DecisionCardComposition.Scale.paneMargin(compact: false)
            frame(maxWidth: DecisionCardComposition.Scale.cardWidth, alignment: .leading)
                .padding(.leading, margin.leading)
                .padding(.trailing, margin.trailing)
                .padding(.top, DecisionCardComposition.Scale.moduleGap)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

/// The notices that stand over the synced surface whatever is selected: the
/// prompt-history save warning, the freshness banner, and the
/// unattended-stopped indicator, in that order.
struct StandingNotices: View {
    let saveWarning: String?
    let onDismissSaveWarning: () -> Void
    let freshness: InboxStore.Freshness
    let lastUpdatedAt: Date?
    let onRePair: (() -> Void)?
    let operation: Components.Schemas.UnattendedOperationSnapshot?
    let reason: (String) -> String?
    let onOpenItem: ((String) -> Void)?
    var placement = StandingNoticePlacement.window

    var body: some View {
        VStack(spacing: 0) {
            if let saveWarning {
                Notice(
                    tone: .wax, keyword: "Not saved", sentence: saveWarning,
                    action: .init(label: "Dismiss", handler: onDismissSaveWarning)
                )
                .standingNoticeInset(placement)
                .accessibilityElement(children: .contain)
            }
            FreshnessBanner(
                freshness: freshness, lastUpdatedAt: lastUpdatedAt, onRePair: onRePair,
                placement: placement)
            UnattendedStoppedIndicator(
                operation: operation, freshness: freshness, lastUpdatedAt: lastUpdatedAt,
                reason: reason, onOpenItem: onOpenItem, placement: placement)
        }
    }

    /// Whether any notice draws at `now`. The detail column reads it to
    /// seat what follows, so it asks each notice the question its own body
    /// answers.
    func isShowing(at now: Date) -> Bool {
        saveWarning != nil
            || FreshnessBanner.isShowing(freshness: freshness, lastUpdatedAt: lastUpdatedAt, at: now)
            || UnattendedStoppedPresentation.make(
                operation: operation, freshness: freshness, lastUpdatedAt: lastUpdatedAt,
                now: now, reason: reason) != nil
    }
}

/// The regular-width detail column under its standing notices. The notices
/// start at the column's top margin and stack above the selected content a
/// module gap apart, pinned while the content scrolls. A column with no
/// notice to draw is the content alone, at the pane's own margin.
struct StandingDetailColumn<Content: View>: View {
    /// Nil where the notices draw in the window's slot instead.
    let notices: StandingNotices?
    /// The selected content. Its argument is the top margin its first
    /// surface takes under a notice; nil keeps the pane's own.
    @ViewBuilder let content: (CGFloat?) -> Content

    var body: some View {
        // The notices' own schedule: its entries are the instants a notice
        // can appear without any input changing. The content keeps one
        // place in the tree whether or not the column holds the notices, so
        // collapsing the sidebar under an open card moves the notices to
        // the window's slot without rebuilding the card.
        TimelineView(
            .periodic(from: notices?.lastUpdatedAt ?? .now, by: SyncCoordinator.stalenessThreshold)
        ) { context in
            let scale = DecisionCardComposition.Scale.self
            let showing = notices?.isShowing(at: context.date) ?? false
            VStack(spacing: 0) {
                if let notices {
                    notices
                        .padding(
                            .top,
                            showing ? scale.paneMargin(compact: false).top - scale.moduleGap : 0)
                }
                content(showing ? scale.moduleGap : nil)
            }
        }
    }
}
