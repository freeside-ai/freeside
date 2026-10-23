/// One + / − pair in the diff cuts (R28): the added count in the add color
/// and the removed count, behind a typographic minus, in the remove color.
/// The cuts are `Text`, so a caller sets the pair inside a longer mono line.
package struct DiffCounts: Equatable {
    package let added: Int
    package let removed: Int

    package init(added: Int, removed: Int) {
        self.added = added
        self.removed = removed
    }

    /// The pair as the plain line the cuts draw.
    package var plain: String { "+\(added) \u{2212}\(removed)" }

    /// The pair as VoiceOver reads it, in words instead of signs.
    package var spoken: String { "\(added) added, \(removed) removed" }

    /// Successive measurements of one diff, earliest first.
    package static func plain(_ measurements: [DiffCounts]) -> String {
        measurements.map(\.plain).joined(separator: growthSeparator)
    }

    package static func spoken(_ measurements: [DiffCounts]) -> String {
        measurements.map(\.spoken).joined(separator: ", then ")
    }

    package static let growthSeparator = " \u{2192} "
}
