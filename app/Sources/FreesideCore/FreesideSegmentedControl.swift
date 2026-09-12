import SwiftUI

/// The design language's segmented control, replacing the system
/// `Picker(.segmented)` in the sidebar (devlog
/// 2026-09-12-0945-chrome-under-design-language): a ground container with
/// a rule border, equal-width segments, the selected one on its own fill,
/// and counts in the mono caption after the word. The former Picker label
/// becomes the control's accessibility name; VoiceOver reads each segment
/// as a button with its count as the value and the selected trait.
///
/// At `xxxLarge` and above the segments stack, one left-aligned row each,
/// the same threshold at which the inbox's scope bar already stacks.
struct FreesideSegmentedControl<Selection: Hashable>: View {
    struct Segment: Identifiable {
        let value: Selection
        let label: String
        var count: Int? = nil
        var id: Selection { value }
    }

    /// The old Picker's label ("Scope", "Section"): spoken, never drawn.
    let accessibilityLabel: String
    let segments: [Segment]
    @Binding var selection: Selection
    /// A screenshot golden's focus ring; the live control reads real focus.
    var screenshotFocused = false

    @Environment(\.isEnabled) private var isEnabled
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @FocusState private var isFocused: Bool
    @State private var hoveredSegment: Selection?

    static func stacks(at size: DynamicTypeSize) -> Bool {
        size >= .xxxLarge
    }

    private var stacked: Bool { Self.stacks(at: dynamicTypeSize) }

    var body: some View {
        Group {
            if stacked {
                VStack(spacing: 2) {
                    ForEach(segments) { segmentButton($0) }
                }
            } else {
                HStack(spacing: 2) {
                    ForEach(segments) { segmentButton($0) }
                }
            }
        }
        .padding(2)
        .background(RoundedRectangle(cornerRadius: 6).fill(Color.ground))
        .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(Color.rule, lineWidth: 1))
        // One focusable element: Tab lands on the control, the arrows move
        // the selection, and the ring sits on the selected segment.
        .focusable()
        .focused($isFocused)
        .focusEffectDisabled()
        .onKeyPress(.leftArrow) { move(by: -1) }
        .onKeyPress(.rightArrow) { move(by: 1) }
        .accessibilityElement(children: .contain)
        .accessibilityLabel(accessibilityLabel)
    }

    private func segmentButton(_ segment: Segment) -> some View {
        let isSelected = segment.value == selection
        return Button {
            selection = segment.value
        } label: {
            HStack(spacing: 0) {
                // One text run, so the word and its count scale together a
                // step at the sidebar's minimum width instead of truncating.
                segmentText(segment)
                    .lineLimit(1)
                    .minimumScaleFactor(0.8)
                if stacked {
                    Spacer(minLength: 0)
                }
            }
        }
        .buttonStyle(
            SegmentStyle(
                isSelected: isSelected,
                isHovered: hoveredSegment == segment.value,
                isFocused: (isFocused || screenshotFocused) && isSelected,
                stacked: stacked)
        )
        // Not a Tab stop of its own: under Full Keyboard Access a button is
        // focusable, and the control is the single stop.
        .focusable(false)
        .onHover { hovering in
            if hovering {
                hoveredSegment = segment.value
            } else if hoveredSegment == segment.value {
                hoveredSegment = nil
            }
        }
        .accessibilityAddTraits(isSelected ? .isSelected : [])
        // The word alone; the derived label would carry the count that the
        // value already speaks.
        .accessibilityLabel(segment.label)
        .accessibilityValue(segment.count.map { "\($0)" } ?? "")
    }

    /// "Open" then the count: inline, the count in the mono caption ("Open
    /// 6"); stacked, the phrase "Open · 6" as one sans run, as the handoff's
    /// stacked mock draws it. The count takes the dim cut (faint when
    /// disabled) in both.
    private func segmentText(_ segment: Segment) -> Text {
        guard let count = segment.count else { return Text(segment.label) }
        let countText = Text(stacked ? "· \(count)" : "\(count)")
            .font(stacked ? FreesideFont.sans(.subheadline, weight: .medium) : FreesideFont.monoCaption)
            .foregroundStyle(isEnabled ? Color.inkDim : Color.inkFaint)
        return Text(segment.label) + Text(" ") + countText
    }

    private func move(by offset: Int) -> KeyPress.Result {
        guard let index = segments.firstIndex(where: { $0.value == selection }) else {
            return .ignored
        }
        let next = index + offset
        guard segments.indices.contains(next) else { return .handled }
        selection = segments[next].value
        return .handled
    }
}

/// One segment: medium sans on a 28pt line (40 stacked), the selected
/// segment on the `segmentSelected` fill, a hovered one on the soft accent
/// wash, a pressed one on ground-3, and a 1pt accent ring for focus. A
/// disabled control keeps its fills and takes the faint cut on every label.
private struct SegmentStyle: ButtonStyle {
    let isSelected: Bool
    let isHovered: Bool
    let isFocused: Bool
    let stacked: Bool
    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(FreesideFont.sans(.subheadline, weight: .medium))
            .foregroundStyle(labelColor)
            // Three segments with counts share a 280pt sidebar with the
            // urgent chip, so the inset stays small.
            .padding(.horizontal, stacked ? 10 : 3)
            .frame(maxWidth: .infinity, minHeight: stacked ? 40 : 28, alignment: stacked ? .leading : .center)
            .background(RoundedRectangle(cornerRadius: 4).fill(fill(isPressed: configuration.isPressed)))
            .overlay(
                RoundedRectangle(cornerRadius: 4)
                    .strokeBorder(isFocused ? Color.accentBorder : .clear, lineWidth: 1)
            )
            .contentShape(RoundedRectangle(cornerRadius: 4))
    }

    private var labelColor: Color {
        guard isEnabled else { return .inkFaint }
        return isSelected ? .ink : .inkDim
    }

    private func fill(isPressed: Bool) -> Color {
        if isSelected { return .segmentSelected }
        guard isEnabled else { return .clear }
        if isPressed { return .ground3 }
        return isHovered ? .accentWashSoft : .clear
    }
}

/// The secondary-tone trigger a `Menu` opens from: the current choice with
/// the up-down chevron, on ground-2 inside the secondary border. The popup
/// list the menu shows stays system chrome; only the trigger is Freeside's.
struct FreesideMenuTriggerLabel: View {
    let title: String

    var body: some View {
        HStack(spacing: 8) {
            Text(title)
                .font(FreesideFont.sans(.subheadline, weight: .medium))
                .foregroundStyle(Color.ink)
                .lineLimit(1)
            Spacer(minLength: 8)
            Image(systemName: "chevron.up.chevron.down")
                .font(FreesideFont.caption)
                .foregroundStyle(Color.inkDim)
        }
        .padding(.horizontal, 10)
        .frame(maxWidth: .infinity, minHeight: 28)
        .background(RoundedRectangle(cornerRadius: 6).fill(Color.ground2))
        .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(Color.secondaryBorder, lineWidth: 1))
        .contentShape(RoundedRectangle(cornerRadius: 6))
    }
}
