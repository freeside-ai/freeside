import FreesideAPI
import Observation
import SwiftUI

@MainActor
private func scheduleDecisionFeedback(
    after delay: Duration,
    action: @escaping @MainActor () -> Void
) -> Task<Void, Never> {
    Task { @MainActor in
        do {
            try await Task.sleep(for: delay)
        } catch {
            return
        }
        guard !Task.isCancelled else { return }
        action()
    }
}

struct DecisionConclusion: Equatable, Sendable {
    let itemID: String
    let actionLabel: String
    /// The canonical lifecycle status when the item remains queryable.
    /// Snoozing a proposal instead proves queue exit through an authoritative
    /// 404, so it has no visible lifecycle status until it wakes again.
    let resultingStatus: Components.Schemas.ItemStatus?
    let at: Date

    var hasQueryableItem: Bool { resultingStatus != nil }
}

@MainActor
@Observable
final class DecisionFeedbackModel {
    static let advanceDelay: Duration = .milliseconds(800)
    static let dismissalDelay: Duration = .seconds(6)

    private(set) var conclusion: DecisionConclusion?
    private var advanceTask: Task<Void, Never>?
    private var dismissalTask: Task<Void, Never>?
    private let announce: @MainActor (String) -> Void
    private let schedule: @MainActor (Duration, @escaping @MainActor () -> Void) -> Task<Void, Never>

    init(
        announce: @escaping @MainActor (String) -> Void = AccessibilityAnnouncer.announce,
        schedule:
            @escaping @MainActor (
                Duration, @escaping @MainActor () -> Void
            ) -> Task<Void, Never> = scheduleDecisionFeedback
    ) {
        self.announce = announce
        self.schedule = schedule
    }

    func present(
        _ conclusion: DecisionConclusion,
        advancesAutomatically: Bool,
        advance: @escaping @MainActor () -> Void
    ) {
        advanceTask?.cancel()
        dismissalTask?.cancel()
        self.conclusion = conclusion

        // The announcement is synchronous and precedes scheduling the focus
        // move, so assistive technology hears the result before selection
        // changes beneath it.
        announce("\(conclusion.actionLabel) applied.")

        if advancesAutomatically {
            advanceTask = schedule(Self.advanceDelay) { [weak self] in
                guard self?.conclusion == conclusion else { return }
                advance()
            }
        }
        dismissalTask = schedule(Self.dismissalDelay) { [weak self] in
            guard self?.conclusion == conclusion else { return }
            self?.conclusion = nil
        }
    }

    func dismiss() {
        advanceTask?.cancel()
        dismissalTask?.cancel()
        conclusion = nil
    }

}

private enum AccessibilityAnnouncer {
    @MainActor
    static func announce(_ message: String) {
        AccessibilityNotification.Announcement(message).post()
    }
}

struct DecisionFeedbackBanner: View {
    let feedback: DecisionFeedbackModel
    let onView: (String) -> Void

    var body: some View {
        if let conclusion = feedback.conclusion {
            HStack(spacing: 10) {
                Image(systemName: "checkmark")
                    .accessibilityHidden(true)
                Text("\(conclusion.actionLabel) applied")
                    .font(FreesideFont.callout)
                Spacer()
                if conclusion.hasQueryableItem {
                    Button("View") { onView(conclusion.itemID) }
                }
            }
            .foregroundStyle(Color.inkDim)
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color.neutralWash)
            .accessibilityElement(children: .contain)
        }
    }
}
