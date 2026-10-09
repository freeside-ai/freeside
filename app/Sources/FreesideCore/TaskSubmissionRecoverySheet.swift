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
            FreesideSheetActionRow.done { dismiss() }
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
        .frame(minWidth: 380, minHeight: 320)
        .onDisappear { retryTask?.cancel() }
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: 0) {
            FreesideSheetHeader(
                eyebrow: "Unconfirmed submissions",
                chip: StateChip(label: "\(model.pendingSubmissions.count)", cut: .attention),
                ask: "Saved, not confirmed",
                consequence:
                    "The daemon did not answer these requests. Nothing is assumed; Retry sends only that one.",
                askLineLimit: nil)
            VStack(alignment: .leading, spacing: 16) {
                if model.pendingSubmissions.isEmpty {
                    Text("No unconfirmed submissions.")
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                }
                ForEach(Array(model.pendingSubmissions.enumerated()), id: \.element.command_id) { index, command in
                    if case .submit_task(let payload) = command.payload {
                        request(payload, index: index, commandID: command.command_id)
                    }
                }
                if retryingID == nil {
                    switch model.state {
                    case .lost:
                        Notice(
                            tone: .accent, keyword: "Unconfirmed",
                            sentence: model.freshness == .unauthenticated
                                ? "Authentication is unavailable. Reconnect to this daemon before retrying."
                                : "Check your connection before retrying."
                        )
                        .accessibilityElement(children: .combine)
                    case .rejected(let reason):
                        Notice(tone: .wax, keyword: "Failed", sentence: reason)
                            .accessibilityElement(children: .combine)
                    case .idle, .submitting, .submitted:
                        EmptyView()
                    }
                }
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
        }
    }

    /// One saved request as a bordered item (R3): what it is and where it
    /// runs, its optional name, the work it asks for, and its own Retry.
    private func request(
        _ payload: Components.Schemas.SubmitTaskPayload, index: Int, commandID: String
    ) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            VStack(alignment: .leading, spacing: 3) {
                Text("New task · \(payload.project_id)")
                    .font(FreesideFont.factLabel)
                    .foregroundStyle(Color.ink)
                if let name = payload.name, !name.isEmpty {
                    Text(name)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                }
                Text(payload.source)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.ink)
                    .textSelection(.enabled)
            }
            .fixedSize(horizontal: false, vertical: true)
            Button(retryingID == commandID ? "Retrying…" : "Retry") {
                retry(commandID)
            }
            .buttonStyle(FreesideActionButtonStyle(tone: .secondary, expands: false))
            .disabled(retryingID != nil)
            .accessibilityLabel("Retry submission \(index + 1)")
        }
        .padding(.horizontal, 18)
        .padding(.vertical, 16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
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
