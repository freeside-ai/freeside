import FreesideAPI
import SwiftUI

#if os(macOS)
    import AppKit
#elseif os(iOS)
    import UIKit
#endif

/// The device's front door until it holds a credential: the code shown
/// by the daemon host plus a human label, exchanged once.
struct PairingView: View {
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @Bindable var model: PairingModel
    var onChangeServer: (() -> Void)? = nil
    let onPaired: (DeviceCredential) -> Void

    var body: some View {
        NavigationStack {
            ScrollView {
                content(now: nil, rendersInteractiveControls: true)
                    .frame(maxWidth: .infinity)
            }
            .background(Color.ground)
            .toolbar {
                if let onChangeServer {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Change server", action: onChangeServer)
                            .disabled(model.phase == .pairing)
                    }
                }
            }
            #if os(macOS)
                .navigationTitle("Pair with Freeside")
            #else
                // The ask names the screen, so the bar carries no title and
                // stays only while it has Change server to hold.
                .navigationBarTitleDisplayMode(.inline)
                .toolbar(onChangeServer == nil ? .hidden : .visible, for: .navigationBar)
            #endif
        }
        // One preview per pause in typing: the task restarts on every code
        // change, so keystrokes inside the delay never reach the daemon, and
        // the details refresh once the operator stops.
        .task(id: model.pairingCode) {
            try? await Task.sleep(for: .milliseconds(400))
            guard !Task.isCancelled else { return }
            await model.refreshFacts()
        }
    }

    static let instructions =
        "Run the pairing command on the daemon host and enter its one-time code. "
        + "The device name appears in Devices on the host and in the audit record "
        + "of every decision made from this device."

    /// The one pairing composition, drawn live and by the screenshot suite.
    /// `now` fixes the clock the expiry row reads; nil lets it tick.
    /// `rendersInteractiveControls` false draws each field as static text,
    /// because `ImageRenderer` cannot draw an AppKit-backed text field
    /// off-screen.
    private func content(now: Date?, rendersInteractiveControls: Bool) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            VStack(alignment: .leading, spacing: 12) {
                KeywordLabel(text: "Pairing")
                Text("Pair this device")
                    .font(FreesideFont.ask)
                    .accessibilityAddTraits(.isHeader)
            }
            Text(Self.instructions)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
            VStack(alignment: .leading, spacing: 16) {
                labeled("Code") {
                    if rendersInteractiveControls {
                        if dynamicTypeSize.isAccessibilitySize {
                            fieldBox { pairingCodeField }
                            pasteButton.frame(maxWidth: .infinity, alignment: .trailing)
                        } else {
                            HStack(spacing: 12) {
                                fieldBox { pairingCodeField }
                                pasteButton
                            }
                        }
                    } else {
                        fieldBox {
                            Text(model.formattedPairingCode)
                                .font(FreesideFont.monoValue)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }
                    }
                }
                labeled("Device name") {
                    fieldBox {
                        if rendersInteractiveControls {
                            TextField("Device name", text: $model.displayName)
                                .textFieldStyle(.plain)
                                .font(FreesideFont.cardBody)
                                .accessibilityLabel("Device name")
                        } else {
                            Text(model.displayName)
                                .font(FreesideFont.cardBody)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }
                    }
                }
            }
            VStack(alignment: .leading, spacing: 11) {
                KeywordLabel(text: "Host facts")
                    .accessibilityAddTraits(.isHeader)
                if let facts = model.facts {
                    Self.detailsContent(facts, now: now)
                } else {
                    Text("Enter a code to see host details")
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            if case .failed(let message) = model.phase {
                Notice(tone: .wax, keyword: "Failed", sentence: message)
                    .accessibilityElement(children: .combine)
            }
            Button {
                Task {
                    if let credential = await model.pair() {
                        onPaired(credential)
                    }
                }
            } label: {
                if model.phase == .pairing {
                    ProgressView()
                } else {
                    Text("Pair")
                }
            }
            .buttonStyle(FreesideActionButtonStyle(tone: .primary))
            .disabled(!model.canSubmit)
        }
        .padding(.horizontal, 24)
        .padding(.vertical, 20)
        .frame(maxWidth: 528, alignment: .leading)
        .foregroundStyle(Color.ink)
        .tint(.accentText)
    }

    /// A field under its visible keyword. Each control carries the same
    /// words as its accessibility label, so VoiceOver reads the label once.
    private func labeled(_ label: String, @ViewBuilder field: () -> some View) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            KeywordLabel(text: label)
                .accessibilityHidden(true)
            field()
        }
    }

    private func fieldBox(@ViewBuilder field: () -> some View) -> some View {
        field()
            .padding(.horizontal, 10)
            .padding(.vertical, 8)
            .background(Color.ground2, in: RoundedRectangle(cornerRadius: 6))
            .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(Color.rule, lineWidth: 1))
            .freesideFocusRing()
    }

    struct DetailRow {
        let label: String
        let value: String
        var valueColor: Color? = nil
        var exact: String? = nil
    }

    static func expiryText(until expiry: Date, at now: Date) -> String {
        let remaining = expiry.timeIntervalSince(now)
        guard remaining > 0 else { return "expired" }
        guard remaining >= 60 else { return "in under a minute" }
        return "in \(Int(remaining / 60)) min"
    }

    static func expiryIsUrgent(until expiry: Date, at now: Date) -> Bool {
        expiry.timeIntervalSince(now) < 5 * 60
    }

    private static func expiryRow(until expiry: Date, at now: Date) -> DetailRow {
        DetailRow(
            label: "Code expires",
            value: expiryText(until: expiry, at: now),
            valueColor: expiryIsUrgent(until: expiry, at: now) ? .accentText : nil,
            exact: expiry.formatted(.iso8601))
    }

    /// The four pairing facts as the operator reads them (plan §5.14): what
    /// is being joined, when the code dies, how the phone reaches the
    /// daemon, and what it may do once paired.
    static func detailRows(_ facts: Components.Schemas.PairingFacts, now: Date) -> [DetailRow] {
        [
            DetailRow(label: "Host", value: facts.host_display_name),
            expiryRow(until: facts.code_expires_at, at: now),
            DetailRow(label: "Connection", value: PairingModel.connectionLabel(facts.connection_mode)),
            DetailRow(label: "Access", value: PairingModel.scopeLabel(facts.granted_scope)),
        ]
    }

    /// A fixed clock renders evidence; the live form ticks only its expiry row.
    @ViewBuilder
    static func detailsContent(_ facts: Components.Schemas.PairingFacts, now: Date? = nil) -> some View {
        ForEach(detailRows(facts, now: now ?? Date()), id: \.label) { row in
            if row.exact != nil, now == nil {
                TimelineView(.periodic(from: .now, by: 1)) { _ in
                    // The schedule is only a tick. Its entry can be ahead of
                    // the wall clock, which would expire the code early.
                    detailContent(expiryRow(until: facts.code_expires_at, at: Date()))
                }
            } else {
                detailContent(row)
            }
        }
    }

    @ViewBuilder
    private static func detailContent(_ row: DetailRow) -> some View {
        let content = FactRow(label: row.label, value: row.value, valueColor: row.valueColor ?? .ink)
        if let exact = row.exact {
            #if os(macOS)
                content.help(exact)
            #elseif os(iOS)
                content.contextMenu {
                    Button("Copy \(exact)") {
                        UIPasteboard.general.string = exact
                    }
                }
            #endif
        } else {
            content
        }
    }

    /// The pairing composition with static fields and a fixed clock, for
    /// the screenshot suite.
    func screenshotContent(now: Date) -> some View {
        content(now: now, rendersInteractiveControls: false)
    }

    private var pasteButton: some View {
        Button("Paste") {
            if let value = clipboardString {
                model.applyPairingCodeInput(value)
            }
        }
        .buttonStyle(.plain)
        .font(FreesideFont.noticeAction)
        .foregroundStyle(Color.accentText)
        .freesideFocusRing(cornerRadius: 4)
    }

    @ViewBuilder private var pairingCodeField: some View {
        let field = TextField(
            "Pairing code",
            text: Binding(
                get: { model.formattedPairingCode },
                set: { model.applyPairingCodeInput($0) })
        )
        .textFieldStyle(.plain)
        .textContentType(.oneTimeCode)
        .autocorrectionDisabled()
        .font(FreesideFont.monoValue)
        .accessibilityLabel("Pairing code")

        #if os(iOS)
            field
                .keyboardType(.asciiCapable)
                .textInputAutocapitalization(.characters)
        #else
            field
        #endif
    }

    private var clipboardString: String? {
        #if os(macOS)
            NSPasteboard.general.string(forType: .string)
        #elseif os(iOS)
            UIPasteboard.general.string
        #endif
    }
}
