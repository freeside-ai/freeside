import Foundation
import Observation

/// The Pair a Device window's state: one fresh code at a time, with the
/// address a phone enters. It keeps no history and writes nothing; `clear()`
/// drops the code when the window closes.
@MainActor
@Observable
public final class PairDeviceModel {
    public enum State: Equatable {
        case idle
        case minting
        case minted(MintedPairingCode, PhoneAddress)
        case failed(PairingCodeFailure)
    }

    public private(set) var state: State = .idle

    private let source: any PairingCodeSource
    private let phoneAddress: (URL) -> PhoneAddress
    /// Bumped by every `mint()` and `clear()`, so an answer that arrives
    /// after a newer request, or after the window closed, is dropped.
    private var generation = 0

    public init(source: any PairingCodeSource, phoneAddress: @escaping (URL) -> PhoneAddress) {
        self.source = source
        self.phoneAddress = phoneAddress
    }

    /// Gets a fresh code, replacing any code shown.
    public func mint() async {
        generation += 1
        let request = generation
        state = .minting
        let outcome: State
        do {
            let minted = try await source.mint()
            outcome = .minted(minted, phoneAddress(minted.apiURL))
        } catch {
            outcome = .failed(error)
        }
        guard request == generation else { return }
        state = outcome
    }

    public func clear() {
        generation += 1
        state = .idle
    }
}
