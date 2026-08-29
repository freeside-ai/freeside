import Foundation

/// Re-runs the daemon's conversation invariants at client trust boundaries.
/// OpenAPI decoding proves field types and closed enums, but it does not enforce
/// scalar constraints or relationships between decoded fields.
public enum ConversationContractValidation {
    public struct InvalidConversation: LocalizedError, CustomStringConvertible, Sendable {
        public let reason: String

        public var description: String { "invalid conversation response: \(reason)" }
        public var errorDescription: String? { description }
    }

    /// Foundation's instant for Go's zero `time.Time` wire value,
    /// `0001-01-01T00:00:00Z`.
    private static let zeroDaemonTime = Date(timeIntervalSince1970: -62_135_769_600)

    public static func validate(
        _ snapshot: Components.Schemas.ConversationSnapshot,
        expectedID: String? = nil,
        maximumRevision: Int64? = nil
    ) throws {
        guard snapshot.as_of_revision >= 1 else {
            throw InvalidConversation(reason: "nonpositive snapshot revision")
        }
        guard snapshot.entity_version >= 1 else {
            throw InvalidConversation(reason: "nonpositive entity version")
        }
        if let maximumRevision, snapshot.as_of_revision > maximumRevision {
            throw InvalidConversation(
                reason:
                    "snapshot revision \(snapshot.as_of_revision) exceeds frontier \(maximumRevision)")
        }

        let conversation = snapshot.conversation
        if let expectedID, conversation.id != expectedID {
            throw InvalidConversation(
                reason:
                    "conversation id \(conversation.id) does not match requested id \(expectedID)")
        }
        guard !conversation.id.isEmpty else {
            throw InvalidConversation(reason: "empty conversation id")
        }

        var messageIDs: Set<String> = []
        for (offset, message) in conversation.messages.enumerated() {
            guard !message.id.isEmpty else {
                throw InvalidConversation(reason: "empty message id")
            }
            guard message.conversation_id == conversation.id else {
                throw InvalidConversation(
                    reason: "message \(message.id) belongs to \(message.conversation_id)")
            }
            guard message.sequence == offset + 1 else {
                throw InvalidConversation(
                    reason: "message \(message.id) has noncontiguous sequence \(message.sequence)")
            }
            guard messageIDs.insert(message.id).inserted else {
                throw InvalidConversation(reason: "duplicate message id \(message.id)")
            }
            guard message.created_at != zeroDaemonTime else {
                throw InvalidConversation(reason: "message \(message.id) has no creation time")
            }

            var attachments: Set<String> = []
            for attachment in message.attachments {
                guard !attachment.isEmpty else {
                    throw InvalidConversation(
                        reason: "message \(message.id) has an empty attachment digest")
                }
                guard attachments.insert(attachment).inserted else {
                    throw InvalidConversation(
                        reason: "message \(message.id) repeats attachment \(attachment)")
                }
            }
        }
    }

    public static func validate(
        _ snapshots: [Components.Schemas.ConversationSnapshot],
        maximumRevision: Int64? = nil
    ) throws {
        var conversationIDs: Set<String> = []
        for snapshot in snapshots {
            try validate(snapshot, maximumRevision: maximumRevision)
            guard conversationIDs.insert(snapshot.conversation.id).inserted else {
                throw InvalidConversation(
                    reason: "duplicate conversation id \(snapshot.conversation.id)")
            }
        }
    }
}
