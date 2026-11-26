import FreesideAPI
import SwiftUI

/// Settings' Workflow pane on both platforms (`SettingsPaneView`): the
/// per-effect gates and the follow-up filing caps (plan §3.2), read from
/// sync and changed as a saved command. General lists every setting, with
/// the value that wins (general's, else the default) and where it came
/// from. Each project the daemon serves lists only the settings its own
/// layer overrides, each with a reset to general, then how many follow
/// general and a menu that overrides one more. Each row's line says what
/// its value does today. Each control saves at once.
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
                    if let project = section.projectID {
                        projectSection(project)
                    } else {
                        SettingsGroup(elements: model.settings(in: section), id: \.key) { setting in
                            row(setting, in: section)
                        }
                    }
                }
            }
        }
    }

    /// A project's overrides, then the count that follows general and the
    /// menu that adds an override, which go once every setting is
    /// overridden.
    @ViewBuilder private func projectSection(_ project: String) -> some View {
        let overrides = model.overrides(projectID: project)
        if !overrides.isEmpty {
            SettingsGroup(elements: overrides, id: \.key) { setting in
                row(setting, in: .project(project))
            }
        }
        if let line = Self.followingGeneralLine(
            following: model.followingGeneralCount(projectID: project), overridden: overrides.count)
        {
            // The control drops under the line where both don't fit on one.
            ViewThatFits(in: .horizontal) {
                HStack(spacing: 16) {
                    followingGeneral(line)
                    Spacer(minLength: 0)
                    overrideMenu(project)
                }
                VStack(alignment: .leading, spacing: 8) {
                    followingGeneral(line)
                    overrideMenu(project)
                }
            }
        }
    }

    private func followingGeneral(_ line: String) -> some View {
        Text(line)
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }

    @ViewBuilder private func overrideMenu(_ project: String) -> some View {
        let choices = model.overridable(projectID: project)
        if !choices.isEmpty {
            let label = SettingsPopupLabel(text: "Override a Setting")
            Group {
                if rendersInteractiveControls {
                    Menu {
                        ForEach(choices, id: \.key) { setting in
                            Button(Self.label(setting.key)) {
                                Task { await model.addOverride(setting.key, projectID: project) }
                            }
                        }
                    } label: {
                        label
                    }
                    .menuStyle(.button)
                    .buttonStyle(.plain)
                    .menuIndicator(.hidden)
                    .accessibilityLabel("Override a setting for \(project)")
                } else {
                    label
                }
            }
            .disabled(!model.canEdit)
        }
    }

    /// The line under a project's overrides: how many of its settings
    /// follow general, or nil once it overrides every one.
    static func followingGeneralLine(following: Int, overridden: Int) -> String? {
        switch (following, overridden) {
        case (0, _): nil
        case (let count, 0): "All \(count) settings follow General."
        case (1, _): "The other setting follows General."
        case (let count, _): "The other \(count) settings follow General."
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

    /// The row's dim line: the winning value, its layer, who set it when,
    /// and what the value does today where that isn't plain from the row.
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
        if let clause = Self.todayClause(setting) {
            parts.append(clause)
        }
        return parts.joined(separator: " · ")
    }

    /// What an off gate does today, which its title alone misstates: the
    /// daemon still opens a filing's card while gates.follow_up_filing is
    /// off, because filing without one waits on the trust profile accepting
    /// the repository's audited issues workflows
    /// (daemon/internal/engine/follow_up_filing.go), and it approves an
    /// eligible comment by policy while gates.issue_comment is off
    /// (daemon/internal/engine/issue_comment_proposal.go). An on gate, the
    /// closure gate, and a cap do what the row says.
    static func todayClause(_ setting: Components.Schemas.WorkflowPolicySetting) -> String? {
        guard setting.value != "true" else { return nil }
        switch setting.key {
        case .gates_period_follow_up_filing:
            return "a filing still asks you today; off takes effect once tasks can file on their own"
        case .gates_period_issue_comment: return "an eligible comment is approved without asking you"
        case .gates_period_source_issue_closure, .follow_up_filing_period_max_per_day,
            .follow_up_filing_period_max_per_run, .follow_up_filing_period_max_depth, .unknown:
            return nil
        }
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
        case .follow_up_filing_period_max_per_run: "Follow-up issues and comments per task run"
        case .follow_up_filing_period_max_depth: "Follow-up depth"
        case .unknown: "Unknown setting"
        }
    }

    /// A gate reads as the path it takes; a cap as its number.
    static func valueText(_ setting: Components.Schemas.WorkflowPolicySetting) -> String {
        guard isGate(setting.key) else { return setting.value }
        return setting.value == "true" ? "Ask first" : "Off"
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
