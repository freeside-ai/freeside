import Foundation
import FreesideAPI
import SwiftUI

/// Presentation state pinned per launch so screenshot and automation
/// workflows drive the app purely through launch arguments (#109):
/// no mutation of the user's system appearance, no accessibility
/// scripting to click a row. Launch arguments, not environment
/// variables, because `open --args` forwards only arguments and
/// `simctl launch` forwards them too; the composition arguments in
/// `AppSession.fromEnvironment` follow the same convention. Unset
/// means the ordinary launch: system appearance, nothing selected.
public struct LaunchInputs {
    /// `-FreesideColorScheme light|dark`; unset or unrecognized
    /// follows the system.
    public let colorScheme: ColorScheme?

    /// `-FreesideSelect <item-id>`: the inbox item selected at launch.
    /// `AttentionFixtures.defaultInboxItemIDs()` is the canonical value
    /// list. An unknown id is ignored with a stderr note, never a
    /// crash: the capture recipe's content check catches the typo, and
    /// a stray persisted default must not take the app down.
    public let selection: String?

    public init(colorSchemeRaw: String?, selectionRaw: String?) {
        colorScheme =
            switch colorSchemeRaw {
            case "light": .light
            case "dark": .dark
            default: nil
            }
        if let selectionRaw, !AttentionFixtures.defaultInboxItemIDs().contains(selectionRaw) {
            FileHandle.standardError.write(
                Data("FreesideSelect ignored: unknown item id \(selectionRaw)\n".utf8))
            selection = nil
        } else {
            selection = selectionRaw
        }
    }

    /// The process's launch arguments, via the UserDefaults argument
    /// domain (`-Key value` pairs).
    public static func standard() -> LaunchInputs {
        let defaults = UserDefaults.standard
        return LaunchInputs(
            colorSchemeRaw: defaults.string(forKey: "FreesideColorScheme"),
            selectionRaw: defaults.string(forKey: "FreesideSelect"))
    }
}
