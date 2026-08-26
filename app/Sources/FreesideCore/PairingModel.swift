import Foundation
import FreesideAPI
import Observation

#if os(iOS)
    import UIKit
#endif

/// The pairing exchange (plan §5.14 devices): a short-lived code read
/// off the daemon host buys this device its private grant. The token and ntfy
/// subscription appear only there, so custody moves to the credential store
/// inside the same operation; a grant whose values cannot be validated or
/// stored is surfaced as exactly that, because only revoke-and-repair fixes it.
@MainActor
@Observable
public final class PairingModel {
    public enum PhaseState: Equatable {
        case idle
        case pairing
        case failed(String)
    }

    public var pairingCode: String {
        didSet {
            if !isApplyingPairingCodePrefill {
                pairingCodeWasEdited = true
            }
        }
    }
    public var displayName: String
    public private(set) var phase: PhaseState = .idle

    private let client: any APIProtocol
    private let credentials: any DeviceCredentialStore
    private var isApplyingPairingCodePrefill = false
    private var pairingCodeWasEdited = false

    public init(
        client: any APIProtocol,
        credentials: any DeviceCredentialStore,
        pairingCode: String = "",
        displayName: String? = nil
    ) {
        self.client = client
        self.credentials = credentials
        self.pairingCode = Self.canonicalPairingCode(pairingCode)
        self.displayName = displayName ?? Self.systemDeviceName()
    }

    public var canSubmit: Bool {
        !Self.canonicalPairingCode(pairingCode).isEmpty && !displayName.isEmpty
            && phase != .pairing
    }

    public func prefillPairingCode(_ code: String) {
        guard !pairingCodeWasEdited else { return }
        isApplyingPairingCodePrefill = true
        defer { isApplyingPairingCodePrefill = false }
        pairingCode = Self.canonicalPairingCode(code)
    }

    public func clearPairingCodePrefill() {
        prefillPairingCode("")
    }

    public var formattedPairingCode: String {
        Self.groupPairingCode(Self.canonicalPairingCode(pairingCode))
    }

    public func applyPairingCodeInput(_ code: String) {
        pairingCode = Self.canonicalPairingCode(code)
    }

    /// Exchanges the code; on success the credential is already saved.
    public func pair() async -> DeviceCredential? {
        guard canSubmit else { return nil }
        let canonicalPairingCode = Self.canonicalPairingCode(pairingCode)
        if pairingCode != canonicalPairingCode {
            pairingCode = canonicalPairingCode
        }
        phase = .pairing
        do {
            let output = try await client.pairDevice(
                body: .json(.init(pairing_code: canonicalPairingCode, display_name: displayName)))
            switch output {
            case .created(let created):
                let grant = try created.body.json
                let deviceID = Self.deviceID(of: grant.device.device)
                guard
                    let subscription = DeviceNtfySubscription(
                        serverURL: grant.ntfy_subscription.server_url,
                        topic: grant.ntfy_subscription.topic
                    ),
                    let credential = DeviceCredential(
                        deviceID: deviceID,
                        token: grant.device_token,
                        ntfySubscription: subscription
                    )
                else {
                    phase = .failed(
                        "Paired, but the private grant was invalid; revoke this device on the daemon host and pair again."
                    )
                    return nil
                }
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

    static func canonicalPairingCode(_ typed: String) -> String {
        let trimmed = typed.trimmingCharacters(in: .whitespacesAndNewlines)
        return String(
            trimmed.uppercased().compactMap { character in
                switch character {
                case "-", " ": nil
                case "O": "0"
                case "I", "L": "1"
                default: character
                }
            })
    }

    static func groupPairingCode(_ canonical: String) -> String {
        guard !canonical.isEmpty else { return "" }
        var groups: [String] = []
        var start = canonical.startIndex
        while start < canonical.endIndex {
            let end =
                canonical.index(start, offsetBy: 4, limitedBy: canonical.endIndex)
                ?? canonical.endIndex
            groups.append(String(canonical[start..<end]))
            start = end
        }
        return groups.joined(separator: "-")
    }

    private static func systemDeviceName() -> String {
        #if os(iOS)
            let name = UIDevice.current.name.trimmingCharacters(in: .whitespacesAndNewlines)
            return name.isEmpty ? UIDevice.current.model : name
        #elseif os(macOS)
            let name = Host.current().localizedName?
                .trimmingCharacters(in: .whitespacesAndNewlines)
            if let name, !name.isEmpty {
                return name
            }
            return ProcessInfo.processInfo.hostName
        #else
            return ProcessInfo.processInfo.hostName
        #endif
    }
}
