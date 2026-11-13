import SwiftUI

/// The iPhone's Settings: a grouped list of the eight panes, pushed from the
/// bottom bar's gear, each row trailing a summary of its pane and pushing
/// it, as the 4 Nov 2026 handoff draws it (`phoneSettings`, canvas band 1).
/// The stack's back button returns to Freeside. Only iOS presents it; it
/// compiles on the Mac too so the screenshot suite renders it.
struct SettingsList: View {
    let navigation: NavigationModel
    let sync: SettingsSync
    let flowPreferences: DecisionFlowPreferences
    let host: String?
    var client: SettingsClient = .current
    var rendersInteractiveControls = true
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        Group {
            if rendersInteractiveControls {
                ScrollView { content }
            } else {
                content
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
        .background(Color.ground)
        #if os(iOS)
            .navigationTitle("Settings")
        #endif
        // Here, not in the Devices pane, because the Devices row states the
        // list. On appear, and again when the heartbeat observes a newer
        // revision.
        .task(id: sync.devices.observedRevision) {
            guard rendersInteractiveControls else { return }
            await sync.devices.loadIfStale()
        }
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: 16) {
            SettingsGroup(NavigationModel.SettingsPage.allCases) { page in
                Button {
                    navigation.pushSettingsPane(page)
                } label: {
                    row(page.title, summary: summary(page))
                }
                .buttonStyle(.plain)
            }
            // The iPhone holds no daemon version: it comes from the Mac's
            // menu-bar model (the handoff's "Values sync lacks").
            if let host {
                Text(host)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            }
        }
        .padding(EdgeInsets(top: 24, leading: 16, bottom: 16, trailing: 16))
    }

    private func summary(_ page: NavigationModel.SettingsPage) -> String {
        switch page {
        case .general: SettingsSummary.general(flowPreferences)
        case .agents: SettingsSummary.agents(sync.routing)
        case .workflow: SettingsSummary.workflow(sync.workflow)
        case .usage: SettingsSummary.usage(sync.usage)
        case .devices: SettingsSummary.devices(sync.devices, client: client)
        case .notifications: SettingsSummary.notifications(sync.devices)
        case .daemon: SettingsSummary.daemon(sync.freshness)
        case .systemHealth: SettingsSummary.systemHealth(sync.findings)
        }
    }

    /// The label, the pane's summary in dim mono, and the chevron that says
    /// the row opens. At the accessibility sizes the summary drops under
    /// the label.
    private func row(_ label: String, summary: String) -> some View {
        Group {
            if dynamicTypeSize.isAccessibilitySize {
                VStack(alignment: .leading, spacing: 3) {
                    rowLabel(label)
                    HStack(spacing: 8) {
                        rowSummary(summary)
                        Spacer(minLength: 0)
                        chevron
                    }
                }
            } else {
                HStack(spacing: 12) {
                    rowLabel(label)
                    Spacer(minLength: 0)
                    HStack(spacing: 8) {
                        rowSummary(summary)
                            .multilineTextAlignment(.trailing)
                        chevron
                    }
                }
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
    }

    private func rowLabel(_ label: String) -> some View {
        Text(label)
            .font(FreesideFont.factLabel)
            .foregroundStyle(Color.ink)
    }

    private func rowSummary(_ summary: String) -> some View {
        Text(summary)
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
    }

    private var chevron: some View {
        Text("›")
            .font(FreesideFont.factLabel)
            .foregroundStyle(Color.inkDim)
            .accessibilityHidden(true)
    }
}
