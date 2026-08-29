import Foundation
import FreesideAPI
import Testing

@Suite struct ConversationTransactionTests {
    @Test func restoreRewindsConversationStateWithTheAttentionFrontier() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before = try await client.getSyncBootstrap().ok.body.json
        let item = try #require(
            before.attention_items.first { $0.item.id == "item-spec_approval" })

        _ = try await client.submitCommand(
            body: .json(
                Self.command(
                    id: "cmd-restore-discuss", action: .discuss,
                    message: "This turn must be rolled back.", against: item)))
        let advanced = try await client.getSyncBootstrap().ok.body.json
        #expect(advanced.conversations.first?.conversation.messages.count == 3)

        await server.restoreAttentionState(
            items: before.attention_items,
            conversations: before.conversations,
            revision: before.revision)
        let restored = try await client.getSyncBootstrap().ok.body.json

        #expect(restored.attention_items == before.attention_items)
        #expect(restored.conversations == before.conversations)
        #expect(restored.revision == before.revision)
        #expect(restored.sync_epoch != before.sync_epoch)
    }

    @Test func contractValidationRejectsDuplicateConversationRows() {
        let conversation = AttentionFixtures.defaultConversations()[0]

        #expect(throws: ConversationContractValidation.InvalidConversation.self) {
            try ConversationContractValidation.validate([conversation, conversation])
        }
    }

    @Test func bootstrapAndGetConversationCarryTheSeededThread() async throws {
        let client = APIClientFactory.mock(server: MockServer())

        let bootstrap = try await client.getSyncBootstrap().ok.body.json
        let seeded = try #require(bootstrap.conversations.first)
        let fetched = try await client.getConversation(
            path: .init(conversation_id: seeded.conversation.id)
        ).ok.body.json

        #expect(fetched == seeded)
        #expect(fetched.conversation.messages.map(\.sequence) == [1, 2])
    }

    @Test func discussAppendsOnceRefusesWhileAwaitingAndCompletesOnExplicitHook() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before = try await client.getAttentionItem(
            path: .init(item_id: "item-spec_approval")
        ).ok.body.json
        let command = Self.command(
            id: "cmd-discuss", action: .discuss, message: "Please preserve the order.",
            against: before)

        let first = try await client.submitCommand(body: .json(command)).ok.body.json
        let updated = try await client.getAttentionItem(
            path: .init(item_id: before.item.id)
        ).ok.body.json
        let conversationID = try #require(updated.item.conversation_id)
        let awaiting = try await client.getConversation(
            path: .init(conversation_id: conversationID)
        ).ok.body.json

        #expect(first.record.action == .discuss)
        #expect(updated.item.status == .open)
        #expect(updated.item.item_version == before.item.item_version + 1)
        #expect(awaiting.conversation.status == .awaiting_agent)
        #expect(awaiting.conversation.messages.last?.body == "Please preserve the order.")

        let second = Self.command(
            id: "cmd-discuss-2", action: .discuss, message: "A second message",
            against: updated)
        let rejection = try await client.submitCommand(body: .json(second)).conflict.body.json
        #expect(rejection.replacement_item == updated)

        let replay = try await client.submitCommand(body: .json(command)).ok.body.json
        #expect(replay == first)
        _ = try await client.getSyncRevision().ok.body.json
        let stillAwaiting = try await client.getConversation(
            path: .init(conversation_id: conversationID)
        ).ok.body.json
        #expect(stillAwaiting.conversation.status == .awaiting_agent)
        await server.completePendingAgentWork()
        let completed = try await client.getConversation(
            path: .init(conversation_id: conversationID)
        ).ok.body.json
        let completedItem = try await client.getAttentionItem(
            path: .init(item_id: before.item.id)
        ).ok.body.json
        #expect(completed.conversation.status == .idle)
        #expect(completed.conversation.messages.count == awaiting.conversation.messages.count + 1)
        #expect(completed.conversation.messages.last?.author == .agent)
        #expect(completed.conversation.messages.last?.id == "msg-agent-cmd-discuss")
        #expect(completedItem.item.status == .open)
        #expect(completedItem.item.item_version == updated.item.item_version + 1)
        #expect(completedItem.entity_version == updated.entity_version + 1)
        #expect(completedItem.as_of_revision == completed.as_of_revision)
    }

    @Test func requestChangesSupersedesAndCreatesAnOpenReplacement() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before = try await client.getAttentionItem(
            path: .init(item_id: "item-spec_approval")
        ).ok.body.json
        let command = Self.command(
            id: "cmd-revision", action: .request_changes,
            message: "Keep the migration order.", against: before)

        _ = try await client.submitCommand(body: .json(command)).ok.body.json
        let superseded = try await client.getAttentionItem(
            path: .init(item_id: before.item.id)
        ).ok.body.json
        let beforeCompletion = try await client.listAttentionItems(.init()).ok.body.json
        #expect(
            !beforeCompletion.contains {
                $0.item.id != before.item.id && $0.item._type == .spec_approval
                    && $0.item.status == .open && $0.item.subject == before.item.subject
            })
        await server.completePendingAgentWork()
        let items = try await client.listAttentionItems(.init()).ok.body.json
        let replacement = try #require(
            items.first {
                $0.item.id != before.item.id && $0.item._type == .spec_approval
                    && $0.item.status == .open && $0.item.subject == before.item.subject
            })

        #expect(superseded.item.status == .superseded)
        #expect(superseded.item.decided_at != nil)
        #expect(replacement.item.item_version == before.item.item_version + 1)
    }

    @Test func requestChangesUsesTheDaemonReplacementNameWhenTheSeedMatches() async throws {
        var before = AttentionFixtures.fixture(type: .spec_approval)
        before.item.id = "spec-approval-run-spec_approval-1"
        before.item.conversation_id = nil
        let server = MockServer(items: [before], conversations: [])
        let client = APIClientFactory.mock(server: server)
        let command = Self.command(
            id: "cmd-revision-name", action: .request_changes,
            message: "Keep the migration order.", against: before)

        _ = try await client.submitCommand(body: .json(command)).ok.body.json
        await server.completePendingAgentWork()
        let replacement = try await client.getAttentionItem(
            path: .init(item_id: "spec-approval-run-spec_approval-2")
        ).ok.body.json

        #expect(replacement.item.status == .open)
        #expect(replacement.item.subject == before.item.subject)
    }

    @Test func discussRejectsAnAttachmentThatIsNotStored() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let before = try await client.getAttentionItem(
            path: .init(item_id: "item-spec_approval")
        ).ok.body.json
        let command = Self.command(
            id: "cmd-missing-attachment", action: .discuss,
            message: "Review the attachment.", attachments: ["sha256:missing"],
            against: before)

        let output = try await client.submitCommand(body: .json(command))
        guard case .undocumented(let status, _) = output else {
            Issue.record("missing attachment was accepted: \(output)")
            return
        }

        #expect(status == 422)
    }

    private static func command(
        id: String,
        action: Components.Schemas.Action,
        message: String,
        attachments: [String]? = nil,
        against snapshot: Components.Schemas.AttentionItemSnapshot
    ) -> Components.Schemas.ClientCommand {
        .init(
            command_id: id,
            device_id: "device-mock",
            expected_entity_version: snapshot.entity_version,
            expected_bindings: .init(additionalProperties: [:]),
            payload: .init(
                item_id: snapshot.item.id,
                action: action,
                item_version: snapshot.item.item_version,
                pr_head_sha: snapshot.item.pr_head_sha,
                artifact_digests: snapshot.item.artifact_digests,
                message: message,
                attachments: attachments))
    }
}
