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
                        ForEach(Self.detailRows(facts), id: \.label) { row in
                            LabeledContent(row.label, value: row.value)
                        }
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
                    .disabled(!model.canSubmit)
                }
                .listRowBackground(Color.ground2)
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
    }

    /// The four pairing facts as the operator reads them (plan §5.14): what
    /// is being joined, when the code dies, how the phone reaches the
    /// daemon, and what it may do once paired.
    static func detailRows(_ facts: Components.Schemas.PairingFacts) -> [DetailRow] {
        [
            DetailRow(label: "Host", value: facts.host_display_name),
            DetailRow(label: "Code expiry", value: facts.code_expires_at.formatted(.iso8601)),
            DetailRow(label: "Connection", value: PairingModel.connectionLabel(facts.connection_mode)),
            DetailRow(label: "Access", value: PairingModel.scopeLabel(facts.granted_scope)),
        ]
    }

    /// The project-owned pairing composition without Form and TextField,
    /// whose AppKit-backed controls ImageRenderer cannot draw off-screen.
    @ViewBuilder
    func screenshotContent() -> some View {
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
                    ForEach(Self.detailRows(facts), id: \.label) { row in
                        if screenshotDynamicTypeSize.isAccessibilitySize {
                            // Stacked so a timestamp never wraps mid-token.
                            VStack(alignment: .leading, spacing: 2) {
                                Text(row.label)
                                    .foregroundStyle(Color.inkDim)
                                Text(row.value)
                            }
                            .font(FreesideFont.body)
                        } else {
                            HStack(alignment: .firstTextBaseline) {
                                Text(row.label)
                                    .foregroundStyle(Color.inkDim)
                                Spacer(minLength: 12)
                                Text(row.value)
                                    .multilineTextAlignment(.trailing)
                            }
                            .font(FreesideFont.body)
                        }
                    }
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
            Text("Pair this device")
                .font(FreesideFont.sans(.body, weight: .semibold))
                .frame(maxWidth: .infinity)
                .padding(.vertical, 10)
                .freesideCard(border: model.canSubmit ? .accentBorder : .rule)
        }
        .padding(24)
        .frame(maxWidth: 560, alignment: .leading)
        .foregroundStyle(Color.ink)
    }

    /// The size the screenshot composition renders at: the regression
    /// test's pinned size when one is set, since the environment is not
    /// populated when `screenshotContent()` is called outside `body`.
    private var screenshotDynamicTypeSize: DynamicTypeSize {
        FreesideFont.screenshotDynamicTypeSize ?? dynamicTypeSize
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
