import Foundation
import FreesideAPI
import Observation

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

package struct DecisionConclusion: Equatable, Sendable {
    package let itemID: String
    package let actionLabel: String
    /// The canonical lifecycle status when the item remains queryable.
    /// Snoozing a proposal instead proves queue exit through an authoritative
    /// 404, so it has no visible lifecycle status until it wakes again.
    let resultingStatus: Components.Schemas.ItemStatus?
    let at: Date

    package var hasQueryableItem: Bool { resultingStatus != nil }
}

@MainActor
@Observable
package final class DecisionFeedbackModel {
    static let advanceDelay: Duration = .milliseconds(800)
    static let dismissalDelay: Duration = .seconds(6)

    package private(set) var conclusion: DecisionConclusion?
    private var advanceTask: Task<Void, Never>?
    private var dismissalTask: Task<Void, Never>?
    private let announce: @MainActor (String) -> Void
    private let schedule: @MainActor (Duration, @escaping @MainActor () -> Void) -> Task<Void, Never>

    /// `init()` supplies the platform's accessibility announcement
    /// (`PlatformServices.swift`).
    package init(
        announce: @escaping @MainActor (String) -> Void,
        schedule:
            @escaping @MainActor (
                Duration, @escaping @MainActor () -> Void
            ) -> Task<Void, Never> = scheduleDecisionFeedback
    ) {
        self.announce = announce
        self.schedule = schedule
    }

    package func present(
        _ conclusion: DecisionConclusion,
        advance: @escaping @MainActor () -> Void
    ) {
        advanceTask?.cancel()
        dismissalTask?.cancel()
        self.conclusion = conclusion

        // The announcement is synchronous and precedes scheduling the focus
        // move, so assistive technology hears the result before selection
        // changes beneath it.
        announce("\(conclusion.actionLabel) applied.")

        // The advance is always scheduled; the closure decides where it goes,
        // so the destination (next item or the inbox) is the caller's choice.
        advanceTask = schedule(Self.advanceDelay) { [weak self] in
            guard self?.conclusion == conclusion else { return }
            advance()
        }
        dismissalTask = schedule(Self.dismissalDelay) { [weak self] in
            guard self?.conclusion == conclusion else { return }
            self?.conclusion = nil
        }
    }

    package func dismiss() {
        advanceTask?.cancel()
        dismissalTask?.cancel()
        conclusion = nil
    }

}
