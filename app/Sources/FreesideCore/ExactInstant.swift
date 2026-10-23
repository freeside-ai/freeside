import SwiftUI

#if os(iOS)
    import UIKit
#endif

extension View {
    /// Keeps the exact instant one gesture away from a `shortTime` string:
    /// hover help on macOS, a long-press Copy on iOS, as the pairing expiry
    /// row does. A row that shortens a recorded time and shows it nowhere
    /// else uses this so the shortening loses nothing.
    func exactInstant(_ date: Date) -> some View {
        exactStamp(FreesideFormat.exactTime(date))
    }

    /// The same for a line that shortens a span ("Observed … to …"): both
    /// ends, exactly.
    func exactInstants(_ start: Date, to end: Date) -> some View {
        exactStamp("\(FreesideFormat.exactTime(start)) to \(FreesideFormat.exactTime(end))")
    }

    @ViewBuilder
    private func exactStamp(_ exact: String) -> some View {
        #if os(macOS)
            help(exact)
        #elseif os(iOS)
            copyMenu {
                Button("Copy \(exact)") {
                    UIPasteboard.general.string = exact
                }
            }
        #else
            self
        #endif
    }
}

extension EnvironmentValues {
    /// The instant `FreesideFormat.shortTime` compares years against, read
    /// beside `locale` and `timeZone` by the views that format times. Nil
    /// reads the clock; a screenshot pins it with the other two.
    @Entry var pinnedNow: Date? = nil
}
