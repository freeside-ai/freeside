import Foundation
import FreesideAPI
import ImageIO
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
/// by construction (plan §5.14). A digest names immutable content, so
/// settled immutable facts are served from memory while retained. Non-image
/// bytes and decoded images use loader-wide bounds; eviction makes a later
/// visit refetch non-image bytes, while an image denied by the active budget
/// retries when capacity is released or on the next card visit.
@MainActor
@Observable
public final class AttachmentLoader {
    public static let macOSMaxBytes = 64 << 20
    public static let macOSMaxImagePixels = 16_777_216

    #if os(macOS)
        /// The Mac is the bounded larger-viewer fallback named by the iOS UI.
        public static let defaultMaxBytes = macOSMaxBytes
        public static let defaultMaxImagePixels = macOSMaxImagePixels
    #else
        public static let defaultMaxBytes = 8 << 20
        public static let defaultMaxImagePixels = 4_194_304
    #endif

    public enum Phase: Equatable {
        public enum TooLargeReason: Equatable {
            case download(bytesSeenAtLeast: Int, limit: Int)
            case image(width: Int, height: Int, pixelLimit: Int)
            case imageBudget(width: Int, height: Int, pixelLimit: Int)
        }

        case loading
        case image(PlatformImage)
        /// Fetched fine, but the bytes are not a decodable image (a verify
        /// log, say). Eviction drops only the optional bytes: the explicit
        /// state and observed size stay rendered, and opening refetches.
        case notImage(bytes: Data?, byteCount: Int)
        /// Missing or failed fetch: the card shows a placeholder with
        /// the digest still visible, and the decision stays bound to
        /// the digest either way. Not settled — the next card visit
        /// retries, so a transient failure (or a digest uploaded after
        /// the first look) recovers without recreating the store.
        case unavailable
        /// The encoded stream or decoded bitmap crossed its inline cap.
        case tooLarge(TooLargeReason)
    }

    public typealias Sleep = @Sendable (Duration) async throws -> Void

    private enum DownloadResult: Sendable {
        case bytes(Data)
        case tooLarge(bytesSeenAtLeast: Int, limit: Int)
        case unavailable
        case timedOut
    }

    private enum DownloadEvent: Sendable {
        case progress(Int)
        case result(DownloadResult)
        case timedOut(Int)
    }

    private enum ImageInspectionResult: Sendable {
        case image(width: Int, height: Int)
        case notImage
    }

    /// The platform image wrapper is constructed only after the sendable
    /// decoded CGImage returns to MainActor.
    private struct DecodedImage: Sendable {
        let cgImage: CGImage
    }

    private final class DownloadActivity: @unchecked Sendable {
        private let lock = NSLock()
        private var generation = 0

        func recordProgress() -> Int {
            lock.lock()
            defer { lock.unlock() }
            generation += 1
            return generation
        }

        func isCurrent(_ candidate: Int) -> Bool {
            lock.lock()
            defer { lock.unlock() }
            return generation == candidate
        }
    }

    private struct DownloadWaiter {
        let id: UUID
        let continuation: CheckedContinuation<Void, any Error>
    }

    private let client: any APIProtocol
    /// Attachments render inline on a card, so anything past this size
    /// stops collecting and settles as non-renderable. Injectable so a
    /// test can exercise the cutoff without megabyte fixtures.
    private let maxBytes: Int
    private let maxImagePixels: Int
    private let maxRetainedImagePixels: Int
    private let maxRetainedNonImageBytes: Int
    private let maxConcurrentDownloads: Int
    private let loadTimeout: Duration
    private let sleep: Sleep
    private var phases: [String: Phase] = [:]
    private var retainedNonImageDigests: [String] = []
    private var retainedNonImageBytes = 0
    private var retainedImageDigests: [String] = []
    private var retainedImagePixelsByDigest: [String: Int] = [:]
    private var retainedImageDimensionsByDigest: [String: (width: Int, height: Int)] = [:]
    private var retainedImagePixels = 0
    private var reservedImagePixels = 0
    private var retryReservedImagePixelsByDigest: [String: Int] = [:]
    private var retryImageBudgetReasonsByDigest: [String: Phase.TooLargeReason] = [:]
    private var activeDownloads = 0
    private var downloadWaiters: [DownloadWaiter] = []
    private var inFlight: [String: Task<DownloadResult, Never>] = [:]
    private var visibleRows: [String: Int] = [:]
    private var discardAfterFetch: Set<String> = []
    private var disappearedDuringInFlight: Set<String> = []
    private var retryAfterReappearingInFlight: Set<String> = []
    @ObservationIgnored
    var beforeApplyingDownloadForTesting: (@Sendable (String) async -> Void)?
    @ObservationIgnored
    var afterApplyingDownloadForTesting: (@Sendable (String) async -> Void)?
    @ObservationIgnored
    var shouldFailImageDecodingForTesting: (@Sendable (String) async -> Bool)?

    public init(
        client: any APIProtocol,
        maxBytes: Int = AttachmentLoader.defaultMaxBytes,
        maxImagePixels: Int = AttachmentLoader.defaultMaxImagePixels,
        maxRetainedImagePixels: Int? = nil,
        maxRetainedNonImageBytes: Int? = nil,
        maxConcurrentDownloads: Int = 2,
        loadTimeout: Duration = .seconds(15),
        sleep: @escaping Sleep = { duration in
            try await ContinuousClock().sleep(for: duration)
        }
    ) {
        self.client = client
        self.maxBytes = maxBytes
        self.maxImagePixels = maxImagePixels
        self.maxRetainedImagePixels = maxRetainedImagePixels ?? maxImagePixels
        self.maxRetainedNonImageBytes = maxRetainedNonImageBytes ?? maxBytes
        self.maxConcurrentDownloads = maxConcurrentDownloads
        self.loadTimeout = loadTimeout
        self.sleep = sleep
        precondition(self.maxImagePixels > 0)
        precondition(self.maxRetainedImagePixels > 0)
        precondition(self.maxRetainedNonImageBytes >= maxBytes)
        precondition(self.maxConcurrentDownloads > 0)
    }

    public func phase(for digest: String) -> Phase? {
        phases[digest]
    }

    func beginDisplaying(_ digest: String) {
        visibleRows[digest, default: 0] += 1
        discardAfterFetch.remove(digest)
        if inFlight[digest] != nil, disappearedDuringInFlight.contains(digest) {
            retryAfterReappearingInFlight.insert(digest)
        }
    }

    func endDisplaying(_ digest: String) {
        guard let count = visibleRows[digest] else { return }
        if count > 1 {
            visibleRows[digest] = count - 1
            return
        }
        visibleRows[digest] = nil
        if let task = inFlight[digest] {
            disappearedDuringInFlight.insert(digest)
            discardAfterFetch.insert(digest)
            task.cancel()
        }
        discardRetainedPayload(for: digest)
    }

    /// Idempotent per digest while retained: immutable settled content is
    /// served from memory, unavailable content and evicted non-image bytes
    /// retry, and concurrent loads coalesce onto one request.
    public func load(_ digest: String) async {
        _ = await resolvedDownload(for: digest)
    }

    /// Gives an operator-selected budget-denied image priority over retained
    /// decoded images. Evicted rows remain explicit budget denials, so each can
    /// be selected again without exceeding the loader-wide pixel bound.
    func loadReplacingRetainedImages(_ digest: String) async {
        guard
            visibleRows[digest] != nil,
            inFlight[digest] == nil,
            case .tooLarge(.imageBudget(let width, let height, _)) = phases[digest],
            Self.imageFitsPixelLimit(
                width: width, height: height, maxPixels: maxRetainedImagePixels)
        else { return }

        let pixels = width * height
        guard pixels <= maxRetainedImagePixels - reservedImagePixels else { return }

        while pixels > maxRetainedImagePixels - retainedImagePixels - reservedImagePixels,
            let evictedDigest = retainedImageDigests.first
        {
            evictRetainedImageToBudgetDenial(evictedDigest)
        }

        reservedImagePixels += pixels
        retryReservedImagePixelsByDigest[digest] = pixels
        retryImageBudgetReasonsByDigest[digest] = .imageBudget(
            width: width,
            height: height,
            pixelLimit: maxRetainedImagePixels)
        phases[digest] = nil
        await load(digest)
    }

    /// Returns the coalesced task's value directly, so a concurrent cache
    /// insertion cannot evict these bytes between load completion and handoff.
    func nonImageBytes(for digest: String) async -> Data? {
        guard case .bytes(let bytes) = await resolvedDownload(for: digest) else { return nil }
        return bytes
    }

    private func resolvedDownload(for digest: String) async -> DownloadResult? {
        if let phase = phases[digest] {
            switch phase {
            case .image, .tooLarge:
                return nil
            case .loading:
                break
            case .notImage(let bytes, _):
                if let bytes { return .bytes(bytes) }
            case .unavailable:
                break
            }
        }
        if let running = inFlight[digest] {
            return await running.value
        }
        phases[digest] = .loading
        let task = Task {
            do {
                try await acquireDownloadSlot()
            } catch {
                await apply(.unavailable, to: digest, wasCancelled: Task.isCancelled)
                return DownloadResult.unavailable
            }
            defer { releaseDownloadSlot() }
            let result = await firstResult(for: digest)
            if let beforeApplyingDownloadForTesting {
                await beforeApplyingDownloadForTesting(digest)
            }
            await apply(result, to: digest, wasCancelled: Task.isCancelled)
            if let afterApplyingDownloadForTesting {
                await afterApplyingDownloadForTesting(digest)
            }
            return result
        }
        inFlight[digest] = task
        let result = await task.value
        inFlight[digest] = nil
        disappearedDuringInFlight.remove(digest)
        if visibleRows[digest] == nil, discardAfterFetch.remove(digest) != nil {
            discardRetainedPayload(for: digest)
        }
        if retryAfterReappearingInFlight.remove(digest) != nil,
            visibleRows[digest] != nil
        {
            switch phases[digest] {
            case .tooLarge(.imageBudget):
                retryVisibleImageBudgetDenials()
                return result
            case .unavailable, .notImage(bytes: nil, byteCount: _), nil:
                phases[digest] = nil
                return await resolvedDownload(for: digest)
            case .image, .loading, .notImage, .tooLarge:
                break
            }
        }
        retryVisibleImageBudgetDenials()
        return result
    }

    private func apply(
        _ result: DownloadResult,
        to digest: String,
        wasCancelled: Bool
    ) async {
        let retryReservedPixels = retryReservedImagePixelsByDigest[digest] ?? 0
        let retryBudgetReason = retryImageBudgetReasonsByDigest[digest]
        guard phases[digest] == .loading else {
            releaseImageRetryReservation(for: digest)
            retryVisibleImageBudgetDenials()
            return
        }
        let phase: Phase
        var imagePixelCount: Int?
        var imageDimensions: (width: Int, height: Int)?
        switch result {
        case .bytes(let bytes):
            let inspection = await Task.detached(priority: .userInitiated) {
                Self.inspectImage(bytes)
            }.value
            switch inspection {
            case .notImage:
                releaseImageRetryReservation(for: digest)
                phase = .notImage(bytes: bytes, byteCount: bytes.count)
            case .image(let width, let height):
                guard
                    Self.imageFitsPixelLimit(
                        width: width, height: height, maxPixels: maxImagePixels)
                else {
                    releaseImageRetryReservation(for: digest)
                    phase = .tooLarge(
                        .image(width: width, height: height, pixelLimit: maxImagePixels))
                    break
                }
                let pixels = width * height
                let availableImagePixels =
                    maxRetainedImagePixels - retainedImagePixels - reservedImagePixels
                    + retryReservedPixels
                guard pixels <= availableImagePixels else {
                    releaseImageRetryReservation(for: digest)
                    phase = .tooLarge(
                        .imageBudget(
                            width: width,
                            height: height,
                            pixelLimit: maxRetainedImagePixels))
                    break
                }
                releaseImageRetryReservation(for: digest)
                reservedImagePixels += pixels
                let failDecoding =
                    await shouldFailImageDecodingForTesting?(digest) ?? false
                let decoded: DecodedImage? =
                    if failDecoding {
                        nil
                    } else {
                        await Task.detached(priority: .userInitiated) {
                            Self.decodeAcceptedImage(bytes, width: width, height: height)
                        }.value
                    }
                reservedImagePixels -= pixels
                if let decoded {
                    phase = .image(Self.platformImage(from: decoded))
                    imagePixelCount = pixels
                    imageDimensions = (width, height)
                } else {
                    phase = .notImage(bytes: bytes, byteCount: bytes.count)
                }
            }
        case .tooLarge(let bytesSeenAtLeast, let limit):
            releaseImageRetryReservation(for: digest)
            phase = .tooLarge(
                .download(bytesSeenAtLeast: bytesSeenAtLeast, limit: limit))
        case .unavailable, .timedOut:
            releaseImageRetryReservation(for: digest)
            if wasCancelled, visibleRows[digest] != nil, let retryBudgetReason {
                phase = .tooLarge(retryBudgetReason)
            } else {
                phase = .unavailable
            }
        }
        store(
            phase,
            for: digest,
            imagePixelCount: imagePixelCount,
            imageDimensions: imageDimensions)
        retryVisibleImageBudgetDenials()
        if discardAfterFetch.remove(digest) != nil {
            discardRetainedPayload(for: digest)
        }
    }

    /// Inspection and decoding run outside MainActor. The actor reserves the
    /// accepted dimensions between these steps, so concurrent rows cannot
    /// overrun the loader-wide budget while only the first frame is decoded.
    nonisolated private static func inspectImage(_ bytes: Data) -> ImageInspectionResult {
        let sourceOptions: [CFString: Any] = [kCGImageSourceShouldCache: false]
        guard
            let source = CGImageSourceCreateWithData(
                bytes as CFData, sourceOptions as CFDictionary),
            CGImageSourceGetCount(source) > 0,
            let properties = CGImageSourceCopyPropertiesAtIndex(source, 0, nil)
                as? [CFString: Any],
            let width = positiveInteger(properties[kCGImagePropertyPixelWidth]),
            let height = positiveInteger(properties[kCGImagePropertyPixelHeight])
        else { return .notImage }
        return .image(width: width, height: height)
    }

    nonisolated private static func decodeAcceptedImage(
        _ bytes: Data,
        width: Int,
        height: Int
    ) -> DecodedImage? {
        let sourceOptions: [CFString: Any] = [kCGImageSourceShouldCache: false]
        guard
            let source = CGImageSourceCreateWithData(
                bytes as CFData, sourceOptions as CFDictionary)
        else { return nil }
        let options: [CFString: Any] = [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceCreateThumbnailWithTransform: true,
            kCGImageSourceShouldCacheImmediately: true,
            kCGImageSourceThumbnailMaxPixelSize: max(width, height),
        ]
        guard
            let image = CGImageSourceCreateThumbnailAtIndex(source, 0, options as CFDictionary)
        else { return nil }
        return DecodedImage(cgImage: image)
    }

    private static func platformImage(from decoded: DecodedImage) -> PlatformImage {
        #if canImport(UIKit)
            return UIImage(cgImage: decoded.cgImage)
        #elseif canImport(AppKit)
            return NSImage(
                cgImage: decoded.cgImage,
                size: NSSize(width: decoded.cgImage.width, height: decoded.cgImage.height))
        #endif
    }

    nonisolated private static func positiveInteger(_ value: Any?) -> Int? {
        guard let number = value as? NSNumber else { return nil }
        let candidate = number.int64Value
        guard
            candidate > 0,
            UInt64(candidate) <= UInt64(Int.max),
            number.compare(NSNumber(value: candidate)) == .orderedSame
        else { return nil }
        return Int(candidate)
    }

    nonisolated static func imageFitsPixelLimit(
        width: Int,
        height: Int,
        maxPixels: Int
    ) -> Bool {
        width > 0 && height > 0 && maxPixels > 0 && width <= maxPixels / height
    }

    static func macOSCanPreview(_ reason: Phase.TooLargeReason) -> Bool {
        switch reason {
        case .download(let bytesSeenAtLeast, _):
            return bytesSeenAtLeast <= macOSMaxBytes
        case .image(let width, let height, _),
            .imageBudget(let width, let height, _):
            return imageFitsPixelLimit(
                width: width,
                height: height,
                maxPixels: macOSMaxImagePixels)
        }
    }

    private func store(
        _ phase: Phase,
        for digest: String,
        imagePixelCount: Int? = nil,
        imageDimensions: (width: Int, height: Int)? = nil
    ) {
        if let previousPixels = retainedImagePixelsByDigest.removeValue(forKey: digest) {
            retainedImagePixels -= previousPixels
        }
        retainedImageDimensionsByDigest[digest] = nil
        retainedImageDigests.removeAll { $0 == digest }
        if case .notImage(let previous?, _) = phases[digest] {
            retainedNonImageBytes -= previous.count
            retainedNonImageDigests.removeAll { $0 == digest }
        }
        phases[digest] = phase
        if case .image = phase {
            let pixels = imagePixelCount ?? 0
            guard pixels > 0, let imageDimensions else {
                preconditionFailure("retained images require validated dimensions")
            }
            retainedImagePixelsByDigest[digest] = pixels
            retainedImageDimensionsByDigest[digest] = imageDimensions
            retainedImageDigests.append(digest)
            retainedImagePixels += pixels
        }
        guard case .notImage(let bytes?, _) = phase else { return }

        retainedNonImageBytes += bytes.count
        retainedNonImageDigests.append(digest)
        while retainedNonImageBytes > maxRetainedNonImageBytes,
            let evictedDigest = retainedNonImageDigests.first
        {
            retainedNonImageDigests.removeFirst()
            guard case .notImage(let evictedBytes?, let byteCount) = phases[evictedDigest] else {
                continue
            }
            phases[evictedDigest] = .notImage(bytes: nil, byteCount: byteCount)
            retainedNonImageBytes -= evictedBytes.count
        }
    }

    private func evictRetainedImageToBudgetDenial(_ digest: String) {
        guard
            let pixels = retainedImagePixelsByDigest[digest],
            let dimensions = retainedImageDimensionsByDigest[digest]
        else {
            preconditionFailure("retained image accounting is incomplete")
        }
        retainedImageDigests.removeAll { $0 == digest }
        retainedImagePixelsByDigest[digest] = nil
        retainedImageDimensionsByDigest[digest] = nil
        retainedImagePixels -= pixels
        phases[digest] = .tooLarge(
            .imageBudget(
                width: dimensions.width,
                height: dimensions.height,
                pixelLimit: maxRetainedImagePixels))
    }

    private func discardRetainedPayload(for digest: String) {
        switch phases[digest] {
        case .image:
            if let pixels = retainedImagePixelsByDigest.removeValue(forKey: digest) {
                retainedImagePixels -= pixels
            }
            retainedImageDimensionsByDigest[digest] = nil
            retainedImageDigests.removeAll { $0 == digest }
            phases[digest] = nil
            retryVisibleImageBudgetDenials()
        case .notImage(let bytes?, let byteCount):
            retainedNonImageBytes -= bytes.count
            retainedNonImageDigests.removeAll { $0 == digest }
            phases[digest] = .notImage(bytes: nil, byteCount: byteCount)
        case .tooLarge(.imageBudget):
            phases[digest] = nil
        case .unavailable:
            phases[digest] = nil
        case .loading, .notImage, .tooLarge, nil:
            break
        }
    }

    /// A budget denial is transient while another visible row owns the
    /// capacity. Releasing retained pixels restarts as many still-visible
    /// denied rows as the newly available budget can admit; download slots
    /// continue to bound their encoded buffers and decoding pipelines.
    private func retryVisibleImageBudgetDenials() {
        var availablePixels =
            maxRetainedImagePixels - retainedImagePixels - reservedImagePixels
        guard availablePixels > 0 else { return }

        let candidates = phases.compactMap {
            digest, phase -> (String, Int, Phase.TooLargeReason)? in
            guard
                visibleRows[digest] != nil,
                inFlight[digest] == nil,
                case .tooLarge(.imageBudget(let width, let height, _)) = phase,
                Self.imageFitsPixelLimit(
                    width: width, height: height, maxPixels: maxImagePixels)
            else { return nil }
            return (
                digest,
                width * height,
                .imageBudget(width: width, height: height, pixelLimit: maxRetainedImagePixels)
            )
        }.sorted { $0.0 < $1.0 }

        for (digest, pixels, reason) in candidates where pixels <= availablePixels {
            availablePixels -= pixels
            reservedImagePixels += pixels
            retryReservedImagePixelsByDigest[digest] = pixels
            retryImageBudgetReasonsByDigest[digest] = reason
            phases[digest] = nil
            Task { [weak self] in
                guard let self else { return }
                guard self.visibleRows[digest] != nil else {
                    self.releaseImageRetryReservation(for: digest)
                    self.retryVisibleImageBudgetDenials()
                    return
                }
                await self.load(digest)
            }
        }
    }

    private func releaseImageRetryReservation(for digest: String) {
        retryImageBudgetReasonsByDigest[digest] = nil
        guard let pixels = retryReservedImagePixelsByDigest.removeValue(forKey: digest) else {
            return
        }
        reservedImagePixels -= pixels
    }

    /// Returns as soon as either task produces a result. These are
    /// deliberately unstructured tasks: a transport that ignores cancellation
    /// must not keep the card spinning after the timeout wins. The loser can no
    /// longer mutate state and is cancelled before this function returns.
    private func firstResult(for digest: String) async -> DownloadResult {
        let (stream, continuation) = AsyncStream.makeStream(
            of: DownloadEvent.self,
            bufferingPolicy: .bufferingNewest(2)
        )
        let activity = DownloadActivity()
        let request = Task { [client, maxBytes] in
            let result = await Self.download(
                digest,
                client: client,
                maxBytes: maxBytes,
                onProgress: {
                    let generation = activity.recordProgress()
                    continuation.yield(.progress(generation))
                })
            guard !Task.isCancelled else { return }
            continuation.yield(.result(result))
        }
        func startTimeout(generation: Int) -> Task<Void, Never> {
            Task { [loadTimeout, sleep] in
                do {
                    try await sleep(loadTimeout)
                } catch {
                    return
                }
                guard !Task.isCancelled else { return }
                guard activity.isCurrent(generation) else { return }
                continuation.yield(.timedOut(generation))
            }
        }
        return await withTaskCancellationHandler {
            var timeoutGeneration = 0
            var timeout = startTimeout(generation: timeoutGeneration)
            defer {
                request.cancel()
                timeout.cancel()
                continuation.finish()
            }
            for await event in stream {
                switch event {
                case .progress(let generation):
                    guard generation > timeoutGeneration else { continue }
                    timeoutGeneration = generation
                    timeout.cancel()
                    timeout = startTimeout(generation: generation)
                case .result(let result):
                    return result
                case .timedOut(let generation):
                    // Progress records its generation before enqueueing the
                    // progress event. This actor check therefore invalidates an
                    // old timeout even when the main actor sees that queued
                    // timeout before it drains the corresponding progress event.
                    guard activity.isCurrent(generation) else { continue }
                    return .timedOut
                }
            }
            return .unavailable
        } onCancel: {
            request.cancel()
            continuation.finish()
        }
    }

    /// Limits the number of request bodies and post-download buffers that can
    /// accumulate independently. Each admitted pipeline is still bounded by
    /// `maxBytes`, so unbounded card row counts cannot multiply the encoded-byte
    /// budget without limit while inspection and decoding are in progress.
    private func acquireDownloadSlot() async throws {
        try Task.checkCancellation()
        guard activeDownloads >= maxConcurrentDownloads else {
            activeDownloads += 1
            return
        }
        let id = UUID()
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { continuation in
                downloadWaiters.append(DownloadWaiter(id: id, continuation: continuation))
            }
        } onCancel: {
            Task { @MainActor [weak self] in self?.cancelDownloadWaiter(id) }
        }
    }

    private func releaseDownloadSlot() {
        precondition(activeDownloads > 0)
        guard !downloadWaiters.isEmpty else {
            activeDownloads -= 1
            return
        }
        downloadWaiters.removeFirst().continuation.resume()
    }

    private func cancelDownloadWaiter(_ id: UUID) {
        guard let index = downloadWaiters.firstIndex(where: { $0.id == id }) else { return }
        let waiter = downloadWaiters.remove(at: index)
        waiter.continuation.resume(throwing: CancellationError())
    }

    var activeDownloadCountForTesting: Int { activeDownloads }
    var waitingDownloadCountForTesting: Int { downloadWaiters.count }
    var retainedImagePixelCountForTesting: Int { retainedImagePixels }

    nonisolated private static func download(
        _ digest: String,
        client: any APIProtocol,
        maxBytes: Int,
        onProgress: @escaping @Sendable () -> Void
    ) async -> DownloadResult {
        do {
            switch try await client.getAttachment(path: .init(digest: digest)) {
            case .ok(let ok):
                var bytes = Data()
                for try await chunk in try ok.body.binary {
                    guard !Task.isCancelled else { return .unavailable }
                    guard !chunk.isEmpty else { continue }
                    onProgress()
                    // The cutoff runs before the copy, so the cap
                    // bounds the accumulation even when one chunk
                    // would cross it. Oversize is a settled fact of the
                    // immutable content, not a transient failure.
                    guard bytes.count + chunk.count <= maxBytes else {
                        return .tooLarge(
                            bytesSeenAtLeast: bytes.count + chunk.count,
                            limit: maxBytes)
                    }
                    bytes.append(contentsOf: chunk)
                }
                return .bytes(bytes)
            case .notFound, .undocumented:
                return .unavailable
            }
        } catch {
            return .unavailable
        }
    }
}
