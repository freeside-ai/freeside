#if os(macOS)
    import AppKit
    import SwiftUI

    /// The New Task text area on the Mac: an NSTextView that intercepts the
    /// history arrows and, when the sheet wires a submit, Return. It reports
    /// its laid-out text height so the sheet can grow it with the draft. A
    /// spec approval's Discuss sheet uses it too (#2995), for its paste and
    /// drop of attachments, with no history.
    struct TaskPromptEditor: NSViewRepresentable {
        @Binding var text: String
        /// The prompts Up recalls, newest first; nil for a field that
        /// recalls none.
        let history: [String]?
        let browse: TaskPromptBrowseState
        /// What VoiceOver calls the field.
        var fieldLabel = "Work to do"
        /// The face the text draws in; nil for the composer's statement face.
        var textFont: NSFont?
        /// Submits if the draft can send, and says whether it did. Return
        /// never inserts a line while a submit is wired, so a draft that
        /// can't send ignores it. Nil keeps Return native (it inserts a line).
        var onSubmit: (() -> Bool)?
        /// The text's laid-out height, insets included.
        var contentHeight: Binding<CGFloat>?
        /// Takes pasted or dropped files and images instead of inserting
        /// them. Nil keeps paste and drop native.
        var onAttach: ((ComposerAttachmentIntake.Pasted) -> Void)?
        /// Whether the text view has keyboard focus, for a field that draws
        /// its own focus ring: SwiftUI's focus state doesn't reach an
        /// AppKit view.
        var isFocused: Binding<Bool>?
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
            scroll.documentView = editor
            updateNSView(scroll, context: context)
            return scroll
        }

        func updateNSView(_ scroll: NSScrollView, context: Context) {
            guard let editor = scroll.documentView as? PromptTextView else { return }
            editor.onEdit = { text = $0 }
            editor.browse = browse
            editor.history = history ?? []
            editor.onSubmit = onSubmit
            editor.onAttach = onAttach
            editor.onFocusChange = isFocused.map { binding in
                { focused in DispatchQueue.main.async { binding.wrappedValue = focused } }
            }
            editor.setAccessibilityLabel(fieldLabel)
            editor.setAccessibilityHelp(
                (onSubmit == nil ? "" : "Return submits and Shift-Return starts a new line. ")
                    + (history == nil
                        ? "" : "Press Up on the first line to recall recent prompts. Down restores your draft."))
            // Reported after this update, never during it: SwiftUI warns on
            // state changed while it draws.
            editor.onContentHeightChange = contentHeight.map { binding in
                { height in DispatchQueue.main.async { binding.wrappedValue = height } }
            }
            editor.isEditable = isEnabled
            editor.isSelectable = isEnabled
            editor.font = textFont ?? Self.font
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
        var onAttach: ((ComposerAttachmentIntake.Pasted) -> Void)?
        var onFocusChange: ((Bool) -> Void)?
        private var isRecalling = false
        private var reportedHeight: CGFloat?

        override func didChangeText() {
            super.didChangeText()
            if !isRecalling { browse.edited() }
            onEdit?(string)
            reportContentHeight()
        }

        override func becomeFirstResponder() -> Bool {
            let became = super.becomeFirstResponder()
            if became { onFocusChange?(true) }
            return became
        }

        override func resignFirstResponder() -> Bool {
            let resigned = super.resignFirstResponder()
            if resigned { onFocusChange?(false) }
            return resigned
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

        // MARK: Attachments

        /// What `pasteboard` holds to attach rather than insert: file URLs,
        /// or image data. On a paste, text beside an image wins, so a copy
        /// that carries both (a rich-text snippet with a rendered preview)
        /// still pastes as text. On a drag the image wins, since a browser's
        /// image drag carries its address as text too.
        static func attachment(
            from pasteboard: some PasteboardContents, textWins: Bool = true
        ) -> ComposerAttachmentIntake.Pasted? {
            let urls =
                pasteboard.readObjects(forClasses: [NSURL.self], options: [.urlReadingFileURLsOnly: true])
                as? [URL] ?? []
            if !urls.isEmpty { return .files(urls) }
            if textWins, pasteboard.string(forType: .string) != nil { return nil }
            for type in [NSPasteboard.PasteboardType.png, .tiff] {
                if let data = pasteboard.data(forType: type) { return .image(data) }
            }
            return nil
        }

        override func paste(_ sender: Any?) {
            if let onAttach, isEditable, let pasted = Self.attachment(from: NSPasteboard.general) {
                onAttach(pasted)
            } else {
                super.paste(sender)
            }
        }

        override var readablePasteboardTypes: [NSPasteboard.PasteboardType] {
            onAttach == nil ? super.readablePasteboardTypes : super.readablePasteboardTypes + [.fileURL, .png, .tiff]
        }

        override var acceptableDragTypes: [NSPasteboard.PasteboardType] {
            onAttach == nil ? super.acceptableDragTypes : super.acceptableDragTypes + [.fileURL, .png, .tiff]
        }

        override func draggingEntered(_ sender: any NSDraggingInfo) -> NSDragOperation {
            onAttach != nil && isEditable && Self.attachment(from: sender.draggingPasteboard, textWins: false) != nil
                ? .copy : super.draggingEntered(sender)
        }

        override func draggingUpdated(_ sender: any NSDraggingInfo) -> NSDragOperation {
            onAttach != nil && isEditable && Self.attachment(from: sender.draggingPasteboard, textWins: false) != nil
                ? .copy : super.draggingUpdated(sender)
        }

        override func performDragOperation(_ sender: any NSDraggingInfo) -> Bool {
            guard let onAttach, isEditable,
                let pasted = Self.attachment(from: sender.draggingPasteboard, textWins: false)
            else {
                return super.performDragOperation(sender)
            }
            onAttach(pasted)
            return true
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

    /// The pasteboard reads `PromptTextView.attachment(from:textWins:)`
    /// makes, with `NSPasteboard`'s signatures. A test checks the paste
    /// policy against an in-memory pasteboard, because an `NSPasteboard`
    /// needs a window-server session, which the macOS verification room
    /// withholds.
    protocol PasteboardContents {
        func readObjects(forClasses: [AnyClass], options: [NSPasteboard.ReadingOptionKey: Any]?) -> [Any]?
        func string(forType: NSPasteboard.PasteboardType) -> String?
        func data(forType: NSPasteboard.PasteboardType) -> Data?
    }

    extension NSPasteboard: PasteboardContents {}
#endif
