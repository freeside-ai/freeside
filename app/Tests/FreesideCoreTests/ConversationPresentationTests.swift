import Foundation
import FreesideAPI
import SwiftUI
import Testing

@testable import FreesideCore

/// Bounded conversation messages (visual audit D04): a body is bounded
/// exactly when six lines would cut it, the bound never shortens the text,
/// and the expanded state belongs to one message id.
@MainActor
@Suite struct ConversationPresentationTests {
    private typealias Presentation = ConversationPresentation

    @Test func togglingChangesOnlyThePressedMessage() {
        let plain = AttentionFixtures.longPlainMessageID
        let code = AttentionFixtures.longCodeMessageID
        #expect(Presentation.toggling(plain, in: []) == [plain])
        #expect(Presentation.toggling(code, in: [plain]) == [plain, code])
        #expect(Presentation.toggling(plain, in: [plain, code]) == [code])
    }

    #if os(macOS)
        @Test func bodyThatFitsSixLinesShowsInFullWithNoControl() {
            // Marking a short message expanded changes nothing: it has no
            // control in either state.
            for body in ["", "Can the revised spec preserve the existing migration order?", lines(6)] {
                let short = thread(body)
                #expect(height(short, expanded: [], width: 560) == height(short, expanded: ["msg-probe"], width: 560))
            }
            for snapshot in AttentionFixtures.defaultConversations() {
                let ids = Set(snapshot.conversation.messages.map(\.id))
                #expect(height(snapshot, expanded: [], width: 390) == height(snapshot, expanded: ids, width: 390))
            }
        }

        @Test func bodyPastSixLinesIsCutToSixWithItsControl() {
            let six = height(thread(lines(6)), expanded: [], width: 560)
            let seven = height(thread(lines(7)), expanded: [], width: 560)
            // The seventh line is cut: collapsed, only the control adds
            // height, and a far longer body is no taller.
            #expect(seven > six)
            #expect(seven == height(thread(lines(70)), expanded: [], width: 560))
            // Expanded, the body returns in full and the control stays
            // under it as "Show less".
            let expanded = height(thread(lines(7)), expanded: ["msg-probe"], width: 560)
            #expect(expanded > seven)
            #expect(
                height(thread(lines(70)), expanded: ["msg-probe"], width: 560) > 3 * expanded)
        }

        @Test func boundFollowsTheLayoutNotTheCharacterCount() {
            // One paragraph that fits six lines in the wide column and
            // wraps past them in a narrow one.
            let paragraph = thread(String(repeating: "word ", count: 70))
            #expect(
                height(paragraph, expanded: [], width: 560) == height(paragraph, expanded: ["msg-probe"], width: 560))
            #expect(
                height(paragraph, expanded: [], width: 300) < height(paragraph, expanded: ["msg-probe"], width: 300))
        }

        @Test func collapsedBodyIsShorterAndEachMessageExpandsOnItsOwn() {
            let snapshot = AttentionFixtures.longConversation()
            let plain = AttentionFixtures.longPlainMessageID
            let code = AttentionFixtures.longCodeMessageID
            let attached = AttentionFixtures.longAttachmentMessageID
            for width in [560.0, 390.0] {
                let collapsed = height(snapshot, expanded: [], width: width)
                let plainOnly = height(snapshot, expanded: [plain], width: width)
                let codeOnly = height(snapshot, expanded: [code], width: width)
                let attachedOnly = height(snapshot, expanded: [attached], width: width)
                let all = height(snapshot, expanded: [plain, code, attached], width: width)
                // Every long body is cut while collapsed, so its control
                // always reveals something.
                #expect(plainOnly > collapsed)
                #expect(codeOnly > collapsed)
                #expect(attachedOnly > collapsed)
                // Expanding one message leaves the others bounded: the
                // growths add up and none stands in for another.
                let growth = (plainOnly - collapsed) + (codeOnly - collapsed) + (attachedOnly - collapsed)
                #expect(abs((all - collapsed) - growth) < 1)
            }
        }

        @Test func expansionStaysWithItsMessageWhenTheSnapshotIsReplaced() throws {
            let plain = AttentionFixtures.longPlainMessageID
            let state = ConversationProbeState(snapshot: AttentionFixtures.longConversation())
            let host = NSHostingView(rootView: ConversationProbe(state: state, expanded: [plain]))
            host.setFrameSize(NSSize(width: 560, height: 4_000))
            host.layoutSubtreeIfNeeded()
            let before = host.fittingSize.height
            #expect(abs(before - height(state.snapshot, expanded: [plain], width: 560)) < 1)

            // A reply lands and an earlier message is inserted ahead of the
            // expanded one, moving its position without changing its id.
            var replaced = state.snapshot
            replaced.entity_version += 1
            replaced.conversation.messages.insert(
                .init(
                    id: "msg-daemon-earlier", conversation_id: replaced.conversation.id, sequence: 0,
                    author: .daemon, body: "Conversation opened.", attachments: [],
                    created_at: AttentionFixtures.createdInstant),
                at: 0)
            replaced.conversation.messages.append(
                .init(
                    id: "msg-agent-later", conversation_id: replaced.conversation.id, sequence: 9,
                    author: .agent, body: lines(9),
                    attachments: [], created_at: AttentionFixtures.createdInstant.addingTimeInterval(900)))
            state.snapshot = replaced
            let expected = height(replaced, expanded: [plain], width: 560)
            #expect(expected != height(replaced, expanded: [], width: 560))
            #expect(expected != height(replaced, expanded: ["msg-agent-later"], width: 560))
            try #require(
                pumpUntil {
                    host.layoutSubtreeIfNeeded()
                    return abs(host.fittingSize.height - expected) < 1
                })
            #expect(host.fittingSize.height > before)
            withExtendedLifetime(host) {}
        }

        private func lines(_ count: Int) -> String {
            Array(repeating: "line", count: count).joined(separator: "\n")
        }

        private func thread(_ body: String) -> Components.Schemas.ConversationSnapshot {
            var snapshot = AttentionFixtures.longConversation()
            snapshot.conversation.messages = [
                .init(
                    id: "msg-probe", conversation_id: snapshot.conversation.id, sequence: 1, author: .agent,
                    body: body, attachments: [], created_at: AttentionFixtures.createdInstant)
            ]
            return snapshot
        }

        private func height(
            _ snapshot: Components.Schemas.ConversationSnapshot, expanded: Set<String>, width: CGFloat
        ) -> CGFloat {
            let host = NSHostingView(
                rootView: ConversationProbe(
                    state: ConversationProbeState(snapshot: snapshot), expanded: expanded, width: width))
            host.setFrameSize(NSSize(width: width, height: 4_000))
            host.layoutSubtreeIfNeeded()
            return host.fittingSize.height
        }

        private func pumpUntil(_ condition: () -> Bool) -> Bool {
            let deadline = Date().addingTimeInterval(2)
            while !condition(), Date() < deadline {
                RunLoop.main.run(until: Date().addingTimeInterval(0.01))
            }
            return condition()
        }
    #endif
}

#if os(macOS)
    @MainActor @Observable private final class ConversationProbeState {
        var snapshot: Components.Schemas.ConversationSnapshot

        init(snapshot: Components.Schemas.ConversationSnapshot) {
            self.snapshot = snapshot
        }
    }

    private struct ConversationProbe: View {
        let state: ConversationProbeState
        let expanded: Set<String>
        var width: CGFloat = 560
        @State private var attachments = AttachmentLoader(client: APIClientFactory.mock(server: MockServer()))

        var body: some View {
            ConversationView(
                snapshot: state.snapshot,
                attachments: attachments,
                loadsAttachments: false,
                now: AttentionFixtures.createdInstant.addingTimeInterval(3_600),
                initiallyExpandedMessageIDs: expanded
            )
            .environment(\.dynamicTypeSize, .large)
            .frame(width: width, alignment: .topLeading)
            .fixedSize(horizontal: false, vertical: true)
        }
    }
#endif
