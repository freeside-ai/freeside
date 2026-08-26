import Foundation
import FreesideAPI
import HTTPTypes
import ImageIO
import OpenAPIRuntime
import Testing

@testable import FreesideCore

/// Counts matching operations, for single-flight assertions.
private actor RequestCounter {
    private(set) var count = 0

    @discardableResult
    func record() -> Int {
        count += 1
        return count
    }
}

/// Holds completed downloads before processing so tests can observe permit lifetime.
private actor DownloadProcessingProbe {
    private var isOpen = false
    private var active = 0
    private var entered: [String] = []
    private var peak = 0
    private var held: [CheckedContinuation<Void, Never>] = []
    private var startWaiters: [(Int, CheckedContinuation<Void, Never>)] = []

    func enter(_ digest: String) async {
        active += 1
        entered.append(digest)
        peak = max(peak, active)
        let arrivals = startWaiters.filter { $0.0 <= entered.count }
        startWaiters.removeAll { $0.0 <= entered.count }
        for (_, waiter) in arrivals { waiter.resume() }
        guard !isOpen else {
            active -= 1
            return
        }
        await withCheckedContinuation { held.append($0) }
        active -= 1
    }

    func waitUntilEntered(_ count: Int) async {
        if entered.count >= count { return }
        await withCheckedContinuation { startWaiters.append((count, $0)) }
    }

    func open() {
        isOpen = true
        let waiters = held
        held.removeAll()
        for waiter in waiters { waiter.resume() }
    }

    var enteredDigests: [String] { entered }
    var peakCount: Int { peak }
}

/// A cancellation-aware, manually released suspension point for timeout tests.
private actor ManualGate {
    private var waiter: (UUID, CheckedContinuation<Void, any Error>)?
    private var cancelled: Set<UUID> = []
    private var arrivalWaiters: [CheckedContinuation<Void, Never>] = []

    func wait() async throws {
        let id = UUID()
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation {
                (continuation: CheckedContinuation<Void, any Error>) in
                if cancelled.remove(id) != nil {
                    continuation.resume(throwing: CancellationError())
                    return
                }
                precondition(waiter == nil)
                waiter = (id, continuation)
                let arrivals = arrivalWaiters
                arrivalWaiters.removeAll()
                for arrival in arrivals { arrival.resume() }
            }
        } onCancel: {
            Task { await self.cancel(id) }
        }
    }

    func waitUntilSuspended() async {
        if waiter != nil { return }
        await withCheckedContinuation { arrivalWaiters.append($0) }
    }

    func open() {
        let continuation = waiter?.1
        waiter = nil
        continuation?.resume()
    }

    private func cancel(_ id: UUID) {
        guard waiter?.0 == id else {
            cancelled.insert(id)
            return
        }
        let continuation = waiter?.1
        waiter = nil
        continuation?.resume(throwing: CancellationError())
    }
}

/// Supplies one cancellation-aware gate per requested sleep and exposes the
/// number of timers started, so progress-reset behavior is deterministic.
private actor ManualSleeps {
    private let gates: [ManualGate]
    private var nextGate = 0
    private var startWaiters: [(Int, CheckedContinuation<Void, Never>)] = []

    init(_ gates: [ManualGate]) {
        self.gates = gates
    }

    func sleep() async throws {
        precondition(nextGate < gates.count)
        let gate = gates[nextGate]
        nextGate += 1
        let ready = startWaiters.filter { $0.0 <= nextGate }
        startWaiters.removeAll { $0.0 <= nextGate }
        for (_, waiter) in ready { waiter.resume() }
        try await gate.wait()
    }

    func waitUntilStarted(_ count: Int) async {
        if nextGate >= count { return }
        await withCheckedContinuation { startWaiters.append((count, $0)) }
    }

    var startedCount: Int {
        nextGate
    }
}

private final class ChunkStream: @unchecked Sendable {
    let stream: AsyncStream<HTTPBody.ByteChunk>
    private let continuation: AsyncStream<HTTPBody.ByteChunk>.Continuation

    init() {
        let pair = AsyncStream.makeStream(of: HTTPBody.ByteChunk.self)
        stream = pair.stream
        continuation = pair.continuation
    }

    func yield(_ bytes: some Collection<UInt8>) {
        continuation.yield(ArraySlice(bytes))
    }

    func finish() {
        continuation.finish()
    }
}

private struct StreamingAttachmentTransport: ClientTransport {
    let chunks: ChunkStream

    func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        guard operationID == "getAttachment" else {
            return (HTTPResponse(status: .notImplemented), nil)
        }
        return (
            HTTPResponse(
                status: .ok,
                headerFields: [.contentType: "application/octet-stream"]),
            HTTPBody(chunks.stream, length: .unknown)
        )
    }
}

/// The card-side attachment path (#103): bytes fetched by digest from
/// the mock, decoded by the platform, and cached in memory only.
@Suite @MainActor struct AttachmentLoaderTests {
    @Test func anImageDigestDecodesToTheImagePhase() async throws {
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: MockServer()))

        await loader.load("sha256:img-spec_approval")

        guard case .image = loader.phase(for: "sha256:img-spec_approval") else {
            Issue.record("expected .image, got \(String(describing: loader.phase(for: "sha256:img-spec_approval"))))")
            return
        }
    }

    @Test func anImageWhoseDecodedDimensionsExceedThePixelCapIsNotDecoded() async throws {
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: MockServer()),
            maxImagePixels: 63_999)

        await loader.load("sha256:img-spec_approval")

        #expect(
            loader.phase(for: "sha256:img-spec_approval")
                == .tooLarge(.image(width: 320, height: 200, pixelLimit: 63_999)))
    }

    @Test func thePixelCapAcceptsItsBoundaryAndRejectsInvalidOrOverflowingAreas() async throws {
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: MockServer()),
            maxImagePixels: 64_000)

        await loader.load("sha256:img-spec_approval")

        guard case .image = loader.phase(for: "sha256:img-spec_approval") else {
            Issue.record("expected the exact pixel-cap boundary to decode")
            return
        }
        #expect(!AttachmentLoader.imageFitsPixelLimit(width: 0, height: 1, maxPixels: 1))
        #expect(!AttachmentLoader.imageFitsPixelLimit(width: 1, height: -1, maxPixels: 1))
        #expect(
            !AttachmentLoader.imageFitsPixelLimit(
                width: Int.max, height: Int.max, maxPixels: Int.max))
    }

    @Test func aMultiFrameContainerDecodesOnlyTheValidatedFirstFrame() async throws {
        let firstFrame = try makeImage(width: 1, height: 1)
        let laterFrame = try makeImage(width: 2, height: 2)
        let bytes = try makeTIFF(frames: [firstFrame, laterFrame])
        let digest = "sha256:multi-frame"
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(
                server: MockServer(attachments: [digest: bytes])),
            maxImagePixels: 1)

        await loader.load(digest)

        guard case .image(let image) = loader.phase(for: digest) else {
            Issue.record("expected the validated first frame to decode")
            return
        }
        #if canImport(AppKit)
            #expect(image.representations.count == 1)
        #elseif canImport(UIKit)
            #expect(image.images == nil)
        #endif
    }

    @Test func retainedImagesStayWithinTheLoaderWidePixelBound() async throws {
        let firstDigest = "sha256:first-image"
        let secondDigest = "sha256:second-image"
        let bytes = try makeTIFF(frames: [makeImage(width: 1, height: 1)])
        let retryGate = ManualGate()
        let counter = RequestCounter()
        let server = MockServer(
            attachments: [firstDigest: bytes, secondDigest: bytes])
        await server.setBeforeRespond { operationID in
            guard operationID == "getAttachment" else { return }
            if await counter.record() == 3 { try await retryGate.wait() }
        }
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: server),
            maxImagePixels: 2,
            maxRetainedImagePixels: 1)

        loader.beginDisplaying(firstDigest)
        loader.beginDisplaying(secondDigest)
        async let firstLoad: Void = loader.load(firstDigest)
        async let secondLoad: Void = loader.load(secondDigest)
        _ = await (firstLoad, secondLoad)

        let firstPhase = loader.phase(for: firstDigest)
        let secondPhase = loader.phase(for: secondDigest)
        let firstAccepted: Bool
        if case .image = firstPhase { firstAccepted = true } else { firstAccepted = false }
        let secondAccepted: Bool
        if case .image = secondPhase { secondAccepted = true } else { secondAccepted = false }
        #expect(firstAccepted != secondAccepted)

        let deniedDigest: String
        let deniedPhase: AttachmentLoader.Phase?
        if firstAccepted {
            deniedDigest = secondDigest
            deniedPhase = secondPhase
        } else {
            deniedDigest = firstDigest
            deniedPhase = firstPhase
        }
        guard
            deniedPhase == .tooLarge(.imageBudget(width: 1, height: 1, pixelLimit: 1))
        else {
            Issue.record("expected one concurrent image to exhaust the active pixel budget")
            return
        }

        let acceptedDigest = firstAccepted ? firstDigest : secondDigest
        loader.endDisplaying(acceptedDigest)
        await retryGate.waitUntilSuspended()
        #expect(loader.phase(for: deniedDigest) == .loading)
        await retryGate.open()
        await loader.load(deniedDigest)
        guard case .image = loader.phase(for: deniedDigest) else {
            Issue.record("expected the visible image to retry after the active budget was released")
            return
        }
        #expect(await counter.count == 3)
    }

    @Test func operatorCanReplaceAnImageHoldingTheActivePixelBudget() async throws {
        let firstDigest = "sha256:first-replaceable-image"
        let secondDigest = "sha256:second-replaceable-image"
        let bytes = try makeTIFF(frames: [makeImage(width: 1, height: 1)])
        let counter = RequestCounter()
        let server = MockServer(
            attachments: [firstDigest: bytes, secondDigest: bytes])
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { await counter.record() }
        }
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: server),
            maxImagePixels: 1,
            maxRetainedImagePixels: 1)

        loader.beginDisplaying(firstDigest)
        loader.beginDisplaying(secondDigest)
        await loader.load(firstDigest)
        await loader.load(secondDigest)
        guard case .image = loader.phase(for: firstDigest) else {
            Issue.record("expected the first image to hold the active budget")
            return
        }
        #expect(
            loader.phase(for: secondDigest)
                == .tooLarge(.imageBudget(width: 1, height: 1, pixelLimit: 1)))

        await loader.loadReplacingRetainedImages(secondDigest)
        #expect(
            loader.phase(for: firstDigest)
                == .tooLarge(.imageBudget(width: 1, height: 1, pixelLimit: 1)))
        guard case .image = loader.phase(for: secondDigest) else {
            Issue.record("expected the selected second image to replace the first")
            return
        }
        #expect(loader.retainedImagePixelCountForTesting == 1)

        await loader.loadReplacingRetainedImages(firstDigest)
        guard case .image = loader.phase(for: firstDigest) else {
            Issue.record("expected the first image to remain selectable after eviction")
            return
        }
        #expect(
            loader.phase(for: secondDigest)
                == .tooLarge(.imageBudget(width: 1, height: 1, pixelLimit: 1)))
        #expect(loader.retainedImagePixelCountForTesting == 1)
        #expect(await counter.count == 4)
    }

    @Test func rapidImageReleasesDoNotOverbookBudgetRetries() async throws {
        let digests = (1...6).map { "sha256:budget-\($0)" }
        let bytes = try makeTIFF(frames: [makeImage(width: 1, height: 1)])
        let server = MockServer(
            attachments: Dictionary(uniqueKeysWithValues: digests.map { ($0, bytes) }))
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: server),
            maxImagePixels: 3,
            maxRetainedImagePixels: 3,
            maxConcurrentDownloads: 2)

        for digest in digests { loader.beginDisplaying(digest) }
        await withTaskGroup(of: Void.self) { group in
            for digest in digests {
                group.addTask { await loader.load(digest) }
            }
        }

        let accepted = digests.filter {
            if case .image = loader.phase(for: $0) { return true }
            return false
        }
        let denied = digests.filter {
            if case .tooLarge(.imageBudget) = loader.phase(for: $0) { return true }
            return false
        }
        guard accepted.count == 3, denied.count == 3 else {
            Issue.record("expected three accepted and three budget-denied images")
            return
        }

        let probe = DownloadProcessingProbe()
        loader.beforeApplyingDownloadForTesting = { await probe.enter($0) }
        loader.endDisplaying(accepted[0])
        loader.endDisplaying(accepted[1])
        await probe.waitUntilEntered(2)
        for _ in 0..<32 { await Task.yield() }

        #expect(loader.activeDownloadCountForTesting == 2)
        #expect(loader.waitingDownloadCountForTesting == 0)
        #expect(Set(await probe.enteredDigests) == Set(denied.sorted().prefix(2)))

        await probe.open()
        for digest in denied { await loader.load(digest) }
        let retriedImages = denied.filter {
            if case .image = loader.phase(for: $0) { return true }
            return false
        }
        #expect(retriedImages.count == 2)
    }

    @Test func aDecodeFailureReleasesCapacityToAVisibleDeniedImage() async throws {
        let failingDigest = "sha256:decode-failure"
        let deniedDigest = "sha256:denied-during-decode"
        let bytes = try makeTIFF(frames: [makeImage(width: 1, height: 1)])
        let decodeGate = ManualGate()
        let retryGate = ManualGate()
        let counter = RequestCounter()
        let server = MockServer(
            attachments: [failingDigest: bytes, deniedDigest: bytes])
        await server.setBeforeRespond { operationID in
            guard operationID == "getAttachment" else { return }
            if await counter.record() == 3 { try await retryGate.wait() }
        }
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: server),
            maxImagePixels: 1,
            maxRetainedImagePixels: 1)
        loader.shouldFailImageDecodingForTesting = { digest in
            guard digest == failingDigest else { return false }
            try? await decodeGate.wait()
            return true
        }

        loader.beginDisplaying(failingDigest)
        loader.beginDisplaying(deniedDigest)
        let failingLoad = Task { await loader.load(failingDigest) }
        await decodeGate.waitUntilSuspended()
        await loader.load(deniedDigest)
        #expect(
            loader.phase(for: deniedDigest)
                == .tooLarge(.imageBudget(width: 1, height: 1, pixelLimit: 1)))

        await decodeGate.open()
        await failingLoad.value
        await retryGate.waitUntilSuspended()
        #expect(loader.phase(for: deniedDigest) == .loading)
        await retryGate.open()
        await loader.load(deniedDigest)
        guard case .image = loader.phase(for: deniedDigest) else {
            Issue.record("expected the decode failure to release capacity to the denied image")
            return
        }
        #expect(await counter.count == 3)
    }

    @Test func aReappearingCanceledBudgetRetryStartsAFreshRequest() async throws {
        let retainedDigest = "sha256:retained-before-retry"
        let deniedDigest = "sha256:reappearing-retry"
        let bytes = try makeTIFF(frames: [makeImage(width: 1, height: 1)])
        let canceledRetryGate = ManualGate()
        let recoveryGate = ManualGate()
        let counter = RequestCounter()
        let server = MockServer(
            attachments: [retainedDigest: bytes, deniedDigest: bytes])
        await server.setBeforeRespond { operationID in
            guard operationID == "getAttachment" else { return }
            switch await counter.record() {
            case 3: try await canceledRetryGate.wait()
            case 4: try await recoveryGate.wait()
            default: break
            }
        }
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: server),
            maxImagePixels: 1,
            maxRetainedImagePixels: 1)

        loader.beginDisplaying(retainedDigest)
        loader.beginDisplaying(deniedDigest)
        await loader.load(retainedDigest)
        await loader.load(deniedDigest)
        #expect(
            loader.phase(for: deniedDigest)
                == .tooLarge(.imageBudget(width: 1, height: 1, pixelLimit: 1)))

        loader.endDisplaying(retainedDigest)
        await canceledRetryGate.waitUntilSuspended()
        loader.endDisplaying(deniedDigest)
        loader.beginDisplaying(deniedDigest)
        let reappearingLoad = Task { await loader.load(deniedDigest) }

        await recoveryGate.waitUntilSuspended()
        #expect(loader.phase(for: deniedDigest) == .loading)
        await recoveryGate.open()
        await reappearingLoad.value
        await loader.load(deniedDigest)
        guard case .image = loader.phase(for: deniedDigest) else {
            Issue.record("expected the reappearing row to recover from its canceled retry")
            return
        }
        #expect(await counter.count == 4)
    }

    @Test func aReappearingCanceledInitialLoadStartsAFreshRequest() async throws {
        let digest = "sha256:reappearing-initial-load"
        let bytes = try makeTIFF(frames: [makeImage(width: 1, height: 1)])
        let canceledRequestGate = ManualGate()
        let recoveryGate = ManualGate()
        let counter = RequestCounter()
        let server = MockServer(attachments: [digest: bytes])
        await server.setBeforeRespond { operationID in
            guard operationID == "getAttachment" else { return }
            switch await counter.record() {
            case 1: try await canceledRequestGate.wait()
            case 2: try await recoveryGate.wait()
            default: break
            }
        }
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: server))

        loader.beginDisplaying(digest)
        let firstLoad = Task { await loader.load(digest) }
        await canceledRequestGate.waitUntilSuspended()
        loader.endDisplaying(digest)
        loader.beginDisplaying(digest)
        let reappearingLoad = Task { await loader.load(digest) }

        await recoveryGate.waitUntilSuspended()
        #expect(loader.phase(for: digest) == .loading)
        await recoveryGate.open()
        await firstLoad.value
        await reappearingLoad.value
        await loader.load(digest)
        guard case .image = loader.phase(for: digest) else {
            Issue.record("expected the reappearing row to restart its canceled initial load")
            return
        }
        #expect(await counter.count == 2)
    }

    @Test func aReappearingAppliedLoadStartsAfterInFlightCleanup() async throws {
        let digest = "sha256:reappearing-applied-load"
        let bytes = try makeTIFF(frames: [makeImage(width: 1, height: 1)])
        let appliedProbe = DownloadProcessingProbe()
        let recoveryGate = ManualGate()
        let counter = RequestCounter()
        let server = MockServer(attachments: [digest: bytes])
        await server.setBeforeRespond { operationID in
            guard operationID == "getAttachment" else { return }
            if await counter.record() == 2 { try await recoveryGate.wait() }
        }
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: server))
        loader.afterApplyingDownloadForTesting = { await appliedProbe.enter($0) }

        loader.beginDisplaying(digest)
        let firstLoad = Task { await loader.load(digest) }
        await appliedProbe.waitUntilEntered(1)
        guard case .image = loader.phase(for: digest) else {
            Issue.record("expected the first response to apply before cleanup")
            return
        }

        loader.endDisplaying(digest)
        loader.beginDisplaying(digest)
        let reappearingLoad = Task { await loader.load(digest) }
        await appliedProbe.open()

        await recoveryGate.waitUntilSuspended()
        #expect(loader.phase(for: digest) == .loading)
        await recoveryGate.open()
        await firstLoad.value
        await reappearingLoad.value
        await loader.load(digest)
        guard case .image = loader.phase(for: digest) else {
            Issue.record("expected the applied load to restart after in-flight cleanup")
            return
        }
        #expect(await counter.count == 2)
    }

    @Test func reappearanceSurvivesApplyConsumingTheDiscardMarker() async throws {
        let digest = "sha256:reappearing-across-apply"
        let bytes = try makeTIFF(frames: [makeImage(width: 1, height: 1)])
        let beforeApplyProbe = DownloadProcessingProbe()
        let afterApplyProbe = DownloadProcessingProbe()
        let recoveryGate = ManualGate()
        let counter = RequestCounter()
        let server = MockServer(attachments: [digest: bytes])
        await server.setBeforeRespond { operationID in
            guard operationID == "getAttachment" else { return }
            if await counter.record() == 2 { try await recoveryGate.wait() }
        }
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: server))
        loader.beforeApplyingDownloadForTesting = { await beforeApplyProbe.enter($0) }
        loader.afterApplyingDownloadForTesting = { await afterApplyProbe.enter($0) }

        loader.beginDisplaying(digest)
        let firstLoad = Task { await loader.load(digest) }
        await beforeApplyProbe.waitUntilEntered(1)
        loader.endDisplaying(digest)
        await beforeApplyProbe.open()

        await afterApplyProbe.waitUntilEntered(1)
        #expect(loader.phase(for: digest) == nil)
        loader.beginDisplaying(digest)
        let reappearingLoad = Task { await loader.load(digest) }
        await afterApplyProbe.open()

        await recoveryGate.waitUntilSuspended()
        #expect(loader.phase(for: digest) == .loading)
        await recoveryGate.open()
        await firstLoad.value
        await reappearingLoad.value
        await loader.load(digest)
        guard case .image = loader.phase(for: digest) else {
            Issue.record("expected reappearance to survive apply consuming the discard marker")
            return
        }
        #expect(await counter.count == 2)
    }

    @Test func aNonImageAttachmentIsNotAFailure() async throws {
        // A verify log fetches fine but does not decode: the card keeps
        // its plain digest row, distinct from the unavailable placeholder.
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: MockServer()))

        await loader.load("sha256:log-spec_approval")

        guard
            case .notImage(let bytes?, let byteCount) =
                loader.phase(for: "sha256:log-spec_approval")
        else {
            Issue.record("expected .notImage")
            return
        }
        #expect(bytes == Data("verify log for spec_approval\n".utf8))
        #expect(byteCount == bytes.count)
    }

    private func makeImage(width: Int, height: Int) throws -> CGImage {
        let context = try #require(
            CGContext(
                data: nil,
                width: width,
                height: height,
                bitsPerComponent: 8,
                bytesPerRow: width * 4,
                space: CGColorSpaceCreateDeviceRGB(),
                bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue))
        return try #require(context.makeImage())
    }

    private func makeTIFF(frames: [CGImage]) throws -> Data {
        let data = NSMutableData()
        let destination = try #require(
            CGImageDestinationCreateWithData(
                data, "public.tiff" as CFString, frames.count, nil))
        for frame in frames { CGImageDestinationAddImage(destination, frame, nil) }
        #expect(CGImageDestinationFinalize(destination))
        return data as Data
    }

    @Test func nonImageTextPreviewIsCappedIndependentlyOfTheDownloadLimit() {
        let bytes = Data(
            repeating: 0x61,
            count: DecisionDetailView.NonImagePreview.textByteLimit + 1)
        let preview = DecisionDetailView.NonImagePreview(bytes: bytes)

        #expect(preview.byteCount == bytes.count)
        #expect(preview.text?.utf8.count == DecisionDetailView.NonImagePreview.textByteLimit)
        #expect(preview.isTruncated)
        #expect(DecisionDetailView.NonImagePreview(bytes: Data([0xFF])).text == nil)

        var splitScalar = Data(
            repeating: 0x61,
            count: DecisionDetailView.NonImagePreview.textByteLimit - 1)
        splitScalar.append(contentsOf: [0xF0, 0x9F, 0x98, 0x80])
        #expect(
            DecisionDetailView.NonImagePreview(bytes: splitScalar).text?.utf8.count
                == DecisionDetailView.NonImagePreview.textByteLimit - 1)
    }

    @Test func aMissingDigestAndATransportFailureAreUnavailable() async throws {
        let server = MockServer()
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: server))

        // The deliberately unseeded fixture digest: an authoritative 404.
        await loader.load("sha256:img-blocked")
        #expect(loader.phase(for: "sha256:img-blocked") == .unavailable)

        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { throw InjectedFailure() }
        }
        await loader.load("sha256:img-agent_question")
        #expect(loader.phase(for: "sha256:img-agent_question") == .unavailable)
    }

    @Test func anUnavailableDigestRetriesOnTheNextVisit() async throws {
        // Unavailable is not settled (Codex P2 on #126): a transient
        // failure at first look must not stick the placeholder for the
        // whole session — the next card visit retries and recovers.
        let server = MockServer()
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: server))
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { throw InjectedFailure() }
        }
        await loader.load("sha256:img-spec_approval")
        #expect(loader.phase(for: "sha256:img-spec_approval") == .unavailable)

        await server.setBeforeRespond(nil)
        await loader.load("sha256:img-spec_approval")
        guard case .image = loader.phase(for: "sha256:img-spec_approval") else {
            Issue.record("expected the retry to recover the image")
            return
        }
    }

    @Test func anOversizedAttachmentSettlesWithoutRetrying() async throws {
        // Oversize is a fact of the immutable content, not a transient
        // failure (Codex P2 on #126): the loader stops collecting at
        // the cutoff, keeps the plain digest row, and never re-downloads
        // on later card visits.
        let server = MockServer(
            attachments: ["sha256:img-huge": Data(repeating: 0x41, count: 64)])
        let counter = RequestCounter()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { await counter.record() }
        }
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: server), maxBytes: 16)

        await loader.load("sha256:img-huge")
        #expect(
            loader.phase(for: "sha256:img-huge")
                == .tooLarge(.download(bytesSeenAtLeast: 64, limit: 16)))

        await loader.load("sha256:img-huge")
        #expect(await counter.count == 1)
    }

    @Test func aStalledLoadTimesOutAndTheNextVisitRecovers() async throws {
        let server = MockServer()
        let requestGate = ManualGate()
        let timeout = ManualGate()
        let sleeps = ManualSleeps([timeout, ManualGate(), ManualGate()])
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { try await requestGate.wait() }
        }
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: server),
            loadTimeout: .seconds(30),
            sleep: { _ in try await sleeps.sleep() })
        let stalledLoad = Task { await loader.load("sha256:img-spec_approval") }

        await requestGate.waitUntilSuspended()
        await sleeps.waitUntilStarted(1)
        #expect(loader.phase(for: "sha256:img-spec_approval") == .loading)

        await timeout.open()
        await stalledLoad.value
        #expect(loader.phase(for: "sha256:img-spec_approval") == .unavailable)

        await server.setBeforeRespond(nil)
        await loader.load("sha256:img-spec_approval")
        guard case .image = loader.phase(for: "sha256:img-spec_approval") else {
            Issue.record("expected the post-timeout retry to recover the image")
            return
        }
    }

    @Test func downloadProgressRestartsTheStallTimeout() async throws {
        let chunks = ChunkStream()
        let firstTimer = ManualGate()
        let secondTimer = ManualGate()
        let thirdTimer = ManualGate()
        let sleeps = ManualSleeps([firstTimer, secondTimer, thirdTimer])
        let serverURL = try #require(URL(string: "https://freeside.invalid"))
        let client = APIClientFactory.live(
            serverURL: serverURL,
            transport: StreamingAttachmentTransport(chunks: chunks))
        let loader = AttachmentLoader(
            client: client,
            loadTimeout: .seconds(30),
            sleep: { _ in try await sleeps.sleep() })
        let load = Task { await loader.load("sha256:streaming-log") }

        await sleeps.waitUntilStarted(1)
        chunks.yield(Data("first ".utf8))
        await sleeps.waitUntilStarted(2)
        await firstTimer.open()
        await Task.yield()
        #expect(loader.phase(for: "sha256:streaming-log") == .loading)

        chunks.yield(Data("second".utf8))
        await sleeps.waitUntilStarted(3)
        await secondTimer.open()
        await Task.yield()
        #expect(loader.phase(for: "sha256:streaming-log") == .loading)

        chunks.finish()
        await load.value
        #expect(
            loader.phase(for: "sha256:streaming-log")
                == .notImage(
                    bytes: Data("first second".utf8),
                    byteCount: Data("first second".utf8).count))
    }

    @Test func emptyChunksDoNotResetTheStallTimeout() async throws {
        let chunks = ChunkStream()
        let timers = (0..<17).map { _ in ManualGate() }
        let sleeps = ManualSleeps(timers)
        let serverURL = try #require(URL(string: "https://freeside.invalid"))
        let client = APIClientFactory.live(
            serverURL: serverURL,
            transport: StreamingAttachmentTransport(chunks: chunks))
        let loader = AttachmentLoader(
            client: client,
            loadTimeout: .seconds(30),
            sleep: { _ in try await sleeps.sleep() })
        let load = Task { await loader.load("sha256:empty-stream") }

        await sleeps.waitUntilStarted(1)
        for _ in 0..<16 { chunks.yield(Data()) }
        for _ in 0..<32 { await Task.yield() }
        #expect(await sleeps.startedCount == 1)

        for timer in timers { await timer.open() }
        await load.value
        #expect(loader.phase(for: "sha256:empty-stream") == .unavailable)
        chunks.finish()
    }

    @Test func retainedNonImageBytesStayWithinTheLoaderWideBound() async throws {
        let firstDigest = "sha256:first-log"
        let secondDigest = "sha256:second-log"
        let server = MockServer(
            attachments: [
                firstDigest: Data("first!".utf8),
                secondDigest: Data("second".utf8),
            ])
        let counter = RequestCounter()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { await counter.record() }
        }
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: server),
            maxBytes: 8,
            maxRetainedNonImageBytes: 8)

        await loader.load(firstDigest)
        await loader.load(secondDigest)
        #expect(loader.phase(for: firstDigest) == .notImage(bytes: nil, byteCount: 6))
        #expect(
            loader.phase(for: secondDigest)
                == .notImage(bytes: Data("second".utf8), byteCount: 6))

        let openedBytes = await loader.nonImageBytes(for: firstDigest)
        #expect(openedBytes == Data("first!".utf8))
        #expect(await counter.count == 3)
        #expect(loader.phase(for: secondDigest) == .notImage(bytes: nil, byteCount: 6))
        #expect(
            loader.phase(for: firstDigest)
                == .notImage(bytes: Data("first!".utf8), byteCount: 6))
    }

    @Test func loadsAreSingleFlightAndSettledDigestsNeverRefetch() async throws {
        let server = MockServer()
        let counter = RequestCounter()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { await counter.record() }
        }
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: server))

        // Concurrent loads of one digest coalesce onto one request, as
        // a card's evidence row and claim row sharing a digest would.
        async let first: Void = loader.load("sha256:img-spec_approval")
        async let second: Void = loader.load("sha256:img-spec_approval")
        _ = await (first, second)
        #expect(await counter.count == 1)

        // A settled digest serves from memory: content is immutable per
        // digest, so revisiting the card refetches nothing.
        await loader.load("sha256:img-spec_approval")
        #expect(await counter.count == 1)
    }

    @Test func distinctAttachmentDownloadsStayWithinTheLoaderWideConcurrencyBound() async {
        let digests = (1...4).map { "sha256:concurrent-\($0)" }
        let attachments = Dictionary(
            uniqueKeysWithValues: digests.map { ($0, Data("log".utf8)) })
        let server = MockServer(attachments: attachments)
        let probe = DownloadProcessingProbe()
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: server),
            maxConcurrentDownloads: 2)
        loader.beforeApplyingDownloadForTesting = { await probe.enter($0) }

        let loads = digests.map { digest in Task { await loader.load(digest) } }
        await probe.waitUntilEntered(2)

        #expect(loader.activeDownloadCountForTesting == 2)
        #expect(await probe.peakCount == 2)

        await probe.open()
        for load in loads { await load.value }
        #expect(await probe.enteredDigests.count == digests.count)
        #expect(await probe.peakCount == 2)
    }

    @Test func invisibleQueuedDownloadsDoNotStarveNewlyVisibleAttachments() async {
        let firstDigest = "sha256:first-visible"
        let staleDigest = "sha256:departed"
        let freshDigest = "sha256:newly-visible"
        let attachments = Dictionary(
            uniqueKeysWithValues: [firstDigest, staleDigest, freshDigest].map {
                ($0, Data("log".utf8))
            })
        let server = MockServer(attachments: attachments)
        let probe = DownloadProcessingProbe()
        let loader = AttachmentLoader(
            client: APIClientFactory.mock(server: server),
            maxConcurrentDownloads: 1)
        loader.beforeApplyingDownloadForTesting = { await probe.enter($0) }

        loader.beginDisplaying(firstDigest)
        let firstLoad = Task { await loader.load(firstDigest) }
        await probe.waitUntilEntered(1)

        loader.beginDisplaying(staleDigest)
        let staleLoad = Task { await loader.load(staleDigest) }
        for _ in 0..<32 where loader.waitingDownloadCountForTesting == 0 {
            await Task.yield()
        }
        #expect(loader.waitingDownloadCountForTesting == 1)

        loader.endDisplaying(staleDigest)
        await staleLoad.value
        #expect(loader.waitingDownloadCountForTesting == 0)
        #expect(loader.phase(for: staleDigest) == nil)

        loader.beginDisplaying(freshDigest)
        let freshLoad = Task { await loader.load(freshDigest) }
        for _ in 0..<32 where loader.waitingDownloadCountForTesting == 0 {
            await Task.yield()
        }
        #expect(loader.waitingDownloadCountForTesting == 1)

        await probe.open()
        await firstLoad.value
        await freshLoad.value
        #expect(await probe.enteredDigests == [firstDigest, freshDigest])
    }

    @Test func aConcurrentCallerWaitsForTheSingleFlightResult() async throws {
        let server = MockServer()
        let requestGate = ManualGate()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { try await requestGate.wait() }
        }
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: server))
        let first = Task { await loader.load("sha256:img-spec_approval") }
        await requestGate.waitUntilSuspended()

        let started = AsyncStream.makeStream(of: Void.self)
        let second = Task { @MainActor in
            started.continuation.yield()
            started.continuation.finish()
            await loader.load("sha256:img-spec_approval")
            return loader.phase(for: "sha256:img-spec_approval") == .loading
        }
        for await _ in started.stream { break }

        await requestGate.open()
        await first.value
        #expect(await second.value == false)
    }

    @Test func imageBytesAreReleasedAfterTheLastVisibleRowDisappears() async {
        let counter = RequestCounter()
        let server = MockServer()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { await counter.record() }
        }
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: server))
        let digest = "sha256:img-spec_approval"
        loader.beginDisplaying(digest)
        loader.beginDisplaying(digest)
        await loader.load(digest)
        #expect(await counter.count == 1)

        loader.endDisplaying(digest)
        #expect(loader.phase(for: digest) != nil)
        loader.endDisplaying(digest)
        #expect(loader.phase(for: digest) == nil)

        loader.beginDisplaying(digest)
        await loader.load(digest)
        #expect(await counter.count == 2)
    }

    @Test func anImageFinishingAfterItsRowDisappearsIsNotRetained() async {
        let server = MockServer()
        let requestGate = ManualGate()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { try await requestGate.wait() }
        }
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: server))
        let digest = "sha256:img-spec_approval"
        loader.beginDisplaying(digest)
        let load = Task { await loader.load(digest) }
        await requestGate.waitUntilSuspended()

        loader.endDisplaying(digest)
        await requestGate.open()
        await load.value
        #expect(loader.phase(for: digest) == nil)
    }
}
