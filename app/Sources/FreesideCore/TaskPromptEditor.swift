#if os(macOS)
    import AppKit
    import SwiftUI

    /// Intercepts only history arrows at the source editor's responder.
    struct TaskPromptEditor: NSViewRepresentable {
        @Binding var text: String
        let history: [String]
        let browse: TaskPromptBrowseState
        @Environment(\.isEnabled) private var isEnabled

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
            editor.setAccessibilityHelp(
                "Press Up on the first line to recall recent prompts. Down restores your draft.")
            scroll.documentView = editor
            updateNSView(scroll, context: context)
            return scroll
        }

        func updateNSView(_ scroll: NSScrollView, context: Context) {
            guard let editor = scroll.documentView as? PromptTextView else { return }
            editor.onEdit = { text = $0 }
            editor.browse = browse
            editor.history = history
            editor.isEditable = isEnabled
            editor.isSelectable = isEnabled
            _ = FreesideFont.registration
            editor.font = NSFont(name: "IBMPlexSans", size: FreesideFont.size(of: .callout))
            editor.textColor = NSColor(Color.ink)
            editor.insertionPointColor = NSColor(Color.ink)
            if editor.string != text {
                // A project switch restores an external draft. Old undo
                // ranges belong to the previous text and cannot be reused.
                editor.undoManager?.removeAllActions()
                editor.string = text
                editor.browse.edited()
            }
        }
    }

    final class PromptTextView: NSTextView {
        var history: [String] = []
        var browse = TaskPromptBrowseState()
        var onEdit: ((String) -> Void)?
        private var isRecalling = false

        override func didChangeText() {
            super.didChangeText()
            if !isRecalling { browse.edited() }
            onEdit?(string)
        }

        override func keyDown(with event: NSEvent) {
            let modifiers = event.modifierFlags.intersection([.shift, .control, .option, .command])
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
