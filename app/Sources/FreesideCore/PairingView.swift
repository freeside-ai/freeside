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
                    LabeledContent("Host", value: "Freeside daemon")
                    LabeledContent("Code expiry", value: "Expires shortly")
                    LabeledContent("Connection", value: "Local or relayed")
                } header: {
                    Text("Pairing details")
                } footer: {
                    Text(
                        "Exact host identity, code expiry, and connection mode appear here when the daemon provides them."
                    )
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.inkDim)
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
