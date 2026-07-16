import FreesideAPI
import Observation

/// The pairing exchange (plan §5.14 devices): a short-lived code read
/// off the daemon host buys this device its credential. The grant is
/// the token's only appearance, ever, so custody moves to the
/// credential store inside the same operation; a grant whose credential
/// cannot be stored is surfaced as exactly that, because the token is
/// already unrecoverable and only revoke-and-repair fixes it.
@MainActor
@Observable
public final class PairingModel {
    public enum PhaseState: Equatable {
        case idle
        case pairing
        case failed(String)
    }

    public var pairingCode = ""
    public var displayName = ""
    public private(set) var phase: PhaseState = .idle

    private let client: any APIProtocol
    private let credentials: any DeviceCredentialStore

    public init(client: any APIProtocol, credentials: any DeviceCredentialStore) {
        self.client = client
        self.credentials = credentials
    }

    public var canSubmit: Bool {
        !pairingCode.isEmpty && !displayName.isEmpty && phase != .pairing
    }

    /// Exchanges the code; on success the credential is already saved.
    public func pair() async -> DeviceCredential? {
        guard canSubmit else { return nil }
        phase = .pairing
        do {
            let output = try await client.pairDevice(
                body: .json(.init(pairing_code: pairingCode, display_name: displayName)))
            switch output {
            case .created(let created):
                let grant = try created.body.json
                let credential = DeviceCredential(
                    deviceID: Self.deviceID(of: grant.device.device),
                    token: grant.device_token
                )
                do {
                    try credentials.save(credential)
                } catch {
                    phase = .failed(
                        "Paired, but the credential could not be stored; revoke this device on the daemon host and pair again."
                    )
                    return nil
                }
                phase = .idle
                return credential
            case .forbidden:
                // The daemon never says which (test 13); neither do we.
                phase = .failed("The code was not accepted: invalid, expired, or already used.")
                return nil
            case .undocumented(let statusCode, _):
                phase = .failed("The daemon answered \(statusCode).")
                return nil
            }
        } catch {
            phase = .failed("Couldn't reach the daemon.")
            return nil
        }
    }

    private static func deviceID(of device: Components.Schemas.Device) -> String {
        switch device {
        case .active(let active): return active.id
        case .revoked(let revoked): return revoked.id
        }
    }
}
