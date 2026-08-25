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
    public enum Contrast: String {
        case standard
        case increased
    }

    public enum Screen: String {
        case inbox
        case runs
    }

    /// `-FreesideScreen inbox|runs`; defaults to the attention inbox.
    public let screen: Screen

    /// `-FreesideColorScheme light|dark`; unset or unrecognized
    /// follows the system.
    public let colorScheme: ColorScheme?

    /// `-FreesideContrast standard|increased`; unset or unrecognized
    /// follows the system accessibility contrast.
    public let contrast: Contrast?

    /// `-FreesideDynamicType <size>`; unset or unrecognized follows the
    /// system. The accepted names mirror the documented screenshot inputs.
    public let dynamicTypeSize: DynamicTypeSize?

    /// `-FreesideSelect <item-id>`: the inbox item selected at launch.
    /// `AttentionFixtures.defaultInboxItemIDs()` is the canonical value
    /// list. An unknown id is ignored with a stderr note, never a
    /// crash: the capture recipe's content check catches the typo, and
    /// a stray persisted default must not take the app down.
    public let selection: String?

    /// Optional deterministic inbox presentation for screenshot launches.
    public let inboxScope: InboxStore.Scope?
    public let projectID: String?
    public let detailsExpanded: Bool

    public init(
        colorSchemeRaw: String?, contrastRaw: String? = nil, selectionRaw: String?,
        inboxScopeRaw: String? = nil, projectIDRaw: String? = nil,
        detailsExpanded: Bool = false, screenRaw: String? = nil,
        dynamicTypeSizeRaw: String? = nil
    ) {
        screen = Screen(rawValue: screenRaw ?? "") ?? .inbox
        colorScheme =
            switch colorSchemeRaw {
            case "light": .light
            case "dark": .dark
            default: nil
            }
        contrast = Contrast(rawValue: contrastRaw ?? "")
        dynamicTypeSize = Self.dynamicTypeSize(rawValue: dynamicTypeSizeRaw)
        let knownSelections =
            screen == .runs
            ? RunFixtures.defaultRunIDs() : AttentionFixtures.defaultInboxItemIDs()
        if let selectionRaw, !knownSelections.contains(selectionRaw) {
            FileHandle.standardError.write(
                Data("FreesideSelect ignored: unknown item id \(selectionRaw)\n".utf8))
            selection = nil
        } else {
            selection = selectionRaw
        }
        inboxScope = inboxScopeRaw.flatMap(InboxStore.Scope.init(rawValue:))
        projectID = projectIDRaw
        self.detailsExpanded = detailsExpanded
    }

    /// The process's launch arguments, via the UserDefaults argument
    /// domain (`-Key value` pairs).
    public static func standard() -> LaunchInputs {
        let defaults = UserDefaults.standard
        return LaunchInputs(
            colorSchemeRaw: defaults.string(forKey: "FreesideColorScheme"),
            contrastRaw: accessibilityContrastOverride(defaults: defaults)?.rawValue,
            selectionRaw: defaults.string(forKey: "FreesideSelect"),
            inboxScopeRaw: defaults.string(forKey: "FreesideInboxScope"),
            projectIDRaw: defaults.string(forKey: "FreesideProject"),
            detailsExpanded: defaults.bool(forKey: "FreesideDetailsExpanded"),
            screenRaw: defaults.string(forKey: "FreesideScreen"),
            dynamicTypeSizeRaw: defaults.string(forKey: "FreesideDynamicType"))
    }

    static func accessibilityContrastOverride(defaults: UserDefaults = .standard) -> Contrast? {
        Contrast(rawValue: defaults.string(forKey: "FreesideContrast") ?? "")
    }

    private static func dynamicTypeSize(rawValue: String?) -> DynamicTypeSize? {
        switch rawValue {
        case "x-small": .xSmall
        case "small": .small
        case "medium": .medium
        case "large": .large
        case "x-large": .xLarge
        case "xx-large": .xxLarge
        case "xxx-large": .xxxLarge
        case "ax1": .accessibility1
        case "ax2": .accessibility2
        case "ax3": .accessibility3
        case "ax4": .accessibility4
        case "ax5": .accessibility5
        default: nil
        }
    }
}
