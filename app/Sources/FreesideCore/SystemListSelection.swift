import SwiftUI

#if os(macOS)
    import AppKit
#endif

extension View {
    /// Hides AppKit's built-in list-selection highlight (the system
    /// control-accent fill, blue by default) so a plain `List`'s own
    /// selected-row treatment is what the operator sees.
    ///
    /// SwiftUI's `.tint` does not reach an `NSTableView`'s
    /// `selectionHighlightStyle`, so the backing table is configured
    /// directly. Apply it to the row content inside a macOS
    /// `List(selection:)`; a no-op off macOS, where the inbox and runs
    /// lists drive selection through `NavigationLink` and never draw the
    /// system fill.
    func hidesSystemListSelection() -> some View {
        #if os(macOS)
            background(SystemListSelectionHider())
        #else
            self
        #endif
    }
}

#if os(macOS)
    /// A zero-cost representable embedded in a `List` row: once in a window
    /// it walks up to the enclosing `NSTableView` and turns the system
    /// selection highlight off. Row-embedded rather than List-attached so
    /// the walk-up stays inside the single table that hosts these rows.
    private struct SystemListSelectionHider: NSViewRepresentable {
        func makeNSView(context: Context) -> EnclosingTableConfigurator {
            EnclosingTableConfigurator()
        }

        func updateNSView(_ nsView: EnclosingTableConfigurator, context: Context) {
            // Re-assert in case a later table update restored the default.
            nsView.disableEnclosingTableSelectionHighlight()
        }
    }

    private final class EnclosingTableConfigurator: NSView {
        override func viewDidMoveToWindow() {
            super.viewDidMoveToWindow()
            disableEnclosingTableSelectionHighlight()
        }

        // Stay transparent to the mouse so clicks reach the table's own
        // row selection instead of being swallowed by this backing view.
        override func hitTest(_ point: NSPoint) -> NSView? { nil }

        fileprivate func disableEnclosingTableSelectionHighlight() {
            var ancestor = superview
            while let current = ancestor {
                if let table = current as? NSTableView {
                    table.selectionHighlightStyle = .none
                    return
                }
                ancestor = current.superview
            }
        }
    }
#endif
