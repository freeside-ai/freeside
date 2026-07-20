import Foundation
import FreesideAPI
import Observation

#if canImport(UIKit)
    import UIKit
    /// The decoded-image currency per platform; the platform decoder is
    /// also the "is this an image?" ground truth, since no attachment
    /// field declares a media type.
    public typealias PlatformImage = UIImage
#elseif canImport(AppKit)
    import AppKit
    public typealias PlatformImage = NSImage
#endif

/// Fetches attachment bytes by content digest and decodes images for
/// inline rendering on decision cards (plan §4: cards render image
/// attachments directly from the artifact store by digest). Memory-only
/// by design: bytes never touch the disk cache, so the
/// no-high-sensitivity-at-rest default documented on CacheStore holds
/// by construction (plan §5.14). A digest names immutable content, so a
/// settled phase never invalidates and never refetches.
@MainActor
@Observable
public final class AttachmentLoader {
    public enum Phase: Equatable {
        case loading
        case image(PlatformImage)
        /// Fetched fine, but the bytes are not a decodable image (a
        /// verify log, say): rendered as the plain digest row, which is
        /// not a failure.
        case notImage
        /// Missing or failed fetch: the card shows a placeholder with
        /// the digest still visible, and the decision stays bound to
        /// the digest either way. Not settled — the next card visit
        /// retries, so a transient failure (or a digest uploaded after
        /// the first look) recovers without recreating the store.
        case unavailable
    }

    private let client: any APIProtocol
    /// Attachments render inline on a card, so anything past this size
    /// stops collecting and settles as non-renderable. Injectable so a
    /// test can exercise the cutoff without megabyte fixtures.
    private let maxBytes: Int
    private var phases: [String: Phase] = [:]
    private var inFlight: [String: Task<Void, Never>] = [:]

    public init(client: any APIProtocol, maxBytes: Int = 8 << 20) {
        self.client = client
        self.maxBytes = maxBytes
    }

    public func phase(for digest: String) -> Phase? {
        phases[digest]
    }

    /// Idempotent per digest: a settled digest (image or non-image —
    /// content is immutable per digest) is served from memory, an
    /// unavailable one retries (Codex P2 on #126: a transient failure
    /// must not stick for the session), and concurrent loads coalesce
    /// onto one request.
    public func load(_ digest: String) async {
        if let phase = phases[digest], phase != .loading, phase != .unavailable { return }
        if let running = inFlight[digest] {
            await running.value
            return
        }
        let task = Task { await fetch(digest) }
        inFlight[digest] = task
        await task.value
        inFlight[digest] = nil
    }

    private func fetch(_ digest: String) async {
        phases[digest] = .loading
        do {
            switch try await client.getAttachment(path: .init(digest: digest)) {
            case .ok(let ok):
                var bytes = Data()
                for try await chunk in try ok.body.binary {
                    // The cutoff runs before the copy, so the cap
                    // bounds the accumulation even when one chunk
                    // would cross it. Oversize is a settled fact of
                    // the immutable content, not a transient failure:
                    // stop collecting and keep the plain digest row,
                    // or a retryable phase would re-download megabytes
                    // on every card visit (Codex P2s on #126).
                    guard bytes.count + chunk.count <= maxBytes else {
                        phases[digest] = .notImage
                        return
                    }
                    bytes.append(contentsOf: chunk)
                }
                phases[digest] = PlatformImage(data: bytes).map { .image($0) } ?? .notImage
            case .notFound, .undocumented:
                phases[digest] = .unavailable
            }
        } catch {
            phases[digest] = .unavailable
        }
    }
}
