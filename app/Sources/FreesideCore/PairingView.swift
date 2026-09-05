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
    let onPaired: (DeviceCredential) -> Void

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    if dynamicTypeSize.isAccessibilitySize {
                        pairingCodeField
                        pasteButton.frame(maxWidth: .infinity, alignment: .trailing)
                    } else {
                        HStack {
                            pairingCodeField
                            pasteButton
                        }
                    }
                } footer: {
                    Text("Run the pairing command on the daemon host and enter its one-time code.")
                        .font(FreesideFont.caption)
                        .foregroundStyle(Color.inkDim)
                }
                .listRowBackground(Color.ground2)
                Section {
                    TextField("Device name", text: $model.displayName)
                } footer: {
                    Text(
                        "This name appears in Devices on the host and in the audit record of every decision made from this device."
                    )
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.inkDim)
                }
                .listRowBackground(Color.ground2)
                Section {
                    if let facts = model.facts {
                        Self.detailsContent(facts)
                    } else {
                        Text("Enter a code to see host details")
                            .foregroundStyle(Color.inkDim)
                    }
                } header: {
                    Text("Pairing details")
                }
                .listRowBackground(Color.ground2)
                if case .failed(let message) = model.phase {
                    Section {
                        Label(message, systemImage: "exclamationmark.triangle")
                            .foregroundStyle(Color.waxText)
                    }
                    .listRowBackground(Color.ground2)
                }
                Section {
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
                            Text("Pair this device")
                        }
                    }
                    .buttonStyle(FreesideActionButtonStyle(tone: .primary))
                    .disabled(!model.canSubmit)
                }
                // The filled control draws its own ground; a list row behind
                // it would put ground-2 inside ground-2.
                .listRowInsets(EdgeInsets())
                .listRowBackground(Color.clear)
            }
            .formStyle(.grouped)
            .font(FreesideFont.body)
            .foregroundStyle(Color.ink)
            .tint(.accentText)
            .scrollContentBackground(.hidden)
            .background(Color.ground)
            .navigationTitle("Pair with Freeside")
            #if os(iOS)
                .navigationBarTitleDisplayMode(
                    dynamicTypeSize.isAccessibilitySize ? .inline : .large)
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

    /// The project-owned pairing composition without Form and TextField,
    /// whose AppKit-backed controls ImageRenderer cannot draw off-screen.
    @ViewBuilder
    func screenshotContent(now: Date) -> some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Pair with Freeside")
                .font(FreesideFont.largeTitle)
            VStack(alignment: .leading, spacing: 6) {
                KeywordLabel(text: "Pairing code")
                Text(model.formattedPairingCode)
                    .font(FreesideFont.monoCallout)
                Divider()
                KeywordLabel(text: "Device name")
                Text(model.displayName)
                    .font(FreesideFont.body)
            }
            .padding(14)
            .freesideCard()
            VStack(alignment: .leading, spacing: 6) {
                KeywordLabel(text: "Pairing details")
                if let facts = model.facts {
                    Self.detailsContent(facts, now: now)
                        .font(FreesideFont.body)
                        .foregroundStyle(Color.inkDim)
                } else {
                    Text("Enter a code to see host details")
                        .font(FreesideFont.body)
                        .foregroundStyle(Color.inkDim)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(14)
            .freesideCard()
            Text(
                "Run the pairing command on the daemon host and enter its one-time code."
            )
            .font(FreesideFont.caption)
            .foregroundStyle(Color.inkDim)
            Button("Pair this device") {}
                .buttonStyle(FreesideActionButtonStyle(tone: .primary))
                .disabled(!model.canSubmit)
        }
        .padding(24)
        .frame(maxWidth: 560, alignment: .leading)
        .foregroundStyle(Color.ink)
    }

    private var pasteButton: some View {
        Button {
            if let value = clipboardString {
                model.applyPairingCodeInput(value)
            }
        } label: {
            Label("Paste", systemImage: "doc.on.clipboard")
        }
        .buttonStyle(.borderless)
    }

    @ViewBuilder private var pairingCodeField: some View {
        let field = TextField(
            "Pairing code",
            text: Binding(
                get: { model.formattedPairingCode },
                set: { model.applyPairingCodeInput($0) })
        )
        .textContentType(.oneTimeCode)
        .autocorrectionDisabled()
        .font(FreesideFont.monoCallout)

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
