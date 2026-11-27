import Foundation
import FreesideAPI
import Observation
import OpenAPIRuntime

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

    public enum PreviewState: Equatable, Sendable {
        /// No code is entered, a preview succeeded, or none has been asked yet.
        case idle
        /// A preview for the current code is in flight.
        case checking
        /// The daemon answered 403: the code is invalid, expired, or used,
        /// and the daemon never says which (test 13).
        case refused
        /// No response arrived from the daemon.
        case unreachable
        /// The daemon answered, but not with valid pairing facts (a status
        /// other than 200 or 403, or a 200 whose body did not decode).
        case unexpectedAnswer(statusCode: Int?)
    }

    public var pairingCode: String {
        didSet {
            if !isApplyingPairingCodePrefill {
                pairingCodeWasEdited = true
            }
            // Facts and preview state belong to one code; a changed code
            // clears both until the daemon answers for the new one.
            if Self.canonicalPairingCode(pairingCode) != Self.canonicalPairingCode(oldValue) {
                previewGeneration &+= 1
                facts = nil
                preview = .idle
            }
        }
    }
    public var displayName: String
    public private(set) var phase: PhaseState = .idle
    /// What the current code would join (plan §5.14 pairing facts), as the
    /// daemon last reported it for this code; nil until a preview succeeds
    /// and again whenever the code changes or a preview fails (`refused`,
    /// `unreachable`, or `unexpectedAnswer`). Display facts only: nothing
    /// here is a credential.
    public private(set) var facts: Components.Schemas.PairingFacts?
    /// State of the preview request for the current code: `idle` before a
    /// preview runs or after facts arrive, `checking` while a preview is in
    /// flight, `refused` on 403 (invalid, expired, or used, never
    /// distinguished), `unreachable` when no response arrives, and
    /// `unexpectedAnswer` on any other status or an undecodable 200. Cleared
    /// when the code changes.
    public private(set) var preview: PreviewState = .idle
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
    private var previewGeneration = 0

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
    /// it. Sets `preview` to `.checking` while in flight, then settles to
    /// `.idle` with `facts` on 200, `.refused` on 403 (the daemon never
    /// distinguishes unknown, expired, or used; test 13), `.unreachable`
    /// when no response arrives, or `.unexpectedAnswer` on any other status
    /// or an undecodable 200. Cancellation or a code edited meanwhile leaves
    /// the current code's state untouched.
    public func refreshFacts() async {
        previewGeneration &+= 1
        let generation = previewGeneration
        let code = Self.canonicalPairingCode(pairingCode)
        guard !code.isEmpty else {
            facts = nil
            preview = .idle
            return
        }
        preview = .checking
        let previewed: Components.Schemas.PairingFacts?
        let outcome: PreviewState
        do {
            switch try await client.previewPairing(body: .json(.init(pairing_code: code))) {
            case .ok(let ok):
                do {
                    previewed = try ok.body.json
                    outcome = .idle
                } catch {
                    previewed = nil
                    outcome = .unexpectedAnswer(statusCode: 200)
                }
            case .forbidden:
                previewed = nil
                outcome = .refused
            case .undocumented(let status, _):
                previewed = nil
                outcome = .unexpectedAnswer(statusCode: status)
            }
        } catch {
            // OpenAPIRuntime wraps any transport error in ClientError, so unwrap cancellation too.
            if error is CancellationError
                || (error as? ClientError)?.underlyingError is CancellationError
                || Task.isCancelled
            {
                return
            }
            previewed = nil
            if let clientError = error as? ClientError, let response = clientError.response {
                // The daemon answered, but the body did not decode.
                outcome = .unexpectedAnswer(statusCode: response.status.code)
            } else {
                outcome = .unreachable
            }
        }
        // The answer belongs to the code and request that asked; cancellation
        // or a code edited meanwhile keeps its own state.
        guard !Task.isCancelled, generation == previewGeneration,
            Self.canonicalPairingCode(pairingCode) == code
        else { return }
        facts = previewed
        preview = outcome
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
