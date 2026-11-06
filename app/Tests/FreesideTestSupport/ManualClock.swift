import Foundation

/// A clock that moves only when a test advances it, so code that sleeps runs
/// without waiting on wall-clock time. It records every sleep it is asked
/// for, in order, so a test awaits the next one and asserts its deadline
/// instead of timing it: a wrong wait fails an assertion, and a loaded
/// machine slows the test without failing it.
///
/// A cancelled sleep throws `CancellationError` at once, as
/// `ContinuousClock`'s does. A task group that cancels a sleeping child and
/// then waits for it, as the heartbeat loop's long poll does, would
/// otherwise wait until the test advances past the deadline.
public final class ManualClock: Clock, @unchecked Sendable {
    public struct Instant: InstantProtocol {
        /// Time since the clock was made.
        public let offset: Duration

        public init(offset: Duration) {
            self.offset = offset
        }

        public func advanced(by duration: Duration) -> Instant {
            Instant(offset: offset + duration)
        }

        public func duration(to other: Instant) -> Duration {
            other.offset - offset
        }

        public static func < (lhs: Instant, rhs: Instant) -> Bool {
            lhs.offset < rhs.offset
        }
    }

    private struct Sleeper {
        let id: UUID
        let deadline: Instant
        let continuation: CheckedContinuation<Void, any Error>
    }

    private let lock = NSLock()
    private var current = Instant(offset: .zero)
    private var pending: [Sleeper] = []
    private var requested: [Instant] = []
    private var requestWaiters: [(count: Int, continuation: CheckedContinuation<Void, Never>)] = []

    public init() {}

    public var now: Instant { lock.withLock { current } }

    public var minimumResolution: Duration { .zero }

    public func sleep(until deadline: Instant, tolerance: Duration? = nil) async throws {
        let id = UUID()
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, any Error>) in
                // The cancellation check sits under the lock that `onCancel`
                // takes, so a cancel that lands before the sleeper is pending
                // is seen here, and one that lands after finds it pending.
                let (outcome, arrived): (Result<Void, any Error>?, [CheckedContinuation<Void, Never>]) =
                    lock.withLock {
                        requested.append(deadline)
                        let arrived = takeArrivedRequestWaiters()
                        if Task.isCancelled { return (.failure(CancellationError()), arrived) }
                        if deadline <= current { return (.success(()), arrived) }
                        pending.append(Sleeper(id: id, deadline: deadline, continuation: continuation))
                        return (nil, arrived)
                    }
                for waiter in arrived { waiter.resume() }
                if let outcome { continuation.resume(with: outcome) }
            }
        } onCancel: {
            let cancelled = lock.withLock { () -> Sleeper? in
                guard let index = pending.firstIndex(where: { $0.id == id }) else { return nil }
                return pending.remove(at: index)
            }
            cancelled?.continuation.resume(throwing: CancellationError())
        }
    }

    /// The deadline of the `number`th sleep requested since the clock was
    /// made (1-based), waiting until that many have been requested. Every
    /// request counts, including one cancelled or already due when made.
    public func sleepRequest(_ number: Int) async -> Instant {
        await withCheckedContinuation { (continuation: CheckedContinuation<Void, Never>) in
            let ready = lock.withLock {
                if requested.count >= number { return true }
                requestWaiters.append((number, continuation))
                return false
            }
            if ready { continuation.resume() }
        }
        return lock.withLock { requested[number - 1] }
    }

    /// Moves the clock forward and wakes every sleeper whose deadline is now
    /// due.
    public func advance(by duration: Duration) {
        precondition(duration >= .zero, "a clock never moves backward")
        let due = lock.withLock {
            current = current.advanced(by: duration)
            let now = current
            let due = pending.filter { $0.deadline <= now }
            pending.removeAll { $0.deadline <= now }
            return due
        }
        for sleeper in due { sleeper.continuation.resume() }
    }

    /// Moves the clock to `instant`, which must not be in its past.
    public func advance(to instant: Instant) {
        advance(by: now.duration(to: instant))
    }

    /// Must be called holding `lock`.
    private func takeArrivedRequestWaiters() -> [CheckedContinuation<Void, Never>] {
        let count = requested.count
        let arrived = requestWaiters.filter { $0.count <= count }.map(\.continuation)
        requestWaiters.removeAll { $0.count <= count }
        return arrived
    }
}
