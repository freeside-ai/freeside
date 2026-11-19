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
            // Facts belong to one code; a changed code shows none until the
            // daemon answers for the new one.
            if Self.canonicalPairingCode(pairingCode) != Self.canonicalPairingCode(oldValue) {
                facts = nil
                codeRefused = false
            }
        }
    }
    public var displayName: String
    public private(set) var phase: PhaseState = .idle
    /// What the current code would join (plan §5.14 pairing facts), as the
    /// daemon last reported it for this code; nil until a preview succeeds
    /// and again whenever the code changes or the daemon rejects it. Display
    /// facts only: nothing here is a credential.
    public private(set) var facts: Components.Schemas.PairingFacts?
    /// Whether the daemon refused to preview the current code (403): it is
    /// invalid, expired, or spent, and the daemon never says which. Cleared
    /// when the code changes. A preview that failed in transit sets nothing.
    public private(set) var codeRefused = false
    /// Get Code's progress, apart from the pairing exchange's `phase`.
    public private(set) var codeRequest: CodeRequestState = .idle

    public enum CodeRequestState: Equatable {
        case idle
        case requesting
        case failed(String)
    }

    private let client: any APIProtocol
    /// Mints a code on this device's own daemon host: set only when this
    /// screen pairs with the Mac's supervised daemon (`AppSession`), so
    /// Get Code never fetches a code for another daemon.
    private let codeSource: (any PairingCodeSource)?
    private let credentials: any DeviceCredentialStore
    private var isApplyingPairingCodePrefill = false
    private var pairingCodeWasEdited = false

    public init(
        client: any APIProtocol,
        credentials: any DeviceCredentialStore,
        pairingCode: String = "",
        displayName: String? = nil,
        codeSource: (any PairingCodeSource)? = nil
    ) {
        self.client = client
        self.credentials = credentials
        self.codeSource = codeSource
        self.pairingCode = Self.canonicalPairingCode(pairingCode)
        self.displayName = displayName ?? Self.systemDeviceName()
    }

    /// Whether Pair may be pressed. The preview must have answered for the
    /// current code: a rejected or unanswered code has no facts, and pairing
    /// against one the operator has not seen described is a blind exchange.
    public var canSubmit: Bool {
        !Self.canonicalPairingCode(pairingCode).isEmpty && !displayName.isEmpty
            && phase != .pairing && facts != nil
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

    /// Whether this screen offers Get Code.
    public var canGetCode: Bool { codeSource != nil }

    /// Mints a code on this Mac's daemon and fills the field with it, as
    /// operator input, so a later readiness change can't clear it. A
    /// failure leaves the field as it was, and so does a code the operator
    /// typed or pasted while the request ran.
    public func getCode() async {
        guard let codeSource, codeRequest != .requesting else { return }
        codeRequest = .requesting
        let codeBefore = pairingCode
        do {
            let minted = try await codeSource.mint()
            if pairingCode == codeBefore { applyPairingCodeInput(minted.code) }
            codeRequest = .idle
        } catch {
            codeRequest = .failed(error.message)
        }
    }

    /// Asks the daemon what the current code would join, without redeeming
    /// it. Any rejection or failure clears the facts. A refusal also sets
    /// `codeRefused`, so the screen can say the code is dead and how to get
    /// another instead of waiting on a preview that will never come; it
    /// still can't say why, because the daemon never does (test 13).
    public func refreshFacts() async {
        let code = Self.canonicalPairingCode(pairingCode)
        guard !code.isEmpty else {
            facts = nil
            return
        }
        let previewed: Components.Schemas.PairingFacts?
        var refused = false
        do {
            switch try await client.previewPairing(body: .json(.init(pairing_code: code))) {
            case .ok(let ok):
                previewed = try ok.body.json
            case .forbidden:
                previewed = nil
                refused = true
            case .undocumented:
                previewed = nil
            }
        } catch {
            previewed = nil
        }
        // The answer is for the code that was asked about; a code edited
        // meanwhile keeps its own (cleared) state.
        guard Self.canonicalPairingCode(pairingCode) == code else { return }
        facts = previewed
        codeRefused = refused
    }

    /// Fixed copy for a connection mode (plan §5.14): how this device is
    /// reaching the daemon.
    public static func connectionLabel(_ mode: Components.Schemas.ConnectionMode) -> String {
        switch mode {
        case .loopback: "Local"
        case .tailscale: "Tailscale"
        case .unknown: "Unknown connection"
        }
    }

    /// Fixed copy for a granted scope: the authority the new device gets.
    public static func scopeLabel(_ scope: Components.Schemas.DeviceScope) -> String {
        switch scope {
        case ._operator: "Full operator control, revocable from the host"
        }
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

    nonisolated static func canonicalPairingCode(_ typed: String) -> String {
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

    nonisolated static func groupPairingCode(_ canonical: String) -> String {
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
