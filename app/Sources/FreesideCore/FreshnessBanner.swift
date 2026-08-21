import SwiftUI

/// The §5.14 freshness banner: while the daemon is unreachable, its
/// sync reads are failing, or the credential is rejected, the cached
/// view stays readable and this says so; it never blocks the content it
/// qualifies. Fresh and unvalidated states show nothing.
struct FreshnessBanner: View {
    let freshness: InboxStore.Freshness

    var body: some View {
        switch freshness {
        case .fresh, .unvalidated:
            EmptyView()
        case .unreachable:
            banner(
                "Daemon unreachable — showing cached items; actions are disabled.",
                keyword: "Unreachable",
                tint: .accent,
                wash: .accentWash
            )
        case .syncFailing:
            // The wash is the softest of the three, so the message keeps
            // the full-contrast foreground; the keyword alone carries the
            // state color.
            banner(
                "Daemon is reachable but sync is failing — showing cached items; actions are disabled.",
                keyword: "Sync failing",
                tint: .accent,
                wash: .accentWashSoft,
                foreground: .ink
            )
        case .unauthenticated:
            banner(
                "This device's access was revoked. Cached items stay readable; actions are disabled.",
                keyword: "Revoked",
                tint: .wax,
                wash: .waxWash
            )
        }
    }

    /// A full-width tinted wash with a leading small-caps mono keyword in
    /// the state color and the message in Plex Sans, text-dim unless the
    /// state passes an explicit high-contrast `foreground`.
    private func banner(
        _ message: String, keyword: String, tint: Color, wash: Color, foreground: Color = .inkDim
    ) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            KeywordLabel(text: keyword, color: tint)
            Text(message)
                .font(FreesideFont.callout)
                .foregroundStyle(foreground)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(wash)
    }
}
