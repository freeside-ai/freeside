import FreesideAPI
import SwiftUI

/// Settings' Workflow pane on both platforms (`SettingsPaneView`): the
/// per-effect gates and the follow-up filing caps (plan §3.2), read from
/// sync and changed as a saved command. General holds what every project
/// inherits; each project the daemon serves follows with its own rows,
/// which show the value that wins (the project's, else general's, else the
/// default) and where it came from. A project row its own layer sets offers
/// a reset to general. Each control saves at once.
struct WorkflowPolicyView: View {
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    let model: WorkflowPolicyModel
    /// Paired device names by device id, so a value a client set names that
    /// device rather than its id.
    let deviceNames: [String: String]
    var client: SettingsClient = .current
    /// False in the screenshot suite, which can't draw a `Toggle` or a
    /// `Menu`: each control then draws its stand-in.
    var rendersInteractiveControls = true

    var body: some View {
        VStack(alignment: .leading, spacing: SettingsPaneInsets.sectionSpacing(client)) {
            SettingsPaneHead(
                title: "Workflow",
                line:
                    "When a task asks you before acting on GitHub, and how many follow-up issues it may file. A change applies to tasks created after it."
            )
            if model.failure != nil || model.pending != nil || model.confirmed != nil
                || model.unavailableReason != nil
            {
                VStack(alignment: .leading, spacing: 9) {
                    WorkflowPolicyNotices(model: model)
                    if let line = model.unavailableReason {
                        Text(line)
                            .font(FreesideFont.cardBody)
                            .foregroundStyle(Color.inkDim)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
            }
            ForEach(model.sections, id: \.self) { section in
                SettingsSection(keyword: Self.keyword(section)) {
                    SettingsGroup(elements: model.settings(in: section), id: \.key) { setting in
                        row(setting, in: section)
                    }
                }
            }
            // Until autonomous dispatch (#3150), only the closure gate
            // changes what a task does; the other two are stored and
            // resolved so a task created now carries them.
            Text(
                "Only the closure gate changes what a task does today. Filing and comment gates apply once tasks can act on their own."
            )
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.inkFaint)
            .fixedSize(horizontal: false, vertical: true)
        }
    }

    static func keyword(_ section: WorkflowPolicySection) -> String {
        switch section {
        case .general: "General · every project"
        case .project(let id): "Project · \(id)"
        }
    }

    @ViewBuilder private func row(
        _ setting: Components.Schemas.WorkflowPolicySetting, in section: WorkflowPolicySection
    ) -> some View {
        let canReset = WorkflowPolicyModel.canReset(setting, in: section)
        SettingsRow(label: Self.label(setting.key), line: line(setting, in: section)) {
            HStack(spacing: 10) {
                if canReset, let project = section.projectID {
                    Button("Use general") {
                        Task { await model.resetToGeneral(setting.key, projectID: project) }
                    }
                    .buttonStyle(.plain)
                    .font(FreesideFont.actionLabel)
                    .foregroundStyle(model.canEdit ? Color.accentText : Color.inkFaint)
                    .disabled(!model.canEdit)
                    .accessibilityLabel("\(Self.label(setting.key)): use general")
                }
                control(setting, in: section)
            }
        }
    }

    @ViewBuilder private func control(
        _ setting: Components.Schemas.WorkflowPolicySetting, in section: WorkflowPolicySection
    ) -> some View {
        if Self.isGate(setting.key) {
            let isOn = setting.value == "true"
            Group {
                if rendersInteractiveControls {
                    Toggle(
                        Self.label(setting.key),
                        isOn: Binding(
                            get: { isOn },
                            set: { on in
                                Task { await model.set(setting.key, to: on ? "true" : "false", in: section) }
                            })
                    )
                    .labelsHidden()
                    .toggleStyle(.switch)
                } else {
                    SettingsToggleStandIn(isOn: isOn)
                }
            }
            .disabled(!model.canEdit)
        } else {
            let label = SettingsPopupLabel(text: setting.value)
            Group {
                if rendersInteractiveControls {
                    Menu {
                        ForEach(Self.capChoices(current: setting.value), id: \.self) { choice in
                            Button("\(choice)") { Task { await model.set(setting.key, to: "\(choice)", in: section) } }
                        }
                    } label: {
                        label
                    }
                    .menuStyle(.button)
                    .buttonStyle(.plain)
                    .menuIndicator(.hidden)
                    .accessibilityLabel(Self.label(setting.key))
                    .accessibilityValue(setting.value)
                } else {
                    label
                }
            }
            .disabled(!model.canEdit)
        }
    }

    /// The row's dim line: the winning value, its layer, and who set it
    /// when.
    private func line(
        _ setting: Components.Schemas.WorkflowPolicySetting, in section: WorkflowPolicySection
    )
        -> String
    {
        var parts = ["\(Self.valueText(setting)) (\(Self.originText(setting.origin, in: section)))"]
        if let setBy = setting.set_by {
            parts.append(Self.setBy(setBy, deviceNames: deviceNames))
        }
        if let setAt = setting.set_at {
            parts.append(FreesideFormat.shortTime(setAt, now: pinnedNow ?? .now, locale: locale, timeZone: timeZone))
        }
        return parts.joined(separator: " · ")
    }

    static func isGate(_ key: Components.Schemas.WorkflowPolicyKey) -> Bool {
        switch key {
        case .gates_period_follow_up_filing, .gates_period_issue_comment, .gates_period_source_issue_closure: true
        case .follow_up_filing_period_max_per_day, .follow_up_filing_period_max_per_run,
            .follow_up_filing_period_max_depth, .unknown:
            false
        }
    }

    static func label(_ key: Components.Schemas.WorkflowPolicyKey) -> String {
        switch key {
        case .gates_period_follow_up_filing: "Ask me before filing follow-up issues"
        case .gates_period_issue_comment: "Ask me before commenting on an issue"
        case .gates_period_source_issue_closure: "Ask me before closing the source issue"
        case .follow_up_filing_period_max_per_day: "Follow-up issues per day"
        case .follow_up_filing_period_max_per_run: "Follow-up issues per task run"
        case .follow_up_filing_period_max_depth: "Follow-up depth"
        case .unknown: "Unknown setting"
        }
    }

    /// A gate reads as the path it takes; a cap as its number.
    static func valueText(_ setting: Components.Schemas.WorkflowPolicySetting) -> String {
        guard isGate(setting.key) else { return setting.value }
        return setting.value == "true" ? "Ask first" : "Autonomous"
    }

    static func originText(
        _ origin: Components.Schemas.WorkflowPolicyOrigin, in section: WorkflowPolicySection
    )
        -> String
    {
        switch origin {
        case .project: "this project"
        case .general: section == .general ? "set here" : "general"
        case ._default: "default"
        case .unknown: "unknown"
        }
    }

    static func setBy(_ setBy: String, deviceNames: [String: String]) -> String {
        guard setBy.hasPrefix("device:") else { return setBy }
        return deviceNames[String(setBy.dropFirst(7))] ?? setBy
    }

    /// The cap popup's choices: a short ladder, with the current value
    /// added when it sits off it.
    static func capChoices(current: String) -> [Int] {
        let ladder = [1, 2, 3, 5, 10, 20, 50]
        guard let value = Int(current), value > 0, !ladder.contains(value) else { return ladder }
        return (ladder + [value]).sorted()
    }
}

/// The saved change's notices, as the Agents pane draws them
/// (`RoutingPolicyNotices`): a refusal, an unanswered change with Retry, an
/// accepted one waiting for sync, and one sync recorded.
struct WorkflowPolicyNotices: View {
    let model: WorkflowPolicyModel

    var body: some View {
        if let failure = model.failure {
            VStack(alignment: .leading, spacing: 8) {
                Notice(tone: .wax, keyword: "Failed", sentence: failure.message)
                    .accessibilityElement(children: .combine)
                if let current = failure.currentPolicy {
                    Text("The daemon holds revision \(current.revision).")
                        .font(FreesideFont.monoValue)
                        .foregroundStyle(Color.inkDim)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
        }
        if let pending = model.pending {
            if pending.receipt == nil {
                Notice(
                    tone: .accent, keyword: "Unconfirmed",
                    sentence: model.sending
                        ? "Sending the change…"
                        : "The daemon hasn't answered. Retry sends the same change to recover its result.",
                    action: .init(
                        label: "Retry", accessibilityLabel: "Retry sending the workflow change",
                        isEnabled: !model.sending
                    ) { Task { await model.retry() } })
            } else {
                Notice(tone: .neutral, keyword: "Accepted", sentence: "Waiting for sync to show the change.")
                    .accessibilityElement(children: .combine)
            }
        } else if let confirmed = model.confirmed {
            Notice(
                tone: .neutral, keyword: "Recorded",
                sentence: confirmed.recorded
                    ? "Revision \(confirmed.revision) is the workflow policy now."
                    : "Revision \(confirmed.revision) already held this value."
            )
            .accessibilityElement(children: .combine)
        }
    }
}
