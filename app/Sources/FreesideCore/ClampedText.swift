import SwiftUI

/// Offers its content the height of its probe, so a `ViewThatFits` inside
/// can ask whether the whole text fits the collapsed line limit. The
/// answer comes from the layout pass itself, at the real width and text
/// size, with no measured state to arrive a frame late; text is bounded
/// exactly when the limit would cut it, so a control never reveals
/// nothing. The content keeps whatever height it then needs.
struct ProbeBoundedLayout: Layout {
    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        subviews[1].sizeThatFits(bounded(width: proposal.width, subviews))
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        subviews[0].place(at: bounds.origin, proposal: ProposedViewSize(width: bounds.width, height: nil))
        subviews[1].place(at: bounds.origin, proposal: bounded(width: bounds.width, subviews))
    }

    private func bounded(width: CGFloat?, _ subviews: Subviews) -> ProposedViewSize {
        let probe = subviews[0].sizeThatFits(ProposedViewSize(width: width, height: nil))
        return ProposedViewSize(width: width, height: probe.height)
    }
}

/// Prose clamped to a few lines with a `More` control that expands it in
/// place and becomes `Less` (#2608). Text that fits the limit draws whole
/// with no control. The clamp is only visual: VoiceOver reads the whole
/// text in both states and offers More or Less as a named action, and the
/// control is a real button that Tab and Full Keyboard Access reach. With
/// Larger Text the line count holds and the text never shrinks to fit.
///
/// `More` sits at the column's trailing edge on the last drawn line's
/// baseline rather than at the ellipsis, because SwiftUI has no inline
/// truncation control; the prose wraps in the width the control leaves. The
/// caller owns `isExpanded`, which is how a surface remembers the state.
struct ClampedText: View {
    static let moreTitle = "More"
    static let lessTitle = "Less"

    let text: String
    let font: Font
    let lineLimit: Int
    @Binding var isExpanded: Bool
    /// Whose text this is, said with the control, where several clamps on
    /// one surface would otherwise each be heard as a bare "More".
    let spokenName: String

    /// The control's title, which is also the accessibility action's name.
    static func controlTitle(isExpanded: Bool) -> String {
        isExpanded ? lessTitle : moreTitle
    }

    /// The control as spoken: its title and whose text it opens.
    static func controlAccessibilityLabel(isExpanded: Bool, spokenName: String) -> String {
        "\(controlTitle(isExpanded: isExpanded)), \(spokenName)"
    }

    var body: some View {
        ProbeBoundedLayout {
            prose(bounded: true)
                .hidden()
                .accessibilityHidden(true)
            ViewThatFits(in: .vertical) {
                prose(bounded: false)
                HStack(alignment: .lastTextBaseline, spacing: 6) {
                    prose(bounded: !isExpanded)
                        .accessibilityAction(named: Text(Self.controlTitle(isExpanded: isExpanded))) {
                            isExpanded.toggle()
                        }
                    control
                }
            }
        }
    }

    /// Always the whole string: the line limit bounds what is drawn, never
    /// the text, so VoiceOver gets all of it.
    private func prose(bounded: Bool) -> some View {
        Text(text)
            .font(font)
            .foregroundStyle(Color.ink)
            .lineLimit(bounded ? lineLimit : nil)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
            .accessibilityElement(children: .ignore)
            .accessibilityAddTraits(.isStaticText)
            .accessibilityLabel(Text(text))
    }

    private var control: some View {
        Button {
            isExpanded.toggle()
        } label: {
            Text(Self.controlTitle(isExpanded: isExpanded))
                .font(font)
                .foregroundStyle(Color.accentText)
                // A touch target taller than the line, without spreading
                // the line it trails.
                .padding(.vertical, 8)
                .contentShape(Rectangle())
                .padding(.vertical, -8)
        }
        .buttonStyle(.plain)
        .fixedSize()
        .freesideFocusRing(cornerRadius: 4)
        .accessibilityLabel(
            Text(Self.controlAccessibilityLabel(isExpanded: isExpanded, spokenName: spokenName)))
    }
}
