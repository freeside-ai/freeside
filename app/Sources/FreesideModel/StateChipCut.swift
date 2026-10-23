/// Which of the three task-status tones a chip takes. A caller picks
/// the cut from what the operator can do, never from the status word.
package enum StateChipCut: CaseIterable {
    /// A bound Inbox item waits on the operator.
    case attention
    /// The daemon is working or has spoken; nothing to do here.
    case ink
    /// Historical, never current readiness.
    case faint
}
