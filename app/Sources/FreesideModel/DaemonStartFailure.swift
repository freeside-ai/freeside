import Foundation

/// A fatal startup error recorded by `freesided` on stderr before exiting.
/// Launchd captures stderr to `<state root>/daemon/freesided.log`; a later
/// successful start writes structured log lines after it, so only a file whose
/// last non-empty line carries the `freesided: ` prefix reports a failure.
public struct DaemonStartFailure: Equatable, Sendable {
    public let message: String
    public let loggedAt: Date
    public let logURL: URL

    public init(message: String, loggedAt: Date, logURL: URL) {
        self.message = message
        self.loggedAt = loggedAt
        self.logURL = logURL
    }
}

/// Reads the tail of a supervised daemon's stderr log to extract its latest
/// startup error, if the last non-empty line is one.
public struct DaemonStartFailureReader: Sendable {
    public static let fileName = "freesided.log"
    public static let prefix = "freesided: "
    /// Bounds how much of a long-lived log file the reader inspects on each poll.
    public static let maxTailBytes = 64 * 1024

    private let modificationDate: @Sendable (URL) -> Date?

    public init(
        modificationDate: @escaping @Sendable (URL) -> Date? = { url in
            try? url.resourceValues(forKeys: [.contentModificationDateKey])
                .contentModificationDate
        }
    ) {
        self.modificationDate = modificationDate
    }

    /// Reads at most the last `maxTailBytes` of `url` and returns the failure
    /// when the last non-empty line starts with `freesided: `. Missing, empty,
    /// or unreadable files return nil.
    public func read(at url: URL) -> DaemonStartFailure? {
        if (try? url.resourceValues(forKeys: [.isDirectoryKey]).isDirectory) == true {
            return nil
        }
        guard let loggedAt = modificationDate(url) else { return nil }
        do {
            let handle = try FileHandle(forReadingFrom: url)
            defer { try? handle.close() }
            let size = try handle.seekToEnd()
            guard size > 0 else { return nil }
            let cap = UInt64(Self.maxTailBytes)
            let truncatedAtStart = size > cap
            let offset = truncatedAtStart ? size - cap : 0
            try handle.seek(toOffset: offset)
            guard let data = try handle.read(upToCount: Self.maxTailBytes), !data.isEmpty else {
                return nil
            }
            let tail = String(decoding: data, as: UTF8.self)
            guard let message = Self.parseMessage(from: tail, truncatedAtStart: truncatedAtStart) else {
                return nil
            }
            return DaemonStartFailure(message: message, loggedAt: loggedAt, logURL: url)
        } catch {
            return nil
        }
    }

    /// Extracts the stripped message from the last non-empty line of a log
    /// tail when that line begins with `freesided: `. When `truncatedAtStart`
    /// is true, the leading partial line is discarded so a mid-line occurrence
    /// of the prefix cannot be mistaken for a start error.
    public static func parseMessage(from tail: String, truncatedAtStart: Bool = false) -> String? {
        var slice = tail[...]
        if truncatedAtStart {
            guard let firstNewline = slice.firstIndex(where: \.isNewline) else { return nil }
            slice = slice[slice.index(after: firstNewline)...]
        }
        let lines = slice.split(whereSeparator: \.isNewline)
        guard
            let lastLine = lines.last(where: { !$0.trimmingCharacters(in: .whitespaces).isEmpty })?
                .trimmingCharacters(in: .whitespaces),
            lastLine.hasPrefix(prefix)
        else { return nil }
        let message = lastLine.dropFirst(prefix.count).trimmingCharacters(in: .whitespaces)
        return message.isEmpty ? nil : message
    }

    /// The stderr log file launchd writes in the daemon's `-state-dir`.
    public static func fileURL(inStateDirectory directory: URL) -> URL {
        directory.appendingPathComponent(fileName, isDirectory: false)
    }
}
