import Foundation
import FreesideAPI

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

    /// What `-FreesideComposer` names: a decision card's message composer, by
    /// the API action whose button opens it. Five cases, not the whole
    /// `Action` type, so a launch can't name an action that has no composer.
    public enum Composer: String, CaseIterable, Sendable {
        case discuss
        case requestChanges = "request_changes"
        case returnToAgent = "return_to_agent"
        case answerAndRetry = "answer_and_retry"
        case answerWithoutRetry = "answer_without_retry"

        public var action: Components.Schemas.Action {
            switch self {
            case .discuss: .discuss
            case .requestChanges: .request_changes
            case .returnToAgent: .return_to_agent
            case .answerAndRetry: .answer_and_retry
            case .answerWithoutRetry: .answer_without_retry
            }
        }
    }

    /// `-FreesideColorScheme` as given; `colorScheme` reads it as the
    /// SwiftUI value in FreesideCore.
    package let colorSchemeRaw: String?

    /// `-FreesideContrast standard|increased`; unset or unrecognized
    /// follows the system accessibility contrast.
    public let contrast: Contrast?

    /// `-FreesideDynamicType` as given; `dynamicTypeSize` reads it as the
    /// SwiftUI value in FreesideCore.
    package let dynamicTypeSizeRaw: String?

    /// `-FreesideSelect <id>`: the item, task, or run opened at launch; a run
    /// opens through its task, with the run timeline pushed.
    /// `AttentionFixtures.defaultInboxItemIDs()`,
    /// `TaskFixtures.defaultTaskIDs()`, and `RunFixtures.defaultRunIDs()` are
    /// the canonical value lists. An unknown id is ignored with a stderr
    /// note, never a crash: the capture recipe's content check catches the
    /// typo, and a stray persisted default must not take the app down.
    public let selection: NavigationModel.Destination?

    public let projectID: String?
    public let detailsExpanded: Bool

    /// `-FreesideDevices YES`: the Devices sheet is open at launch, so a
    /// capture needs no click on the toolbar.
    public let devicesPresented: Bool

    /// `-FreesideComposer <action>`: the selected inbox item's composer for
    /// that action is presented at launch, so a capture of the sheet needs no
    /// click. Nil unless the value names a composer, `-FreesideSelect` names
    /// an inbox item, and that item's fixture offers the action; anything
    /// else is ignored with a stderr note, as an unknown selection is. The
    /// card still waits until the action's button is enabled before it opens
    /// the composer.
    public let composer: Composer?

    public init(
        colorSchemeRaw: String?, contrastRaw: String? = nil, selectionRaw: String?,
        projectIDRaw: String? = nil, detailsExpanded: Bool = false,
        dynamicTypeSizeRaw: String? = nil, devicesPresented: Bool = false,
        composerRaw: String? = nil
    ) {
        self.colorSchemeRaw = colorSchemeRaw
        contrast = Contrast(rawValue: contrastRaw ?? "")
        self.dynamicTypeSizeRaw = dynamicTypeSizeRaw
        selection = Self.resolveSelection(selectionRaw)
        if selectionRaw != nil, selection == nil {
            FileHandle.standardError.write(
                Data("FreesideSelect ignored: unknown id \(selectionRaw ?? "")\n".utf8))
        }
        projectID = projectIDRaw
        self.detailsExpanded = detailsExpanded
        self.devicesPresented = devicesPresented
        switch Self.resolveComposer(composerRaw, selection: selection) {
        case .resolved(let resolved):
            composer = resolved
        case .ignored(let reason):
            composer = nil
            FileHandle.standardError.write(
                Data("FreesideComposer ignored: \(reason)\n".utf8))
        }
    }

    /// An id names one kind of thing: the fixture sets never share one.
    private static func resolveSelection(_ raw: String?) -> NavigationModel.Destination? {
        guard let raw else { return nil }
        if AttentionFixtures.defaultInboxItemIDs().contains(raw) {
            return .attentionItem(raw)
        }
        if TaskFixtures.defaultTaskIDs().contains(raw) {
            return .task(raw)
        }
        return RunFixtures.defaultRuns().first { $0.run.id == raw }.map {
            .run(taskID: $0.run.task_id, runID: $0.run.id)
        }
    }

    private enum ComposerResolution {
        /// Nil when the argument is unset.
        case resolved(Composer?)
        case ignored(reason: String)
    }

    /// A composer needs an inbox item that offers its action, checked against
    /// the same fixtures that decide which ids a selection may name.
    private static func resolveComposer(
        _ raw: String?, selection: NavigationModel.Destination?
    ) -> ComposerResolution {
        guard let raw else { return .resolved(nil) }
        guard let composer = Composer(rawValue: raw) else {
            return .ignored(reason: "\(raw) names no composer")
        }
        guard case .attentionItem(let selection)? = selection else {
            return .ignored(reason: "\(raw) needs an inbox item from FreesideSelect")
        }
        let item = AttentionFixtures.defaultInbox().first { $0.item.id == selection }?.item
        guard item?.requested_decision.contains(composer.action) == true else {
            return .ignored(reason: "\(selection) does not offer \(raw)")
        }
        return .resolved(composer)
    }

    /// The process's launch arguments, via the UserDefaults argument
    /// domain (`-Key value` pairs).
    public static func standard() -> LaunchInputs {
        let defaults = UserDefaults.standard
        return LaunchInputs(
            colorSchemeRaw: defaults.string(forKey: "FreesideColorScheme"),
            contrastRaw: accessibilityContrastOverride(defaults: defaults)?.rawValue,
            selectionRaw: defaults.string(forKey: "FreesideSelect"),
            projectIDRaw: defaults.string(forKey: "FreesideProject"),
            detailsExpanded: defaults.bool(forKey: "FreesideDetailsExpanded"),
            dynamicTypeSizeRaw: defaults.string(forKey: "FreesideDynamicType"),
            devicesPresented: defaults.bool(forKey: "FreesideDevices"),
            composerRaw: defaults.string(forKey: "FreesideComposer"))
    }

    /// Screenshot capture scopes this input to the synchronous AppKit color
    /// callback. Normal launches retain the argument/defaults and host fallback.
    @TaskLocal static var screenshotIncreasedContrast: Bool?

    package static func accessibilityContrastOverride(defaults: UserDefaults = .standard) -> Contrast? {
        if let increased = screenshotIncreasedContrast {
            return increased ? .increased : .standard
        }
        return Contrast(rawValue: defaults.string(forKey: "FreesideContrast") ?? "")
    }
}
