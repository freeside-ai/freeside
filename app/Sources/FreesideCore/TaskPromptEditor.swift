#if os(macOS)
    import AppKit
    import SwiftUI

    /// The New Task text area on the Mac: an NSTextView that intercepts the
    /// history arrows and, when the sheet wires a submit, Return. It reports
    /// its laid-out text height so the sheet can grow it with the draft.
    struct TaskPromptEditor: NSViewRepresentable {
        @Binding var text: String
        let history: [String]
        let browse: TaskPromptBrowseState
        /// Submits if the draft can send, and says whether it did. Return
        /// never inserts a line while a submit is wired, so a draft that
        /// can't send ignores it. Nil keeps Return native (it inserts a line).
        var onSubmit: (() -> Bool)?
        /// The text's laid-out height, insets included.
        var contentHeight: Binding<CGFloat>?
        @Environment(\.isEnabled) private var isEnabled

        /// The serif statement face the composer's text area draws in.
        static var font: NSFont {
            _ = FreesideFont.registration
            return NSFont(name: "FreesideSerif-Regular", size: FreesideFont.statementSize)
                ?? .systemFont(ofSize: FreesideFont.statementSize)
        }

        /// The height of `lines` lines in `font`, as the text view lays them out.
        static func height(lines: Int) -> CGFloat {
            CGFloat(lines) * NSLayoutManager().defaultLineHeight(for: font)
        }

        func makeNSView(context: Context) -> NSScrollView {
            let scroll = NSScrollView()
            scroll.drawsBackground = false
            scroll.hasVerticalScroller = true
            let editor = PromptTextView(frame: .zero)
            editor.isRichText = false
            editor.allowsUndo = true
            editor.drawsBackground = false
            editor.isVerticallyResizable = true
            editor.isHorizontallyResizable = false
            editor.autoresizingMask = [.width]
            editor.textContainer?.widthTracksTextView = true
            editor.textContainer?.containerSize = NSSize(width: 0, height: CGFloat.greatestFiniteMagnitude)
            editor.textContainerInset = .zero
            editor.setAccessibilityLabel("Work to do")
            scroll.documentView = editor
            updateNSView(scroll, context: context)
            return scroll
        }

        func updateNSView(_ scroll: NSScrollView, context: Context) {
            guard let editor = scroll.documentView as? PromptTextView else { return }
            editor.onEdit = { text = $0 }
            editor.browse = browse
            editor.history = history
            editor.onSubmit = onSubmit
            editor.setAccessibilityHelp(
                (onSubmit == nil ? "" : "Return submits and Shift-Return starts a new line. ")
                    + "Press Up on the first line to recall recent prompts. Down restores your draft.")
            // Reported after this update, never during it: SwiftUI warns on
            // state changed while it draws.
            editor.onContentHeightChange = contentHeight.map { binding in
                { height in DispatchQueue.main.async { binding.wrappedValue = height } }
            }
            editor.isEditable = isEnabled
            editor.isSelectable = isEnabled
            editor.font = Self.font
            editor.textColor = NSColor(Color.ink)
            editor.insertionPointColor = NSColor(Color.ink)
            if editor.string != text {
                // A project switch restores an external draft. Old undo
                // ranges belong to the previous text and cannot be reused.
                editor.undoManager?.removeAllActions()
                editor.string = text
                editor.browse.edited()
            }
            editor.reportContentHeight()
        }
    }

    final class PromptTextView: NSTextView {
        var history: [String] = []
        var browse = TaskPromptBrowseState()
        var onEdit: ((String) -> Void)?
        /// The sheet's submit: it reads the live draft and says whether it
        /// sent. The view keeps no copy of whether the draft can send, which
        /// would lag a keystroke behind SwiftUI's next update.
        var onSubmit: (() -> Bool)?
        var onContentHeightChange: ((CGFloat) -> Void)?
        private var isRecalling = false
        private var reportedHeight: CGFloat?

        override func didChangeText() {
            super.didChangeText()
            if !isRecalling { browse.edited() }
            onEdit?(string)
            reportContentHeight()
        }

        // A width change reflows the text, so its height may change too.
        override func setFrameSize(_ newSize: NSSize) {
            super.setFrameSize(newSize)
            reportContentHeight()
        }

        /// Reports the laid-out text height when it changes; an unchanged
        /// height reports nothing, so the sheet's frame can't loop on it.
        func reportContentHeight() {
            guard let onContentHeightChange, let layoutManager, let textContainer else { return }
            layoutManager.ensureLayout(for: textContainer)
            let height = ceil(layoutManager.usedRect(for: textContainer).height + 2 * textContainerInset.height)
            guard height != reportedHeight else { return }
            reportedHeight = height
            onContentHeightChange(height)
        }

        override func keyDown(with event: NSEvent) {
            let modifiers = event.modifierFlags.intersection([.shift, .control, .option, .command])
            // Return or keypad Enter submits; Shift-Return falls through and
            // inserts the line. Marked text stays native: Return commits an
            // input-method composition. While the draft can't send, Return
            // does nothing, as a disabled default button would.
            if onSubmit != nil, event.keyCode == 36 || event.keyCode == 76, modifiers.isEmpty, !hasMarkedText() {
                _ = onSubmit?()
                return
            }
            guard isEditable, modifiers.isEmpty, selectedRange().length == 0, !hasMarkedText(),
                event.keyCode == 126 || event.keyCode == 125
            else {
                super.keyDown(with: event)
                return
            }
            let replacement: String?
            if event.keyCode == 126, browse.isBrowsing || isOnFirstVisualLine {
                replacement = browse.up(from: string, history: history)
            } else if event.keyCode == 125 {
                replacement = browse.down()
            } else {
                replacement = nil
            }
            guard let replacement else {
                super.keyDown(with: event)
                return
            }
            // Use NSTextView's edit machinery so Undo always addresses the
            // matching text, even when recall replaces a much longer draft.
            breakUndoCoalescing()
            isRecalling = true
            insertText(replacement, replacementRange: NSRange(location: 0, length: (string as NSString).length))
            isRecalling = false
            breakUndoCoalescing()
            setSelectedRange(NSRange(location: 0, length: 0))
            scrollRangeToVisible(selectedRange())
            NSAccessibility.post(element: self, notification: .valueChanged)
        }

        // VoiceOver reaches the submit Return gives sighted operators.
        override func accessibilityCustomActions() -> [NSAccessibilityCustomAction]? {
            guard onSubmit != nil else { return super.accessibilityCustomActions() }
            return [
                NSAccessibilityCustomAction(name: "Submit") { [weak self] in
                    self?.onSubmit?() ?? false
                }
            ]
        }

        var isOnFirstVisualLine: Bool {
            guard !string.isEmpty else { return true }
            guard let layoutManager, let textContainer else { return false }
            layoutManager.ensureLayout(for: textContainer)
            let location = selectedRange().location
            // A trailing newline's insertion point belongs to the extra line.
            if location == (string as NSString).length, string.hasSuffix("\n") { return false }
            let character = min(location, (string as NSString).length - 1)
            let glyph = layoutManager.glyphIndexForCharacter(at: character)
            var first = NSRange()
            _ = layoutManager.lineFragmentRect(forGlyphAt: 0, effectiveRange: &first)
            return NSLocationInRange(glyph, first)
        }
    }
#endif
