#if os(macOS)
    import CoreGraphics
    import Testing

    extension Trait where Self == ConditionTrait {
        /// Runs a test only in a window-server session. A real
        /// `NSPasteboard` needs one, and the hosted task-page tests drive
        /// real AppKit scrolling in an `NSWindow`, which can't be trusted
        /// without one. The macOS verification
        /// room withholds one on purpose: its pre-job probe requires this
        /// same call to return nil (`no-window-server-session` in
        /// `daemon/internal/macverify/testdata/probe/main.go`). A gated
        /// test skips in the room and runs on a Mac with a logged-in
        /// session.
        static var needsWindowServerSession: Self {
            .enabled(
                if: CGSessionCopyCurrentDictionary() != nil,
                "needs a window-server session, which the macOS verification room withholds")
        }
    }
#endif
