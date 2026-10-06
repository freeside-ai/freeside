import FreesideAPI
import SwiftUI

/// The Devices screen: every device paired with this daemon, with this
/// device first and revoked ones grouped apart, and revocation behind a
/// confirmation. Presented as a sheet on both platforms.
struct DevicesView: View {
    @Environment(\.dismiss) private var dismiss
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    @State private var model: DevicesModel
    @State private var confirming: DevicesModel.Row?
    /// Actions made under this pairing whose delivery is unresolved; the
    /// confirmation for revoking this device says they will not be retried.
    let unresolvedActionCount: Int
    var rendersInteractiveControls = true

    init(model: DevicesModel, unresolvedActionCount: Int = 0, rendersInteractiveControls: Bool = true) {
        _model = State(initialValue: model)
        self.unresolvedActionCount = unresolvedActionCount
        self.rendersInteractiveControls = rendersInteractiveControls
    }

    var body: some View {
        VStack(spacing: 0) {
            if rendersInteractiveControls {
                ScrollView { content }
            } else {
                content
            }
            Divider().overlay(Color.rule)
            HStack(spacing: 12) {
                Button("Refresh") {
                    Task { await model.load() }
                }
                .font(FreesideFont.callout)
                .buttonStyle(FreesideActionButtonStyle(tone: .tertiary))
                Spacer()
                Button("Close") { dismiss() }
                    .font(FreesideFont.callout)
                    .buttonStyle(FreesideActionButtonStyle(tone: .secondary, expands: false))
                    .keyboardShortcut(.cancelAction)
            }
            .padding(16)
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
        .frame(minWidth: 380, minHeight: 320)
        // On appear, and again when the heartbeat observes a newer revision.
        .task(id: model.observedRevision) {
            guard rendersInteractiveControls else { return }
            await model.loadIfStale()
        }
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
        VStack(alignment: .leading, spacing: 0) {
            FreesideSheetHeader(
                title: "Devices",
                prompt: "Devices paired with this daemon. A revoked device can't read or act until it pairs again.")
            VStack(alignment: .leading, spacing: 16) {
                if let failure = model.revokeFailure {
                    Text(failure)
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.waxText)
                        .fixedSize(horizontal: false, vertical: true)
                }
                switch model.loadState {
                case .loading:
                    Text("Loading devices…")
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.inkDim)
                case .failed(let reason):
                    Text(reason)
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.waxText)
                        .fixedSize(horizontal: false, vertical: true)
                case .loaded:
                    if let current = model.currentDevice {
                        section("This device", rows: [current])
                    }
                    if !model.otherDevices.isEmpty {
                        section("Other devices", rows: model.otherDevices)
                    }
                    if !model.revokedDevices.isEmpty {
                        section("Revoked", rows: model.revokedDevices)
                    }
                }
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
        }
    }

    private func section(_ title: String, rows: [DevicesModel.Row]) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            KeywordLabel(text: title)
                .accessibilityAddTraits(.isHeader)
            ForEach(rows) { row in
                card(row)
            }
        }
    }

    private func card(_ row: DevicesModel.Row) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            VStack(alignment: .leading, spacing: 4) {
                Text(row.name)
                    .font(FreesideFont.callout.weight(.semibold))
                    .foregroundStyle(row.isRevoked ? Color.inkDim : Color.ink)
                    .fixedSize(horizontal: false, vertical: true)
                fact("Paired", row.pairedAt)
                if let lastSeenAt = row.lastSeenAt {
                    fact("Last seen", lastSeenAt)
                } else {
                    // No authenticated request since the daemon began
                    // recording activity.
                    fact("Last seen", value: "Never")
                }
                if let revokedAt = row.revokedAt {
                    fact("Revoked", revokedAt)
                }
            }
            .accessibilityElement(children: .combine)
            if !row.isRevoked {
                Button(model.revokingID == row.id ? "Revoking…" : "Revoke") {
                    confirming = row
                }
                .font(FreesideFont.callout)
                // Hugs its label: a destructive control should not be the
                // widest thing on the card.
                .buttonStyle(FreesideActionButtonStyle(tone: .destructive, expands: false))
                .disabled(model.revokingID != nil)
                .accessibilityLabel("Revoke \(row.name)")
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(16)
        .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
        .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
    }

    /// One recorded instant in the shared short time format, with the exact
    /// instant a hover or long press away.
    private func fact(_ label: String, _ date: Date) -> some View {
        fact(
            label,
            value: FreesideFormat.shortTime(
                date, now: pinnedNow ?? .now, locale: locale, timeZone: timeZone)
        )
        .exactInstant(date)
    }

    /// One paragraph, so a long value wraps under its label at large type
    /// sizes rather than squeezing beside it.
    private func fact(_ label: String, value: String) -> some View {
        Text("\(Text(label).foregroundStyle(Color.inkDim)) \(Text(value).foregroundStyle(Color.ink))")
            .font(FreesideFont.caption)
            .fixedSize(horizontal: false, vertical: true)
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
