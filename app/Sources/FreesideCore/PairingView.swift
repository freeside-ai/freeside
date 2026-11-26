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
    var onChangeServer: (() -> Void)? = nil
    let onPaired: (DeviceCredential) -> Void

    var body: some View {
        NavigationStack {
            ScrollView {
                content(now: nil, rendersInteractiveControls: true)
                    .frame(maxWidth: .infinity)
            }
            .background(Color.ground)
            .toolbar {
                if let onChangeServer {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Change server", action: onChangeServer)
                            .disabled(model.phase == .pairing)
                    }
                }
            }
            #if os(macOS)
                .navigationTitle("Pair with Freeside")
            #else
                // The ask names the screen, so the bar carries no title and
                // stays only while it has Change server to hold.
                .navigationBarTitleDisplayMode(.inline)
                .toolbar(onChangeServer == nil ? .hidden : .visible, for: .navigationBar)
            #endif
        }
        // One preview per pause in typing: the task restarts on every code
        // change, so keystrokes inside the delay never reach the daemon, and
        // the details refresh once the operator stops.
        .task(id: model.pairingCode) {
            try? await Task.sleep(for: .milliseconds(400))
            guard !Task.isCancelled else { return }
            await model.refreshFacts()
        }
    }

    /// Names Get Code when the screen offers it: only the Mac, pairing
    /// with its own supervised daemon.
    static func instructions(offersGetCode: Bool) -> String {
        (offersGetCode
            ? "Choose Get Code for a one-time code from this Mac's daemon, or run the pairing command "
                + "on the daemon host and enter its code. "
            : "Run the pairing command on the daemon host and enter its one-time code. ")
            + "The device name appears in Devices on the host and in the audit record "
            + "of every decision made from this device."
    }

    static func previewFailureNotice(
        for preview: PairingModel.PreviewState
    ) -> (keyword: String, sentence: String)? {
        switch preview {
        case .idle, .checking, .refused:
            nil
        case .unreachable:
            (
                keyword: "Unreachable",
                sentence:
                    "Couldn't reach the daemon to check this code. "
                    + "Check that it's running and that this device can reach its address."
            )
        case .unexpectedAnswer(let statusCode):
            (
                keyword: "Failed",
                sentence: statusCode.map {
                    "The daemon gave an unexpected answer (HTTP \($0)) when checking this code."
                } ?? "The daemon gave an unexpected answer when checking this code."
            )
        }
    }

    /// The one pairing composition, drawn live and by the screenshot suite.
    /// `now` fixes the clock the expiry row reads; nil lets it tick.
    /// `rendersInteractiveControls` false draws each field as static text,
    /// because `ImageRenderer` cannot draw an AppKit-backed text field
    /// off-screen.
    private func content(now: Date?, rendersInteractiveControls: Bool) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            VStack(alignment: .leading, spacing: 12) {
                KeywordLabel(text: "Pairing")
                Text("Pair this device")
                    .font(FreesideFont.ask)
                    .accessibilityAddTraits(.isHeader)
            }
            Text(Self.instructions(offersGetCode: model.canGetCode))
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
            VStack(alignment: .leading, spacing: 16) {
                labeled("Code") {
                    if rendersInteractiveControls {
                        if dynamicTypeSize.isAccessibilitySize {
                            fieldBox { pairingCodeField }
                            HStack(spacing: 12) {
                                getCodeButton
                                pasteButton
                            }
                            .frame(maxWidth: .infinity, alignment: .trailing)
                        } else {
                            HStack(spacing: 12) {
                                fieldBox { pairingCodeField }
                                getCodeButton
                                pasteButton
                            }
                        }
                    } else {
                        HStack(spacing: 12) {
                            fieldBox {
                                Text(model.formattedPairingCode)
                                    .font(FreesideFont.monoValue)
                                    .frame(maxWidth: .infinity, alignment: .leading)
                            }
                            getCodeButton
                        }
                    }
                }
                labeled("Device name") {
                    fieldBox {
                        if rendersInteractiveControls {
                            TextField("Device name", text: $model.displayName)
                                .textFieldStyle(.plain)
                                .font(FreesideFont.cardBody)
                                .accessibilityLabel("Device name")
                        } else {
                            Text(model.displayName)
                                .font(FreesideFont.cardBody)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }
                    }
                }
            }
            VStack(alignment: .leading, spacing: 11) {
                KeywordLabel(text: "Host facts")
                    .accessibilityAddTraits(.isHeader)
                if let facts = model.facts {
                    FactGroup(spacing: 11) {
                        Self.detailsContent(facts, now: now)
                    }
                } else {
                    switch model.preview {
                    case .refused:
                        // The daemon refused the preview; without this the screen
                        // would wait on host details that never come.
                        Text("This code isn't valid or has expired; run `freesided pairing-code`")
                            .font(FreesideFont.cardBody)
                            .foregroundStyle(Color.inkDim)
                    case .checking:
                        Text("Checking this code with the daemon")
                            .font(FreesideFont.cardBody)
                            .foregroundStyle(Color.inkDim)
                    case .unreachable, .unexpectedAnswer:
                        Text("No host details")
                            .font(FreesideFont.cardBody)
                            .foregroundStyle(Color.inkDim)
                    case .idle:
                        Text("Enter a code to see host details")
                            .font(FreesideFont.cardBody)
                            .foregroundStyle(Color.inkDim)
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            if model.facts == nil, let failure = Self.previewFailureNotice(for: model.preview) {
                Notice(
                    tone: .wax,
                    keyword: failure.keyword,
                    sentence: failure.sentence,
                    action: Notice.Action(label: "Check Again", isEnabled: model.phase != .pairing) {
                        Task { await model.refreshFacts() }
                    }
                )
            }
            if case .failed(let message) = model.codeRequest {
                Notice(tone: .wax, keyword: "No code", sentence: message)
                    .accessibilityElement(children: .combine)
            }
            if case .failed(let message) = model.phase {
                Notice(tone: .wax, keyword: "Failed", sentence: message)
                    .accessibilityElement(children: .combine)
            }
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
                    Text("Pair")
                }
            }
            .buttonStyle(FreesideActionButtonStyle(tone: .primary))
            .disabled(!model.canSubmit)
        }
        .padding(.horizontal, 24)
        .padding(.vertical, 20)
        .frame(maxWidth: 528, alignment: .leading)
        .foregroundStyle(Color.ink)
        .tint(.accentText)
    }

    /// A field under its visible keyword. Each control carries the same
    /// words as its accessibility label, so VoiceOver reads the label once.
    private func labeled(_ label: String, @ViewBuilder field: () -> some View) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            KeywordLabel(text: label)
                .accessibilityHidden(true)
            field()
        }
    }

    private func fieldBox(@ViewBuilder field: () -> some View) -> some View {
        field()
            .padding(.horizontal, 10)
            .padding(.vertical, 8)
            .background(Color.ground2, in: RoundedRectangle(cornerRadius: 6))
            .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(Color.rule, lineWidth: 1))
            .freesideFocusRing()
    }

    struct DetailRow {
        let label: String
        let value: String
        var valueColor: Color? = nil
        var exact: String? = nil
    }

    static func expiryText(until expiry: Date, at now: Date) -> String {
        let remaining = expiry.timeIntervalSince(now)
        guard remaining > 0 else { return "expired" }
        guard remaining >= 60 else { return "in under a minute" }
        return "in \(Int(remaining / 60)) min"
    }

    static func expiryIsUrgent(until expiry: Date, at now: Date) -> Bool {
        expiry.timeIntervalSince(now) < 5 * 60
    }

    private static func expiryRow(until expiry: Date, at now: Date) -> DetailRow {
        DetailRow(
            label: "Code expires",
            value: expiryText(until: expiry, at: now),
            valueColor: expiryIsUrgent(until: expiry, at: now) ? .accentText : nil,
            exact: expiry.formatted(.iso8601))
    }

    /// The four pairing facts as the operator reads them (plan §5.14): what
    /// is being joined, when the code dies, how the phone reaches the
    /// daemon, and what it may do once paired.
    static func detailRows(_ facts: Components.Schemas.PairingFacts, now: Date) -> [DetailRow] {
        [
            DetailRow(label: "Host", value: facts.host_display_name),
            expiryRow(until: facts.code_expires_at, at: now),
            DetailRow(label: "Connection", value: PairingModel.connectionLabel(facts.connection_mode)),
            DetailRow(label: "Access", value: PairingModel.scopeLabel(facts.granted_scope)),
        ]
    }

    /// A fixed clock renders evidence; the live form ticks only its expiry row.
    @ViewBuilder
    static func detailsContent(_ facts: Components.Schemas.PairingFacts, now: Date? = nil) -> some View {
        ForEach(detailRows(facts, now: now ?? Date()), id: \.label) { row in
            if row.exact != nil, now == nil {
                TimelineView(.periodic(from: .now, by: 1)) { _ in
                    // The schedule is only a tick. Its entry can be ahead of
                    // the wall clock, which would expire the code early.
                    detailContent(expiryRow(until: facts.code_expires_at, at: Date()))
                }
            } else {
                detailContent(row)
            }
        }
    }

    @ViewBuilder
    private static func detailContent(_ row: DetailRow) -> some View {
        let content = FactRow(label: row.label, value: row.value, valueColor: row.valueColor ?? .ink)
        if let exact = row.exact {
            #if os(macOS)
                content.help(exact)
            #elseif os(iOS)
                content.copyMenu {
                    Button("Copy \(exact)") {
                        UIPasteboard.general.string = exact
                    }
                }
            #endif
        } else {
            content
        }
    }

    /// The pairing composition with static fields and a fixed clock, for
    /// the screenshot suite.
    func screenshotContent(now: Date) -> some View {
        content(now: now, rendersInteractiveControls: false)
    }

    /// Fills the code from this Mac's own daemon; absent when the screen
    /// pairs with any other daemon, and on the iPhone.
    @ViewBuilder private var getCodeButton: some View {
        if model.canGetCode {
            Button(model.codeRequest == .requesting ? "Getting Code…" : "Get Code") {
                Task { await model.getCode() }
            }
            .buttonStyle(.plain)
            .font(FreesideFont.noticeAction)
            .foregroundStyle(Color.accentText)
            .freesideFocusRing(cornerRadius: 4)
            .disabled(model.codeRequest == .requesting || model.phase == .pairing)
        }
    }

    private var pasteButton: some View {
        Button("Paste") {
            if let value = clipboardString {
                model.applyPairingCodeInput(value)
            }
        }
        .buttonStyle(.plain)
        .font(FreesideFont.noticeAction)
        .foregroundStyle(Color.accentText)
        .freesideFocusRing(cornerRadius: 4)
    }

    @ViewBuilder private var pairingCodeField: some View {
        let field = TextField(
            "Pairing code",
            text: Binding(
                get: { model.formattedPairingCode },
                set: { model.applyPairingCodeInput($0) })
        )
        .textFieldStyle(.plain)
        .textContentType(.oneTimeCode)
        .autocorrectionDisabled()
        .font(FreesideFont.monoValue)
        .accessibilityLabel("Pairing code")

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
