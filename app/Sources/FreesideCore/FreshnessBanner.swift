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
                icon: "wifi.slash",
                tint: .orange
            )
        case .syncFailing:
            // Yellow reads as caution but is too light to double as body
            // text on its own tint, so the foreground is the adaptive
            // high-contrast primary; the yellow panel and warning icon
            // carry the signal. Orange and red are dark enough to serve
            // as both, so they keep the single-tint form.
            banner(
                "Daemon is reachable but sync is failing — showing cached items; actions are disabled.",
                icon: "exclamationmark.triangle",
                tint: .yellow,
                foreground: .primary
            )
        case .unauthenticated:
            banner(
                "This device's access was revoked. Cached items stay readable; actions are disabled.",
                icon: "lock.slash",
                tint: .red
            )
        }
    }

    /// `tint` sets the panel wash and, by default, the text/icon color.
    /// A banner whose tint is too light to read as text passes an
    /// explicit high-contrast `foreground` while keeping the tint as the
    /// panel accent.
    private func banner(
        _ message: String, icon: String, tint: Color, foreground: Color? = nil
    ) -> some View {
        Label(message, systemImage: icon)
            .font(.callout)
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(tint.opacity(0.15))
            .foregroundStyle(foreground ?? tint)
    }
}
