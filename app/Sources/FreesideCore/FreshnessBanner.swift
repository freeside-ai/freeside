import SwiftUI

/// The §5.14 freshness banner: while the daemon is unreachable or the
/// credential is rejected, the cached view stays readable and this says
/// so; it never blocks the content it qualifies. Fresh and unvalidated
/// states show nothing.
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
        case .unauthenticated:
            banner(
                "This device's access was revoked. Cached items stay readable; actions are disabled.",
                icon: "lock.slash",
                tint: .red
            )
        }
    }

    private func banner(_ message: String, icon: String, tint: Color) -> some View {
        Label(message, systemImage: icon)
            .font(.callout)
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(tint.opacity(0.15))
            .foregroundStyle(tint)
    }
}
