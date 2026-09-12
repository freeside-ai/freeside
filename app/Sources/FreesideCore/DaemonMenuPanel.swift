#if os(macOS)
    import AppKit
    import SwiftUI

    /// The menu-bar panel: the operator's glance at product state without
    /// opening the window. Drawn in the design language under
    /// `MenuBarExtra`'s `.window` style, replacing the standard menu items
    /// the bar rendered as system chrome (devlog
    /// 2026-09-12-0945-chrome-under-design-language).
    ///
    /// The view takes plain state, so the screenshot suite renders every
    /// daemon state; the app binds it to `DaemonMenuModel` and the session's
    /// inbox counts. Click-outside dismissal comes from the window style;
    /// Escape and each row action dismiss through the environment.
    public struct DaemonMenuPanel: View {
        /// The Show Inbox row's counts; `nil` until the session is ready,
        /// which disables the row.
        public struct InboxCounts: Equatable, Sendable {
            public let open: Int
            public let urgent: Int

            public init(open: Int, urgent: Int) {
                self.open = open
                self.urgent = urgent
            }
        }

        public struct Actions {
            public let openApp: () -> Void
            public let showInbox: () -> Void
            public let start: () -> Void
            public let stop: () -> Void
            public let openApprovalSettings: () -> Void
            public let quit: () -> Void

            public init(
                openApp: @escaping () -> Void,
                showInbox: @escaping () -> Void,
                start: @escaping () -> Void,
                stop: @escaping () -> Void,
                openApprovalSettings: @escaping () -> Void,
                quit: @escaping () -> Void
            ) {
                self.openApp = openApp
                self.showInbox = showInbox
                self.start = start
                self.stop = stop
                self.openApprovalSettings = openApprovalSettings
                self.quit = quit
            }
        }

        /// The interactive elements, in arrow-key order; non-interactive
        /// lines (the state block, the cards) are skipped.
        enum Row: Hashable {
            case openApp
            case showInbox
            case openApprovalSettings
            case control
            case quit
        }

        private enum Control {
            case start
            case stop
        }

        let state: DaemonMenuState
        let actionError: String?
        let inbox: InboxCounts?
        let actions: Actions
        /// The row a screenshot golden shows hovered; the live panel reads
        /// the pointer instead.
        private var screenshotHoveredRow: Row?

        @Environment(\.dismiss) private var dismiss
        /// The panel body holds keyboard focus for its whole life, the way a
        /// menu does; the arrow keys move a highlight over the rows rather
        /// than focus itself, since a macOS button will not take programmatic
        /// focus outside Full Keyboard Access.
        @FocusState private var panelFocused: Bool
        @State private var keyboardRow: Row?
        @State private var hoveredRow: Row?

        public init(
            state: DaemonMenuState,
            actionError: String?,
            inbox: InboxCounts?,
            actions: Actions
        ) {
            self.state = state
            self.actionError = actionError
            self.inbox = inbox
            self.actions = actions
        }

        func screenshotHovering(_ row: Row) -> Self {
            var panel = self
            panel.screenshotHoveredRow = row
            return panel
        }

        public var body: some View {
            VStack(alignment: .leading, spacing: 2) {
                row(.openApp, "Open Freeside") { actions.openApp() }
                row(.showInbox, "Show Inbox", enabled: inbox != nil, trailing: inboxTrailing) {
                    actions.showInbox()
                }
                divider
                KeywordLabel(text: "Daemon")
                    .padding(.top, 4)
                    .padding(.bottom, 6)
                    .padding(.horizontal, 10)
                stateBlock
                if state == .needsApproval {
                    row(.openApprovalSettings, "Open Login Items…") { actions.openApprovalSettings() }
                }
                if case .running(let health, let restartObserved) = state {
                    if !health.contractMatchesClient {
                        mismatchCard(health)
                    }
                    if restartObserved {
                        HStack(spacing: 8) {
                            Text("↻")
                            Text("Restart observed")
                        }
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.inkDim)
                        .padding(.horizontal, 10)
                        .padding(.bottom, 8)
                        .accessibilityElement(children: .combine)
                    }
                }
                if let actionError {
                    insetCard(fill: .waxWash) {
                        Text(actionError)
                            .font(FreesideFont.callout)
                            .foregroundStyle(Color.waxText)
                    }
                }
                if let control {
                    controlButton(control)
                }
                divider
                row(.quit, "Quit Freeside", trailing: shortcutGloss("⌘Q")) { actions.quit() }
            }
            .padding(6)
            .frame(width: 320)
            .background(RoundedRectangle(cornerRadius: 10).fill(Color.ground2))
            .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(Color.secondaryBorder, lineWidth: 1))
            // Edit-style focus: on macOS the default interaction set is
            // focusable only under Full Keyboard Access, and the panel's
            // keys must work for every operator.
            .focusable(interactions: .edit)
            .focused($panelFocused)
            .focusEffectDisabled()
            .defaultFocus($panelFocused, true)
            .onAppear {
                keyboardRow = nil
                panelFocused = true
            }
            .onKeyPress(.downArrow) {
                moveFocus(by: 1)
                return .handled
            }
            .onKeyPress(.upArrow) {
                moveFocus(by: -1)
                return .handled
            }
            .onKeyPress(.return) { activateKeyboardRow() }
            // The window style closes on a click outside but not on Escape.
            .onKeyPress(.escape) {
                dismiss()
                return .handled
            }
        }

        // MARK: Rows

        private func row(
            _ id: Row,
            _ label: String,
            enabled: Bool = true,
            action: @escaping () -> Void
        ) -> some View {
            row(id, label, enabled: enabled, trailing: EmptyView(), action: action)
        }

        private func row(
            _ id: Row,
            _ label: String,
            enabled: Bool = true,
            trailing: some View,
            action: @escaping () -> Void
        ) -> some View {
            Button {
                perform(action)
            } label: {
                HStack(spacing: 8) {
                    Text(label)
                    Spacer(minLength: 8)
                    trailing
                }
            }
            .buttonStyle(PanelRowStyle(isHovered: isHovered(id), isHighlighted: keyboardRow == id))
            .disabled(!enabled)
            .onHover { hovering in
                if hovering {
                    hoveredRow = id
                } else if hoveredRow == id {
                    hoveredRow = nil
                }
            }
        }

        private func isHovered(_ id: Row) -> Bool {
            hoveredRow == id || screenshotHoveredRow == id
        }

        @ViewBuilder private var inboxTrailing: some View {
            if let inbox {
                Text("\(inbox.open)")
                    .font(FreesideFont.monoCaption)
                    .foregroundStyle(Color.inkDim)
                if inbox.urgent > 0 {
                    StateChip(label: "\(inbox.urgent) urgent", color: .waxText)
                }
            }
        }

        private func shortcutGloss(_ keys: String) -> some View {
            Text(keys)
                .font(FreesideFont.monoCaption)
                .foregroundStyle(Color.inkFaint)
                .accessibilityLabel("Command Q")
        }

        private var divider: some View {
            Rectangle()
                .fill(Color.rule)
                .frame(height: 1)
                .padding(.horizontal, 4)
                .padding(.vertical, 6)
                .accessibilityHidden(true)
        }

        /// Runs a row's handler, then closes the panel the way a menu item
        /// would have.
        private func perform(_ action: () -> Void) {
            action()
            dismiss()
        }

        // MARK: Daemon state

        private var stateBlock: some View {
            VStack(alignment: .leading, spacing: 2) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    stateGlyph
                        .frame(width: 12)
                    Text(stateTitle)
                        .font(FreesideFont.sans(.body, weight: .medium))
                }
                .foregroundStyle(stateColor)
                if let secondary = stateSecondaryLine {
                    secondary
                        .fixedSize(horizontal: false, vertical: true)
                        .padding(.leading, 20)
                }
            }
            .padding(.horizontal, 10)
            .padding(.bottom, 8)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(stateAccessibilityLabel)
        }

        @ViewBuilder private var stateGlyph: some View {
            switch state {
            case .checking:
                ProgressView()
                    .controlSize(.mini)
                    .tint(Color.waterText)
            case .running:
                Text("✓")
            case .stopped:
                Text("■")
            case .needsApproval:
                Text("!")
            case .unavailable, .unreachable:
                Text("✕")
            }
        }

        private var stateTitle: String {
            switch state {
            case .checking: "Checking daemon…"
            case .running: "Running"
            case .stopped: "Stopped"
            case .needsApproval: "Approval needed"
            case .unavailable: "LaunchAgent unavailable"
            case .unreachable: "Unreachable"
            }
        }

        private var stateColor: Color {
            switch state {
            case .checking: .inkDim
            case .running: .ink
            case .stopped, .needsApproval: .accentText
            case .unavailable, .unreachable: .waxText
            }
        }

        /// The explanation under the state line: the version and start
        /// time while running, otherwise what the state means for the
        /// operator. Unavailable has nothing to add; checking is transient.
        private var stateSecondaryLine: Text? {
            switch state {
            case .running(let health, _):
                (Text("v\(health.version) · started ") + Text(health.startedAt, format: Self.startedFormat))
                    .font(FreesideFont.monoCaption)
                    .foregroundStyle(Color.inkDim)
            case .stopped:
                callout("Actions in the app are disabled until it starts.")
            case .needsApproval:
                callout("Allow Freeside in Login Items to start the daemon.")
            case .unreachable:
                callout("launchd is keeping the service enabled, but health is not answering.")
            case .checking, .unavailable:
                nil
            }
        }

        private func callout(_ string: String) -> Text {
            Text(string)
                .font(FreesideFont.callout)
                .foregroundStyle(Color.inkDim)
        }

        private static let startedFormat = Date.FormatStyle.dateTime.month(.abbreviated).day().hour().minute()

        /// VoiceOver reads the state block as one element, in the sentence
        /// the visual line and its explanation add up to.
        private var stateAccessibilityLabel: String {
            switch state {
            case .checking:
                "Checking daemon"
            case .running(let health, _):
                "Daemon running, version \(health.version), started "
                    + health.startedAt.formatted(date: .abbreviated, time: .shortened)
            case .stopped:
                "Daemon stopped. Actions in the app are disabled until it starts."
            case .needsApproval:
                "Approval needed. Allow Freeside in Login Items to start the daemon."
            case .unavailable:
                "LaunchAgent unavailable"
            case .unreachable:
                "Daemon unreachable. launchd is keeping the service enabled, but health is not answering."
            }
        }

        private func mismatchCard(_ health: DaemonHealth) -> some View {
            insetCard(fill: .accentWashSoft) {
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    KeywordLabel(text: "Mismatch", color: .accentText)
                    Text(
                        "Daemon contract \(ContractDigestDisplay.short(health.contractDigest)), "
                            + "app built for \(ContractDigestDisplay.shortClient) — "
                            + "update the daemon or the app."
                    )
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.ink)
                }
            }
        }

        private func insetCard(fill: Color, @ViewBuilder content: () -> some View) -> some View {
            content()
                .fixedSize(horizontal: false, vertical: true)
                .padding(.vertical, 8)
                .padding(.horizontal, 10)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(RoundedRectangle(cornerRadius: 6).fill(fill))
                .padding(.horizontal, 10)
                .padding(.bottom, 8)
                .accessibilityElement(children: .combine)
        }

        // MARK: Control

        /// One lifecycle control, never both: Start leads as the filled
        /// primary only when the daemon is down; Stop stays secondary.
        private var control: Control? {
            switch state {
            case .running, .unreachable, .needsApproval: .stop
            case .stopped, .unavailable: .start
            case .checking: nil
            }
        }

        private func controlButton(_ control: Control) -> some View {
            let label: String
            let tone: FreesideActionButtonStyle.Tone
            let action: () -> Void
            switch control {
            case .start:
                label = "Start"
                tone = .primary
                action = actions.start
            case .stop:
                label = "Stop"
                tone = .secondary
                action = actions.stop
            }
            // The control leaves the panel open: the state line under it
            // is where the operator sees the start or stop take effect.
            // The highlight sits 2pt outside the control: on the filled
            // primary its own accent border would otherwise swallow the ring.
            return Button(label, action: action)
                .buttonStyle(FreesideActionButtonStyle(tone: tone, compact: true, expands: false))
                .overlay(
                    RoundedRectangle(cornerRadius: 8)
                        .strokeBorder(keyboardRow == .control ? Color.accentBorder : .clear, lineWidth: 1)
                        .padding(-2)
                )
                .padding(.horizontal, 10)
                .padding(.top, 2)
                .padding(.bottom, 8)
        }

        // MARK: Keyboard

        private var keyboardOrder: [Row] {
            var order: [Row] = [.openApp]
            if inbox != nil {
                order.append(.showInbox)
            }
            if state == .needsApproval {
                order.append(.openApprovalSettings)
            }
            if control != nil {
                order.append(.control)
            }
            order.append(.quit)
            return order
        }

        private func moveFocus(by offset: Int) {
            let order = keyboardOrder
            guard let keyboardRow, let index = order.firstIndex(of: keyboardRow) else {
                self.keyboardRow = offset > 0 ? order.first : order.last
                return
            }
            let next = index + offset
            guard order.indices.contains(next) else { return }
            self.keyboardRow = order[next]
        }

        private func activateKeyboardRow() -> KeyPress.Result {
            guard let keyboardRow else { return .ignored }
            switch keyboardRow {
            case .openApp:
                perform(actions.openApp)
            case .showInbox:
                perform(actions.showInbox)
            case .openApprovalSettings:
                perform(actions.openApprovalSettings)
            case .control:
                switch control {
                case .start: actions.start()
                case .stop: actions.stop()
                case nil: return .ignored
                }
            case .quit:
                perform(actions.quit)
            }
            return .handled
        }
    }

    /// The panel row: body ink on a 34pt line, ground-3 under the pointer,
    /// accent-wash-soft while pressed, and a 1pt accent ring for the
    /// keyboard highlight with no fill change. A disabled row keeps its
    /// place and takes the faint cut.
    private struct PanelRowStyle: ButtonStyle {
        let isHovered: Bool
        let isHighlighted: Bool
        @Environment(\.isEnabled) private var isEnabled

        func makeBody(configuration: Configuration) -> some View {
            configuration.label
                .font(FreesideFont.body)
                .foregroundStyle(isEnabled ? Color.ink : Color.inkFaint)
                .padding(.horizontal, 10)
                .frame(maxWidth: .infinity, minHeight: 34, alignment: .leading)
                .background(RoundedRectangle(cornerRadius: 6).fill(fill(isPressed: configuration.isPressed)))
                .overlay(
                    RoundedRectangle(cornerRadius: 6)
                        .strokeBorder(isHighlighted ? Color.accentBorder : .clear, lineWidth: 1)
                )
                .contentShape(RoundedRectangle(cornerRadius: 6))
        }

        private func fill(isPressed: Bool) -> Color {
            guard isEnabled else { return .clear }
            if isPressed { return .accentWashSoft }
            return isHovered ? .ground3 : .clear
        }
    }

    extension DaemonMenuState {
        /// The status dot on the menu-bar mark. The bar draws on the
        /// system's own ground, so the day cut serves both appearances; a
        /// healthy running daemon shows no dot.
        public var menuBadgeColor: NSColor? {
            switch self {
            case .checking:
                nil
            case .running(let health, _):
                health.contractMatchesClient ? nil : NSColor(hex: FreesidePalette.accentText.day)
            case .stopped, .needsApproval:
                NSColor(hex: FreesidePalette.accentText.day)
            case .unavailable, .unreachable:
                NSColor(hex: FreesidePalette.waxText.day)
            }
        }

        public var accessibilityDescription: String {
            switch self {
            case .checking:
                "checking daemon"
            case .stopped:
                "daemon stopped"
            case .needsApproval:
                "approval needed"
            case .unavailable:
                "LaunchAgent unavailable"
            case .unreachable:
                "daemon unreachable"
            case .running(let health, _):
                health.contractMatchesClient ? "daemon running" : "daemon running, contract mismatch"
            }
        }
    }
#endif
