import FreesideAPI
import SwiftUI

/// The Devices pane of Settings: every device paired with this daemon as
/// grouped rows, this device first with a faint chip, revoked ones grouped
/// apart, each active device's notifications switch, and revocation behind a
/// confirmation. The window and the iPhone's
/// pushed page host it (`SettingsPaneView`) and own the model's loading.
struct DevicesView: View {
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    let model: DevicesModel
    /// The daemon's host, for the pane's line; nil reads "this daemon".
    let host: String?
    @State private var confirming: DevicesModel.Row?
    /// Actions made under this pairing whose delivery is unresolved; the
    /// confirmation for revoking this device says they will not be retried.
    let unresolvedActionCount: Int
    var client: SettingsClient = .current
    var rendersInteractiveControls = true

    var body: some View {
        content
            .confirmationDialog(
                confirming.map { Self.confirmationTitle(for: $0) } ?? "",
                isPresented: Binding(
                    get: { confirming != nil },
                    set: { if !$0 { confirming = nil } }),
                titleVisibility: .visible,
                presenting: confirming
            ) { row in
                Button(row.isCurrent ? "Revoke and Sign Out" : "Revoke", role: .destructive) {
                    Task { await model.revoke(row.id) }
                }
                Button("Cancel", role: .cancel) {}
            } message: { row in
                Text(Self.confirmationMessage(for: row, unresolvedActionCount: unresolvedActionCount))
            }
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: SettingsPaneInsets.sectionSpacing(client)) {
            SettingsPaneHead(title: "Devices", line: "Paired with \(host ?? "this daemon").")
            if let failure = model.revokeFailure {
                Notice(tone: .wax, keyword: "Failed", sentence: failure)
                    .accessibilityElement(children: .combine)
            }
            if let failure = model.notificationsFailure {
                Notice(tone: .wax, keyword: "Failed", sentence: failure)
                    .accessibilityElement(children: .combine)
            }
            switch model.loadState {
            case .loading:
                Text("Loading devices…")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            case .failed(let reason):
                Notice(tone: .wax, keyword: "Failed", sentence: reason)
                    .accessibilityElement(children: .combine)
            case .loaded:
                let active = [model.currentDevice].compactMap(\.self) + model.otherDevices
                if !active.isEmpty {
                    SettingsGroup(active) { row in
                        activeRow(row)
                    }
                }
                if !model.revokedDevices.isEmpty {
                    SettingsSection(keyword: "Revoked") {
                        SettingsGroup(model.revokedDevices) { row in
                            SettingsRow(label: row.name, line: revokedLine(row))
                                .foregroundStyle(Color.inkDim)
                                .exactInstants(revokedInstants(row))
                        }
                    }
                }
            }
            HStack {
                Spacer(minLength: 0)
                Button("Refresh") {
                    Task { await model.load() }
                }
                .buttonStyle(FreesideActionButtonStyle(tone: .tertiary, expands: false))
            }
        }
    }

    /// An active device: when it paired and was last seen, the chip that
    /// marks this device, and Revoke… behind the confirmation, with its
    /// notifications switch on a labelled line below, where a phone-width
    /// row has room for it.
    private func activeRow(_ row: DevicesModel.Row) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            activeHead(row)
            HStack(spacing: 16) {
                Text("Notifications")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .accessibilityHidden(true)
                Spacer(minLength: 0)
                DeviceNotificationsSwitch(
                    model: model, row: row, rendersInteractiveControls: rendersInteractiveControls)
            }
            .padding(.horizontal, 14)
            .padding(.bottom, 10)
        }
    }

    private func activeHead(_ row: DevicesModel.Row) -> some View {
        SettingsRow(label: row.name, line: activeLine(row)) {
            HStack(spacing: 10) {
                if row.isCurrent {
                    StateChip(label: "This Device", cut: .faint)
                }
                Button(model.revokingID == row.id ? "Revoking…" : "Revoke…") {
                    confirming = row
                }
                .font(FreesideFont.noticeAction)
                .foregroundStyle(Color.waxText)
                .buttonStyle(.plain)
                .disabled(model.revokingID != nil)
                .accessibilityLabel("Revoke \(row.name)")
            }
        }
        .exactInstants(activeInstants(row))
    }

    private func short(_ date: Date) -> String {
        FreesideFormat.shortTime(date, now: pinnedNow ?? .now, locale: locale, timeZone: timeZone)
    }

    /// "Paired … · last seen …", with "never" for a device that has made no
    /// authenticated request since the daemon began recording activity.
    private func activeLine(_ row: DevicesModel.Row) -> String {
        "Paired \(short(row.pairedAt)) · last seen \(row.lastSeenAt.map(short) ?? "never")"
    }

    private func activeInstants(_ row: DevicesModel.Row) -> [(label: String, date: Date)] {
        [("Paired", row.pairedAt)] + (row.lastSeenAt.map { [("Last seen", $0)] } ?? [])
    }

    private func revokedLine(_ row: DevicesModel.Row) -> String? {
        row.revokedAt.map { "Revoked \(short($0))" }
    }

    private func revokedInstants(_ row: DevicesModel.Row) -> [(label: String, date: Date)] {
        [("Paired", row.pairedAt)] + (row.revokedAt.map { [("Revoked", $0)] } ?? [])
    }

    static func confirmationTitle(for row: DevicesModel.Row) -> String {
        row.isCurrent ? "Revoke this device?" : "Revoke \(row.name)?"
    }

    /// The confirmation copy. Revoking this device also deletes its stored
    /// credential, so the copy says the app signs out and, as the re-pair
    /// confirmation does, that unresolved actions will not be retried.
    static func confirmationMessage(for row: DevicesModel.Row, unresolvedActionCount: Int) -> String {
        guard row.isCurrent else {
            return
                "It stops reading from and acting on this daemon right away. It can connect again only by pairing with a new code."
        }
        let base =
            "This app signs out and returns to pairing. Connecting again takes a new pairing code from the daemon's host."
        guard unresolvedActionCount > 0 else { return base }
        let actions =
            unresolvedActionCount == 1 ? "1 unresolved action" : "\(unresolvedActionCount) unresolved actions"
        return "\(base) \(actions) made under this pairing won't be retried."
    }
}

/// One device's notifications switch, on the Devices and Notifications
/// panes. While a change is in flight it shows the requested value, the
/// opposite of the stored one, and takes no other change.
struct DeviceNotificationsSwitch: View {
    let model: DevicesModel
    let row: DevicesModel.Row
    var rendersInteractiveControls = true

    private var isOn: Bool {
        model.notificationsChangingID == row.id ? !row.notificationsEnabled : row.notificationsEnabled
    }

    var body: some View {
        if rendersInteractiveControls {
            Toggle(
                "Notifications",
                isOn: Binding(
                    get: { isOn },
                    set: { enabled in Task { await model.setNotifications(row.id, enabled: enabled) } })
            )
            .labelsHidden()
            .toggleStyle(.switch)
            .disabled(model.notificationsChangingID != nil)
            .accessibilityLabel("Notifications for \(row.name)")
        } else {
            SettingsToggleStandIn(isOn: isOn)
        }
    }
}
