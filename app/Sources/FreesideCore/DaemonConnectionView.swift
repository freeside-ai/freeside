import SwiftUI

/// First launch on a device that has no local daemon or saved deployment.
/// No client, credentials, or sample data are created until a server is chosen.
struct DaemonConnectionView: View {
    @Binding var address: String
    let onConnect: (URL) -> Void

    private var serverURL: URL? { AppSession.serverURL(from: address) }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Text("Enter the address of the Freeside daemon running on your Mac.")
                    addressField
                } header: {
                    Text("Daemon Address")
                } footer: {
                    Text("Use the Mac's Tailscale address and daemon port. You'll pair this device next.")
                        .foregroundStyle(Color.inkDim)
                }
                .listRowBackground(Color.ground2)
                Section {
                    Button("Continue", action: connect)
                        .buttonStyle(FreesideActionButtonStyle(tone: .primary))
                        .disabled(serverURL == nil)
                }
                .listRowInsets(EdgeInsets())
                .listRowBackground(Color.clear)
            }
            .formStyle(.grouped)
            .font(FreesideFont.body)
            .foregroundStyle(Color.ink)
            .tint(.accentText)
            .scrollContentBackground(.hidden)
            .background(Color.ground)
            .navigationTitle("Connect to Freeside")
        }
    }

    private var addressField: some View {
        TextField("http://your-mac-address:7331", text: $address)
            .autocorrectionDisabled()
            .onSubmit(connect)
            #if os(iOS)
                .keyboardType(.URL)
                .textInputAutocapitalization(.never)
            #endif
    }

    private func connect() {
        guard let serverURL else { return }
        onConnect(serverURL)
    }
}
