#if os(macOS)
    import AppKit
    import Testing

    @testable import FreesideCore

    @Suite @MainActor struct TaskPromptEditorTests {
        private func editor(_ text: String = "") -> PromptTextView {
            _ = NSApplication.shared
            let editor = PromptTextView(frame: NSRect(x: 0, y: 0, width: 160, height: 160))
            editor.string = text
            editor.font = .monospacedSystemFont(ofSize: 14, weight: .regular)
            editor.history = ["new prompt", "old prompt"]
            editor.setSelectedRange(NSRange(location: 0, length: 0))
            return editor
        }

        private func arrow(_ up: Bool, modifiers: NSEvent.ModifierFlags = []) throws -> NSEvent {
            try #require(
                NSEvent.keyEvent(
                    with: .keyDown, location: .zero, modifierFlags: modifiers, timestamp: 0, windowNumber: 0,
                    context: nil, characters: up ? "\u{f700}" : "\u{f701}",
                    charactersIgnoringModifiers: up ? "\u{f700}" : "\u{f701}", isARepeat: false,
                    keyCode: up ? 126 : 125))
        }

        private func returnKey(modifiers: NSEvent.ModifierFlags = [], keypad: Bool = false) throws -> NSEvent {
            try #require(
                NSEvent.keyEvent(
                    with: .keyDown, location: .zero, modifierFlags: modifiers, timestamp: 0, windowNumber: 0,
                    context: nil, characters: keypad ? "\u{3}" : "\r",
                    charactersIgnoringModifiers: keypad ? "\u{3}" : "\r", isARepeat: false,
                    keyCode: keypad ? 76 : 36))
        }

        /// What the sheet's submit does: it sends only while the draft can.
        private final class Sheet {
            var canSubmit: Bool
            var submits = 0
            init(canSubmit: Bool) { self.canSubmit = canSubmit }
            func submit() -> Bool {
                guard canSubmit else { return false }
                submits += 1
                return true
            }
        }

        /// An editor wired to a sheet's submit, its caret at the end.
        private func submittingEditor(_ text: String, canSubmit: Bool) -> (PromptTextView, Sheet) {
            let view = editor(text)
            view.setSelectedRange(NSRange(location: (text as NSString).length, length: 0))
            let sheet = Sheet(canSubmit: canSubmit)
            view.onSubmit = sheet.submit
            return (view, sheet)
        }

        @Test func returnSubmitsWithoutInsertingALine() throws {
            let (view, sheet) = submittingEditor("draft", canSubmit: true)
            view.keyDown(with: try returnKey())
            #expect(sheet.submits == 1)
            #expect(view.string == "draft")
            view.keyDown(with: try returnKey(keypad: true))
            #expect(sheet.submits == 2)
            #expect(view.string == "draft")
        }

        @Test func arrowsStillRecallWhileReturnSubmits() throws {
            let (view, sheet) = submittingEditor("", canSubmit: true)
            view.keyDown(with: try arrow(true))
            #expect(view.string == "new prompt")
            view.keyDown(with: try arrow(false))
            #expect(view.string == "")
            view.keyDown(with: try arrow(true))
            view.keyDown(with: try returnKey())
            #expect(sheet.submits == 1)
            #expect(view.string == "new prompt")
        }

        @Test func returnWhileTheDraftCannotSendDoesNothing() throws {
            let (view, sheet) = submittingEditor("draft", canSubmit: false)
            view.keyDown(with: try returnKey())
            #expect(sheet.submits == 0)
            #expect(view.string == "draft")
        }

        @Test func shiftReturnInsertsALineAndDoesNotSubmit() throws {
            let (view, sheet) = submittingEditor("draft", canSubmit: true)
            view.keyDown(with: try returnKey(modifiers: .shift))
            #expect(sheet.submits == 0)
            #expect(view.string == "draft\n")
        }

        @Test func returnCommitsACompositionInsteadOfSubmitting() throws {
            let (view, sheet) = submittingEditor("", canSubmit: true)
            view.setMarkedText(
                "かな", selectedRange: NSRange(location: 2, length: 0),
                replacementRange: NSRange(location: NSNotFound, length: 0))
            view.keyDown(with: try returnKey())
            #expect(sheet.submits == 0)
        }

        @Test func withNoSubmitWiredReturnInsertsALine() throws {
            let view = editor("draft")
            view.setSelectedRange(NSRange(location: 5, length: 0))
            view.keyDown(with: try returnKey())
            #expect(view.string == "draft\n")
        }

        @Test func voiceOverSubmitActionSubmitsOnlyWhenTheDraftCanSend() throws {
            let (view, sheet) = submittingEditor("draft", canSubmit: false)
            let action = try #require(view.accessibilityCustomActions()?.first)
            #expect(action.name == "Submit")
            #expect(action.handler?() == false)
            #expect(sheet.submits == 0)
            sheet.canSubmit = true
            #expect(action.handler?() == true)
            #expect(sheet.submits == 1)
        }

        @Test func contentHeightGrowsWithTheDraftAndReportsOnlyChanges() throws {
            let view = editor()
            var heights: [CGFloat] = []
            view.onContentHeightChange = { heights.append($0) }
            view.insertText("one", replacementRange: NSRange(location: 0, length: 0))
            view.insertText("two", replacementRange: NSRange(location: 3, length: 0))
            #expect(heights.count == 1)
            view.insertText("\nthree", replacementRange: NSRange(location: 6, length: 0))
            #expect(heights.count == 2)
            #expect(heights[1] > heights[0])
        }

        @Test func deliveredArrowsBrowseAndRestoreExactDraft() throws {
            let view = editor("  draft\n\n")
            var updates: [String] = []
            view.onEdit = { updates.append($0) }
            view.keyDown(with: try arrow(true))
            #expect(view.string == "new prompt")
            view.keyDown(with: try arrow(true))
            #expect(view.string == "old prompt")
            view.keyDown(with: try arrow(false))
            view.keyDown(with: try arrow(false))
            #expect(view.string == "  draft\n\n")
            #expect(updates == ["new prompt", "old prompt", "new prompt", "  draft\n\n"])
        }

        @Test func nativeMultilineAndWrappedLinesDoNotStartHistory() throws {
            let view = editor("first\nsecond")
            view.setSelectedRange(NSRange(location: 8, length: 0))
            #expect(!view.isOnFirstVisualLine)
            view.keyDown(with: try arrow(true))
            #expect(view.string == "first\nsecond")
            #expect(!view.browse.isBrowsing)
            view.string = String(repeating: "word ", count: 30)
            view.setSelectedRange(NSRange(location: 50, length: 0))
            #expect(!view.isOnFirstVisualLine)
            view.keyDown(with: try arrow(true))
            #expect(!view.browse.isBrowsing)
            view.string = "line\n"
            view.setSelectedRange(NSRange(location: 5, length: 0))
            #expect(!view.isOnFirstVisualLine)
        }

        @Test func selectionsModifiersCompositionAndFrozenInputStayNative() throws {
            for modifier: NSEvent.ModifierFlags in [.shift, .option, .command, .control] {
                let view = editor("draft")
                view.keyDown(with: try arrow(true, modifiers: modifier))
                #expect(view.string == "draft")
                #expect(!view.browse.isBrowsing)
            }
            let selected = editor("draft")
            selected.setSelectedRange(NSRange(location: 0, length: 2))
            selected.keyDown(with: try arrow(true))
            #expect(!selected.browse.isBrowsing)
            let composing = editor()
            composing.setMarkedText(
                "かな", selectedRange: NSRange(location: 2, length: 0),
                replacementRange: NSRange(location: NSNotFound, length: 0))
            #expect(composing.hasMarkedText())
            composing.keyDown(with: try arrow(true))
            #expect(!composing.browse.isBrowsing)
            let frozen = editor()
            frozen.isEditable = false
            frozen.keyDown(with: try arrow(true))
            #expect(frozen.string.isEmpty)
        }

        @Test func nativeEditingEndsBrowsing() throws {
            let view = editor("draft")
            view.keyDown(with: try arrow(true))
            view.insertText("edited ", replacementRange: NSRange(location: 0, length: 0))
            #expect(!view.browse.isBrowsing)
            #expect(view.string == "edited new prompt")
            view.keyDown(with: try arrow(false))
            #expect(view.string == "edited new prompt")
        }

        @Test func undoAfterRecallingAShorterPromptRestoresTheDraft() throws {
            let view = editor()
            let window = NSWindow(contentRect: view.frame, styleMask: .borderless, backing: .buffered, defer: false)
            window.contentView = view
            view.allowsUndo = true
            let undo = try #require(view.undoManager)
            undo.groupsByEvent = false
            undo.beginUndoGrouping()
            view.insertText("a much longer original draft", replacementRange: NSRange(location: 0, length: 0))
            undo.endUndoGrouping()
            view.setSelectedRange(NSRange(location: 0, length: 0))
            undo.beginUndoGrouping()
            view.keyDown(with: try arrow(true))
            undo.endUndoGrouping()
            #expect(view.string == "new prompt")
            undo.undo()
            #expect(view.string == "a much longer original draft")
            #expect(!view.browse.isBrowsing)
        }
    }
#endif
