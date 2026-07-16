import FreesideAPI
import FreesideCore
import Foundation
import Testing

/// Counts matching operations, for single-flight assertions.
private actor RequestCounter {
    private(set) var count = 0

    func record() {
        count += 1
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

    @Test func aNonImageAttachmentIsNotAFailure() async throws {
        // A verify log fetches fine but does not decode: the card keeps
        // its plain digest row, distinct from the unavailable placeholder.
        let loader = AttachmentLoader(client: APIClientFactory.mock(server: MockServer()))

        await loader.load("sha256:log-spec_approval")

        #expect(loader.phase(for: "sha256:log-spec_approval") == .notImage)
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
        #expect(loader.phase(for: "sha256:img-huge") == .notImage)

        await loader.load("sha256:img-huge")
        #expect(await counter.count == 1)
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
}
