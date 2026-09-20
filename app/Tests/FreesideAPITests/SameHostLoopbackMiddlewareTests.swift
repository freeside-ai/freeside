#if os(macOS)
    import Foundation
    import FreesideAPI
    import HTTPTypes
    import OpenAPIRuntime
    import Testing

    /// A transport that records the base URL and Authorization header each request
    /// reaches it with, then returns a bare 200. The client fails to decode the
    /// empty body, so callers ignore the result and read `records`.
    private final class RecordingTransport: ClientTransport, @unchecked Sendable {
        struct Record: Sendable {
            let url: URL
            let authorization: String?
        }

        private let lock = NSLock()
        private var stored: [Record] = []

        var records: [Record] {
            lock.lock()
            defer { lock.unlock() }
            return stored
        }

        func send(
            _ request: HTTPRequest,
            body: HTTPBody?,
            baseURL: URL,
            operationID: String
        ) async throws -> (HTTPResponse, HTTPBody?) {
            lock.withLock {
                stored.append(.init(url: baseURL, authorization: request.headerFields[.authorization]))
            }
            return (HTTPResponse(status: .ok), nil)
        }
    }

    /// A mutable address set the test flips to model Tailscale appearing and
    /// disappearing on this Mac.
    private final class AddressBox: @unchecked Sendable {
        private let lock = NSLock()
        private var addresses: Set<String>

        init(_ addresses: Set<String>) {
            self.addresses = addresses
        }

        func set(_ value: Set<String>) {
            lock.lock()
            addresses = value
            lock.unlock()
        }

        func get() -> Set<String> {
            lock.lock()
            defer { lock.unlock() }
            return addresses
        }
    }

    @Suite struct SameHostLoopbackMiddlewareTests {
        // swift-format-ignore: NeverForceUnwrap
        private func url(_ string: String) -> URL { URL(string: string)! }

        @Test func localTailscaleAddressesMapToLoopbackKeepingPortAndPath() {
            let v4 = SameHostLoopbackMiddleware.loopbackURL(
                for: url("http://100.64.0.7:8677/sync"),
                localAddresses: ["100.64.0.7"]
            )
            #expect(v4 == url("http://127.0.0.1:8677/sync"))

            let v6 = SameHostLoopbackMiddleware.loopbackURL(
                for: url("http://[fd7a:115c:a1e0::7]:8677/sync"),
                localAddresses: ["fd7a:115c:a1e0::7"]
            )
            #expect(v6 == url("http://127.0.0.1:8677/sync"))
        }

        @Test func nonQualifyingAddressesReturnNil() {
            // A Tailscale IP that is not assigned to this Mac.
            #expect(
                SameHostLoopbackMiddleware.loopbackURL(
                    for: url("http://100.64.0.7:8677"), localAddresses: ["100.64.0.8"]
                ) == nil)
            // A local LAN address, in no Tailscale range.
            #expect(
                SameHostLoopbackMiddleware.loopbackURL(
                    for: url("http://192.168.1.10:8677"), localAddresses: ["192.168.1.10"]
                ) == nil)
            // A loopback host is already reachable and not a Tailscale range.
            #expect(
                SameHostLoopbackMiddleware.loopbackURL(
                    for: url("http://127.0.0.1:8677"), localAddresses: ["127.0.0.1"]
                ) == nil)
            // A hostname, not an IP literal.
            #expect(
                SameHostLoopbackMiddleware.loopbackURL(
                    for: url("http://daemon.local:8677"), localAddresses: ["daemon.local"]
                ) == nil)
            // An https URL is never redirected to plaintext loopback.
            #expect(
                SameHostLoopbackMiddleware.loopbackURL(
                    for: url("https://100.64.0.7:8677"), localAddresses: ["100.64.0.7"]
                ) == nil)
        }

        @Test func rangeBoundariesArePinned() {
            // Inclusive edges of 100.64.0.0/10 map when locally assigned.
            for host in ["100.64.0.0", "100.127.255.255"] {
                #expect(
                    SameHostLoopbackMiddleware.loopbackURL(
                        for: url("http://\(host):8677"), localAddresses: [host]
                    ) == url("http://127.0.0.1:8677"), "\(host) is inside the range")
            }
            // Just outside 100.64.0.0/10 never maps, even when locally assigned, so a
            // loosening of the second-octet bound can't slip through unnoticed.
            for host in ["100.63.255.255", "100.128.0.0"] {
                #expect(
                    SameHostLoopbackMiddleware.loopbackURL(
                        for: url("http://\(host):8677"), localAddresses: [host]
                    ) == nil, "\(host) is outside the range")
            }
            // Just outside fd7a:115c:a1e0::/48 (byte 5 off by one either way).
            for host in ["fd7a:115c:a1e1::7", "fd7a:115c:a1df::7"] {
                #expect(
                    SameHostLoopbackMiddleware.loopbackURL(
                        for: url("http://[\(host)]:8677"), localAddresses: [host]
                    ) == nil, "\(host) is outside the range")
            }
        }

        @Test func liveClientDialsLoopbackForALocalTailscaleAddress() async throws {
            let transport = RecordingTransport()
            let client = APIClientFactory.live(
                serverURL: url("http://100.64.0.7:8677"),
                transport: transport,
                token: { "device-token" },
                localAddresses: { ["100.64.0.7"] }
            )
            _ = try? await client.getSyncRevision()
            let record = try #require(transport.records.last)
            #expect(record.url == url("http://127.0.0.1:8677"))
            // The bearer credential still rides the loopback request.
            #expect(record.authorization == "Bearer device-token")
        }

        @Test func liveClientDialsThePairedAddressWhenNotLocal() async throws {
            let transport = RecordingTransport()
            let client = APIClientFactory.live(
                serverURL: url("http://100.64.0.7:8677"),
                transport: transport,
                token: { "device-token" },
                localAddresses: { ["100.64.0.8"] }
            )
            _ = try? await client.getSyncRevision()
            let record = try #require(transport.records.last)
            #expect(record.url == url("http://100.64.0.7:8677"))
            #expect(record.authorization == "Bearer device-token")
        }

        @Test func liveClientSwitchesToLoopbackOnceAndStaysAfterTheAddressDisappears() async throws {
            let transport = RecordingTransport()
            let box = AddressBox([])
            let client = APIClientFactory.live(
                serverURL: url("http://100.64.0.7:8677"),
                transport: transport,
                localAddresses: { box.get() }
            )

            // Absent: the paired address.
            _ = try? await client.getSyncRevision()
            #expect(transport.records.last?.url == url("http://100.64.0.7:8677"))

            // Present: loopback.
            box.set(["100.64.0.7"])
            _ = try? await client.getSyncRevision()
            #expect(transport.records.last?.url == url("http://127.0.0.1:8677"))

            // Gone again: still loopback for the rest of the process.
            box.set([])
            _ = try? await client.getSyncRevision()
            #expect(transport.records.last?.url == url("http://127.0.0.1:8677"))
        }
    }
#endif
