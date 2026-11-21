import FreesideAPI

/// The records a running writer's transcript sheet holds between polls
/// (#1926). The daemon serves complete records after the offset the sheet
/// already holds; the buffer appends them, keeps the offset for the next
/// read, and keeps a bounded tail, as the daemon keeps a bounded window.
///
/// It is display-only and unverified: nothing here is stored, and the sheet
/// drops it on any answer that isn't live, before it shows anything else.
package struct LiveTranscriptBuffer: Sendable, Equatable {
    /// The most bytes held. Past it, whole records leave from the front and
    /// the sheet says earlier activity isn't shown. It matches the daemon's
    /// window, so a sheet that keeps up never trims what the daemon serves.
    package static let byteLimit = 8 << 20

    package private(set) var bytes: [UInt8] = []
    /// The offset the next read sends as `after`: where the last complete
    /// record the daemon served ended.
    package private(set) var end: Int64 = 0
    /// Earlier records are not held: they left the daemon's window before
    /// the sheet read them, or this buffer's own bound.
    package private(set) var omittedBefore = false
    /// Records the daemon withheld whole for size or encoding since the
    /// writer started, as its latest answer counts them.
    package private(set) var unreadableRecords = 0

    package init() {}

    /// Appends one live answer read with `after` equal to `end`. An answer
    /// that skips ahead (the coordinator admits that only with
    /// `omitted_before`) replaces what is held, so the records shown are
    /// always contiguous under the line that says earlier ones are missing.
    package mutating func append(_ live: Components.Schemas.LiveTranscript) {
        if live.start != end {
            bytes = []
            omittedBefore = true
        }
        bytes.append(contentsOf: live.records.data)
        end = live.end
        unreadableRecords = live.unreadable_records
        guard bytes.count > Self.byteLimit else { return }
        let excess = bytes.count - Self.byteLimit
        // Cut after the first newline at or past the excess, so the front
        // keeps whole records.
        let cut = bytes[excess...].firstIndex(of: 10).map { $0 + 1 } ?? bytes.count
        bytes.removeFirst(cut)
        omittedBefore = true
    }

    /// What the sheet does after one answer.
    package enum Next: Equatable, Sendable {
        /// Show the held records.
        case showLive
        /// The writer's transcript is kept: open it.
        case keep
        /// The daemon stopped serving the records for a credential match.
        case withheld
        /// The daemon serves nothing for this invocation.
        case notKept
    }

    /// Takes one answer read with `after` equal to `end`. Any answer that
    /// isn't live drops every held record first, so no live bytes outlive
    /// the daemon's last live answer.
    package mutating func take(_ answer: Components.Schemas.InvocationTranscript) -> Next {
        guard answer.availability == .live, let live = answer.live?.value1 else {
            drop()
            switch answer.availability {
            case .available: return .keep
            case .withheld: return .withheld
            case .live, .unavailable, .unknown: return .notKept
            }
        }
        append(live)
        return .showLive
    }

    /// Drops everything, so the next read starts from 0.
    package mutating func drop() {
        self = Self()
    }
}
