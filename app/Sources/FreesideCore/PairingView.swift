import FreesideAPI
import SwiftUI

/// The device's front door until it holds a credential: the code shown
/// by the daemon host plus a human label, exchanged once.
struct PairingView: View {
    @Bindable var model: PairingModel
    let onPaired: (DeviceCredential) -> Void

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("Pairing code", text: $model.pairingCode)
                        .autocorrectionDisabled()
                        .font(FreesideFont.monoCallout)
                    TextField("Device name", text: $model.displayName)
                } footer: {
                    Text(
                        "Run the pairing command on the daemon host and enter the code it displays. The code works once and expires quickly."
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
        }
    }
}
