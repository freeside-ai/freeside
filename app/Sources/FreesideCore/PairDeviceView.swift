#if os(macOS)
    import SwiftUI

    /// The Pair a Device window's content: a code from this Mac's supervised
    /// daemon and the address a phone enters, kept on screen while the
    /// operator types on the phone. A window rather than the menu-bar panel,
    /// which closes on any click outside it. It needs no device credential,
    /// so it works whether or not this Mac is paired.
    ///
    /// It mints when it appears and drops the code when it closes; the code
    /// lives only in `PairDeviceModel`'s memory.
    public struct PairDeviceWindow: View {
        /// The `Window` scene's identifier, which `openWindow(id:)` opens.
        public static let id = "pair-device"

        @State private var model: PairDeviceModel

        public init(source: any PairingCodeSource) {
            FreesideFont.registration
            _model = State(
                initialValue: PairDeviceModel(source: source, phoneAddress: { PhoneAddress.resolve(apiURL: $0) }))
        }

        public var body: some View {
            PairDeviceView(state: model.state, newCode: { Task { await model.mint() } })
                .task { await model.mint() }
                .onDisappear { model.clear() }
                .textSelection(.enabled)
                .tint(.accentText)
        }
    }

    /// The one Pair a Device composition, drawn live and by the screenshot
    /// suite. It takes plain state, so the suite renders every state.
    struct PairDeviceView: View {
        let state: PairDeviceModel.State
        let newCode: () -> Void
        /// Fixes the clock the countdown reads; nil lets it tick.
        var now: Date? = nil

        static let instructions =
            "On your phone, open Freeside and enter this address, then the code. The code works once."

        var body: some View {
            VStack(alignment: .leading, spacing: 22) {
                VStack(alignment: .leading, spacing: 12) {
                    KeywordLabel(text: "Pairing")
                    Text("Pair a Device")
                        .font(FreesideFont.ask)
                        .accessibilityAddTraits(.isHeader)
                }
                Text(Self.instructions)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
                stateContent
                Button("New Code", action: newCode)
                    .buttonStyle(FreesideActionButtonStyle(tone: .secondary, expands: false))
                    .disabled(state == .minting || state == .idle)
            }
            .padding(.horizontal, 24)
            .padding(.vertical, 20)
            .frame(width: 420, alignment: .leading)
            .foregroundStyle(Color.ink)
            // Here rather than on the window, so the screenshots draw it.
            .background(Color.ground)
        }

        @ViewBuilder private var stateContent: some View {
            switch state {
            case .idle, .minting:
                HStack(spacing: 10) {
                    ProgressView().controlSize(.small)
                    Text("Getting a code…")
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                }
            case .minted(let minted, let address):
                VStack(alignment: .leading, spacing: 16) {
                    labeled("Address") {
                        valueBox {
                            Text(address.url.absoluteString)
                                .font(FreesideFont.monoValue)
                        }
                    }
                    addressNote(address)
                    labeled("Code") {
                        valueBox {
                            Text(minted.formattedCode)
                                .font(FreesideFont.mono(.title2, weight: .medium))
                                .accessibilityLabel("Pairing code \(minted.formattedCode)")
                        }
                    }
                    expiry(minted.expiresAt)
                }
            case .failed(let failure):
                Notice(tone: .wax, keyword: "Failed", sentence: failure.message)
                    .accessibilityElement(children: .combine)
            }
        }

        /// The address's caveat: the forwarding it assumes, or that a phone
        /// can't reach it at all.
        @ViewBuilder private func addressNote(_ address: PhoneAddress) -> some View {
            switch address {
            case .tailnet:
                EmptyView()
            case .forwarded(let url):
                Text(Self.forwardingNote(port: url.port ?? 80))
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            case .thisMacOnly:
                Notice(
                    tone: .accent, keyword: "This Mac only",
                    sentence: "This Mac has no Tailscale address, so a phone can't reach the daemon here."
                )
                .accessibilityElement(children: .combine)
            }
        }

        static func forwardingNote(port: Int) -> String {
            "This Mac's Tailscale address. It assumes Tailscale forwards port \(port) to the daemon."
        }

        /// The countdown, or Expired with New Code as the way on.
        @ViewBuilder private func expiry(_ expiresAt: Date) -> some View {
            if let now {
                expiryLine(until: expiresAt, at: now)
            } else {
                TimelineView(.periodic(from: .now, by: 1)) { _ in
                    // The schedule is only a tick; its entry can run ahead of
                    // the wall clock, as PairingView's countdown notes.
                    expiryLine(until: expiresAt, at: Date())
                }
            }
        }

        @ViewBuilder private func expiryLine(until expiresAt: Date, at now: Date) -> some View {
            if let text = Self.expiryText(until: expiresAt, at: now) {
                Text(text)
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(expiresAt.timeIntervalSince(now) < 60 ? Color.accentText : Color.inkDim)
                    .help(expiresAt.formatted(.iso8601))
            } else {
                HStack(spacing: 10) {
                    StateChip(label: "Expired", cut: .attention)
                    Text("Get a new code to pair.")
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                }
                .accessibilityElement(children: .combine)
            }
        }

        /// "Expires in m:ss", or nil once the code has expired.
        static func expiryText(until expiresAt: Date, at now: Date) -> String? {
            let remaining = Int(expiresAt.timeIntervalSince(now).rounded(.up))
            guard remaining > 0 else { return nil }
            return "Expires in \(remaining / 60):" + String(format: "%02d", remaining % 60)
        }

        private func labeled(_ label: String, @ViewBuilder value: () -> some View) -> some View {
            VStack(alignment: .leading, spacing: 6) {
                KeywordLabel(text: label)
                    .accessibilityHidden(true)
                value()
            }
        }

        private func valueBox(@ViewBuilder value: () -> some View) -> some View {
            value()
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 10)
                .padding(.vertical, 8)
                .background(Color.ground2, in: RoundedRectangle(cornerRadius: 6))
                .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(Color.rule, lineWidth: 1))
        }
    }
#endif
