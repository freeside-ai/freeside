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
                    TextField("Device name", text: $model.displayName)
                } footer: {
                    Text(
                        "Run the pairing command on the daemon host and enter the code it displays. The code works once and expires quickly."
                    )
                }
                if case .failed(let message) = model.phase {
                    Section {
                        Label(message, systemImage: "exclamationmark.triangle")
                            .foregroundStyle(.red)
                    }
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
            }
            .formStyle(.grouped)
            .navigationTitle("Pair with Freeside")
        }
    }
}
