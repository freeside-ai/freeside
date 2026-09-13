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

    public enum Screen: String, Hashable {
        case inbox
        case tasks
    }

    /// What `-FreesideSelect` names on the tasks screen: a task, or a run
    /// reached through its task, so a run link opens the task with the run
    /// timeline pushed.
    public enum TaskSelection: Equatable {
        case task(String)
        case run(taskID: String, runID: String)
    }

    /// `-FreesideScreen inbox|tasks`; defaults to the attention inbox.
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

    /// `-FreesideSelect <id>`: the inbox item selected at launch, or on
    /// the tasks screen the task or run opened at launch.
    /// `AttentionFixtures.defaultInboxItemIDs()`,
    /// `TaskFixtures.defaultTaskIDs()`, and `RunFixtures.defaultRunIDs()` are
    /// the canonical value lists. An unknown id is ignored with a stderr
    /// note, never a crash: the capture recipe's content check catches the
    /// typo, and a stray persisted default must not take the app down.
    public let selection: String?

    /// The tasks-screen reading of `selection`: nil on the inbox screen or
    /// when nothing valid was selected.
    public let taskSelection: TaskSelection?

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
        let resolved = Self.resolveSelection(selectionRaw, screen: screen)
        selection = resolved.selection
        taskSelection = resolved.taskSelection
        if selectionRaw != nil, selection == nil {
            FileHandle.standardError.write(
                Data("FreesideSelect ignored: unknown item id \(selectionRaw ?? "")\n".utf8))
        }
        inboxScope = inboxScopeRaw.flatMap(InboxStore.Scope.init(rawValue:))
        projectID = projectIDRaw
        self.detailsExpanded = detailsExpanded
    }

    /// The screen decides which fixture ids a selection may name. On the
    /// tasks screen a run id resolves to its task, from the run fixtures,
    /// so the launch routes to the task with that run pushed.
    private static func resolveSelection(
        _ raw: String?, screen: Screen
    ) -> (selection: String?, taskSelection: TaskSelection?) {
        guard let raw else { return (nil, nil) }
        switch screen {
        case .inbox:
            return AttentionFixtures.defaultInboxItemIDs().contains(raw) ? (raw, nil) : (nil, nil)
        case .tasks:
            if TaskFixtures.defaultTaskIDs().contains(raw) {
                return (raw, .task(raw))
            }
            if let run = RunFixtures.defaultRuns().first(where: { $0.run.id == raw })?.run {
                return (raw, .run(taskID: run.task_id, runID: run.id))
            }
            return (nil, nil)
        }
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
