import FreesideAPI
import SwiftUI

/// Reviews immutable saved requests without composition fields. Presentation
/// never sends; only a request's Retry button sends its original command.
struct TaskSubmissionRecoverySheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var model: TaskSubmissionModel
    @State private var retryingID: String?
    @State private var retryTask: Task<Void, Never>?
    let onRecovered: (String) -> Void
    var rendersInteractiveControls = true

    init(
        model: TaskSubmissionModel,
        onRecovered: @escaping (String) -> Void,
        rendersInteractiveControls: Bool = true
    ) {
        _model = State(initialValue: model)
        self.onRecovered = onRecovered
        self.rendersInteractiveControls = rendersInteractiveControls
    }

    var body: some View {
        VStack(spacing: 0) {
            if rendersInteractiveControls {
                ScrollView { content }
            } else {
                content
            }
            Divider().overlay(Color.rule)
            HStack {
                Spacer()
                Button("Close") { dismiss() }
                    .font(FreesideFont.callout)
                    .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
                    .keyboardShortcut(.cancelAction)
            }
            .padding(16)
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
        .frame(minWidth: 380, minHeight: 320)
        .onDisappear { retryTask?.cancel() }
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: 0) {
            FreesideSheetHeader(
                title: "Unconfirmed submissions",
                prompt: "These requests may already be accepted. Retry sends the original request to find its result.")
            VStack(alignment: .leading, spacing: 16) {
                if model.pendingSubmissions.isEmpty {
                    Text("No unconfirmed submissions.").font(FreesideFont.callout)
                }
                ForEach(Array(model.pendingSubmissions.enumerated()), id: \.element.command_id) { index, command in
                    if case .submit_task(let payload) = command.payload {
                        VStack(alignment: .leading, spacing: 12) {
                            Text("Submission \(index + 1)")
                                .font(FreesideFont.caption)
                                .foregroundStyle(Color.inkDim)
                            if let name = payload.name, !name.isEmpty {
                                Text(name).font(FreesideFont.callout.weight(.semibold))
                            }
                            Text(payload.project_id)
                                .font(FreesideFont.caption)
                                .foregroundStyle(Color.inkDim)
                            Text(payload.source)
                                .font(FreesideFont.callout)
                                .fixedSize(horizontal: false, vertical: true)
                                .textSelection(.enabled)
                            Button(retryingID == command.command_id ? "Retrying…" : "Retry") {
                                retry(command.command_id)
                            }
                            .font(FreesideFont.callout)
                            .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
                            .disabled(retryingID != nil)
                            .accessibilityLabel("Retry submission \(index + 1)")
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(16)
                        .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
                        .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
                    }
                }
                if retryingID == nil {
                    switch model.state {
                    case .lost:
                        Text(
                            model.freshness == .unauthenticated
                                ? "Authentication is unavailable. Reconnect to this daemon before retrying."
                                : "Still unconfirmed. Check your connection before retrying."
                        )
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.waxText)
                    case .rejected(let reason):
                        Text(reason).font(FreesideFont.callout).foregroundStyle(Color.waxText)
                    case .idle, .submitting, .submitted:
                        EmptyView()
                    }
                }
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
        }
    }

    private func retry(_ commandID: String) {
        guard retryingID == nil, model.selectPending(commandID) != nil else { return }
        retryingID = commandID
        retryTask = Task {
            let taskID = await model.retry()
            retryingID = nil
            guard !Task.isCancelled else { return }
            if let taskID {
                onRecovered(taskID)
                dismiss()
            }
        }
    }
}
