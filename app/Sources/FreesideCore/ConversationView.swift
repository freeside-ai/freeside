import FreesideAPI
import SwiftUI

struct ConversationView: View {
    let snapshot: Components.Schemas.ConversationSnapshot
    let attachments: AttachmentLoader
    let loadsAttachments: Bool
    var now = Date.now
    var rendersInteractiveControls = true

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Label("Conversation", systemImage: "bubble.left.and.bubble.right")
                .font(FreesideFont.sans(.headline, weight: .semibold))
                .foregroundStyle(Color.ink)

            ForEach(snapshot.conversation.messages.sorted(by: { $0.sequence < $1.sequence }), id: \.id) {
                message in
                VStack(alignment: .leading, spacing: 6) {
                    HStack(alignment: .firstTextBaseline) {
                        Text(authorLabel(message.author))
                            .font(FreesideFont.sans(.callout, weight: .semibold))
                            .foregroundStyle(Color.ink)
                        Spacer()
                        Text(AttentionDisplay.relativeRowTime(message.created_at, now: now))
                            .font(FreesideFont.caption)
                            .foregroundStyle(Color.inkDim)
                    }
                    Text(message.body)
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.ink)
                        .fixedSize(horizontal: false, vertical: true)
                        .textSelection(.enabled)
                    ForEach(Array(message.attachments.enumerated()), id: \.offset) { index, digest in
                        DecisionDetailView.AttachmentRow(
                            label: "Attachment \(index + 1)",
                            digest: digest,
                            attachments: attachments,
                            loadsAttachments: loadsAttachments,
                            rendersInteractiveControls: rendersInteractiveControls)
                    }
                }
                .padding(10)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(
                    message.author == .user ? Color.accentWashSoft : Color.ground,
                    in: RoundedRectangle(cornerRadius: 8)
                )
                .overlay(alignment: .leading) {
                    if message.author == .user {
                        Capsule()
                            .fill(Color.accentText)
                            .frame(width: 3)
                            .padding(.vertical, 8)
                    }
                }
            }

            if snapshot.conversation.status == .awaiting_agent {
                HStack(spacing: 8) {
                    if rendersInteractiveControls {
                        ProgressView().controlSize(.small)
                    } else {
                        Image(systemName: "clock")
                    }
                    Text("Awaiting the agent's reply")
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.inkDim)
                }
                .accessibilityElement(children: .combine)
            }
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .freesideCard()
    }

    private func authorLabel(_ author: Components.Schemas.Author) -> String {
        switch author {
        case .user: "You"
        case .agent: "Agent"
        case .daemon: "Freeside"
        }
    }

}

struct MessageComposerSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var message = ""
    let title: String
    let prompt: String
    let submitLabel: String
    var byteLimit: Int?
    var rendersInteractiveControls = true
    let submit: (String) async -> Bool
    @State private var isSubmitting = false

    private var trimmedMessage: String {
        message.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private var byteCount: Int {
        trimmedMessage.lengthOfBytes(using: .utf8)
    }

    private var canSubmit: Bool {
        !trimmedMessage.isEmpty && byteLimit.map { byteCount <= $0 } != false
    }

    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                VStack(alignment: .leading, spacing: 12) {
                    if !rendersInteractiveControls {
                        Text(title)
                            .font(FreesideFont.sans(.title3, weight: .semibold))
                            .foregroundStyle(Color.ink)
                    }
                    Text(prompt)
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.inkDim)
                    if rendersInteractiveControls {
                        TextEditor(text: $message)
                            .font(FreesideFont.callout)
                            .scrollContentBackground(.hidden)
                            .padding(8)
                            .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
                            .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
                            .accessibilityLabel("Message")
                    } else {
                        Text("Message")
                            .font(FreesideFont.callout)
                            .foregroundStyle(Color.inkDim)
                            .padding(8)
                            .frame(maxWidth: .infinity, minHeight: 190, alignment: .topLeading)
                            .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
                            .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
                    }
                    if let byteLimit {
                        Text("\(byteCount) of \(byteLimit) bytes")
                            .font(FreesideFont.caption)
                            .foregroundStyle(byteCount > byteLimit ? Color.waxText : Color.inkDim)
                            .frame(maxWidth: .infinity, alignment: .trailing)
                    }
                }
                .padding()
                .frame(maxWidth: .infinity, maxHeight: .infinity)

                // The submit lives in the sheet body rather than the toolbar so
                // it carries the design language's primary recipe; the row keeps
                // the Return and Escape bindings the placements supplied.
                FreesideSheetActionRow(
                    submitLabel: submitLabel,
                    isSubmitEnabled: canSubmit && !isSubmitting,
                    submit: performSubmit,
                    cancel: { dismiss() })
            }
            .background(Color.ground2)
            .navigationTitle(title)
        }
        .frame(minWidth: 380, minHeight: 300)
    }

    private func performSubmit() {
        let draft = trimmedMessage
        isSubmitting = true
        Task {
            let didClaimCommand = await submit(draft)
            isSubmitting = false
            if didClaimCommand {
                dismiss()
            }
        }
    }
}
