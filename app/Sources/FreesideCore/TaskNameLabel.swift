import FreesideAPI
import SwiftUI

/// A task's name as the daemon labels it: a chosen name in the given face,
/// an identifier fallback in the mono face the inbox context line uses, and
/// an agent-proposed name followed by the "Agent" keyword, so agent prose
/// stays a labeled claim (plan §9) wherever the name appears. The keyword is
/// part of the same text run, so a name that wraps carries its mark after
/// its last word rather than beside its first line.
struct TaskNameLabel: View {
    let name: Components.Schemas.DisplayName
    var font: Font = FreesideFont.itemTitle
    var monoFont: Font = FreesideFont.monoCallout
    var color: Color = .ink
    var lineLimit: Int? = 2
    /// False where the surface draws the mark apart from the name (the task
    /// row's keyword line). VoiceOver reads it after the name either way.
    var drawsAgentMark = true

    var body: some View {
        var text = Text(name.text)
            .font(name.source == .identifier ? monoFont : font)
            .foregroundStyle(color)
        if name.source == .agent, drawsAgentMark {
            text =
                text
                + Text(Self.agentMark)
                .font(FreesideFont.keyword)
                .tracking(FreesideFont.keywordTracking)
                .foregroundStyle(Color.inkDim)
        }
        return
            text
            .lineLimit(lineLimit)
            .truncationMode(.middle)
            .accessibilityLabel(name.source == .agent ? name.text + Self.agentMark : name.text)
    }

    private static let agentMark = "  AGENT"
}
