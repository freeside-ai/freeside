import Foundation
import FreesideAPI
import FreesideCore
import HTTPTypes
import OpenAPIRuntime
import OpenAPIURLSession

/// The two URLs a running freeside-signet-dev harness advertises. Both
/// must be present for the convergence suite to run; otherwise every
/// suite is disabled and plain `swift test` stays daemon-free.
enum ConvergenceEnvironment {
    static var apiURL: URL? {
        ProcessInfo.processInfo.environment["FREESIDE_CONVERGENCE_URL"]
            .flatMap(URL.init(string:))
    }

    static var controlURL: URL? {
        ProcessInfo.processInfo.environment["FREESIDE_CONVERGENCE_CONTROL_URL"]
            .flatMap(URL.init(string:))
    }

    static var isConfigured: Bool { apiURL != nil && controlURL != nil }
}

struct ConvergenceOutage: Error {}

/// The dev harness's choreography surface: what the in-process mock
/// exposed as direct actor methods (seed, advance, rotate) the real
/// daemon exposes only through freeside-signet-dev's control listener.
struct ControlClient {
    let baseURL: URL

    private func post(_ path: String, body: [String: Any]? = nil) async throws -> Data {
        var request = URLRequest(url: baseURL.appendingPathComponent(path))
        request.httpMethod = "POST"
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: body)
        }
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let http = response as? HTTPURLResponse, (200...299).contains(http.statusCode) else {
            throw ConvergenceOutage()
        }
        return data
    }

    /// Mints one single-use pairing code, the plaintext the daemon host
    /// would display on its terminal (plan §5.14).
    func mintPairingCode() async throws -> String {
        let data = try await post("control/pairing-codes")
        let payload = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        guard let code = payload?["pairing_code"] as? String, !code.isEmpty else {
            throw ConvergenceOutage()
        }
        return code
    }

    /// Issues a new sync epoch, the §5.14 restore simulation (test 8).
    func rotateEpoch() async throws {
        _ = try await post("control/epoch")
    }

    /// Seeds or advances one attention item; the harness constructs the
    /// item body server-side, so version bumps here are the real
    /// analogue of the mock's advance hook.
    func seedItem(id: String, version: Int) async throws {
        _ = try await post("control/items", body: ["id": id, "item_version": version])
    }
}

/// A client transport that fails matching operations before any bytes
/// leave the process: the real-daemon replacement for the mock's
/// setBeforeRespond hook ("the attempt never reached the daemon").
final class FailableTransport: ClientTransport, @unchecked Sendable {
    private let base: any ClientTransport
    private let lock = NSLock()
    private var failingOperations: Set<String> = []

    init(base: any ClientTransport = URLSessionTransport()) {
        self.base = base
    }

    func fail(operations: Set<String>) {
        lock.withLock { failingOperations = operations }
    }

    func restore() {
        lock.withLock { failingOperations = [] }
    }

    func send(
        _ request: HTTPRequest, body: HTTPBody?, baseURL: URL, operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        let failing = lock.withLock { failingOperations.contains(operationID) }
        if failing { throw ConvergenceOutage() }
        return try await base.send(request, body: body, baseURL: baseURL, operationID: operationID)
    }
}

/// One paired device against the real daemon: the credential from a
/// real mint-and-exchange, a live client whose bearer middleware holds
/// that token, and the identity commands submit under.
@MainActor
struct LiveDevice {
    let client: any APIProtocol
    let identity: DeviceIdentity
    let token: String
    let transport: FailableTransport

    var deviceID: String { identity.deviceID }
}

enum ConvergenceHarness {
    @MainActor
    static func control() throws -> ControlClient {
        guard let url = ConvergenceEnvironment.controlURL else { throw ConvergenceOutage() }
        return ControlClient(baseURL: url)
    }

    /// Walks the real pairing exchange (control mint, then the contract
    /// POST /pairing) and returns a device whose client authenticates
    /// with the granted token. In-memory stores back every test device:
    /// the pass targets client-daemon protocol convergence, and
    /// Keychain/disk custody is already unit-covered where it can fail
    /// honestly (headless CI cannot host a login keychain).
    @MainActor
    static func pairDevice(displayName: String) async throws -> LiveDevice {
        guard let apiURL = ConvergenceEnvironment.apiURL else { throw ConvergenceOutage() }
        let code = try await control().mintPairingCode()
        let grant = try await APIClientFactory.live(serverURL: apiURL).pairDevice(
            body: .json(.init(pairing_code: code, display_name: displayName))
        ).created.body.json
        guard case .active(let active) = grant.device.device else {
            throw ConvergenceOutage()
        }
        let token = grant.device_token
        let transport = FailableTransport()
        let client = APIClientFactory.live(
            serverURL: apiURL,
            transport: transport,
            token: { token }
        )
        return LiveDevice(
            client: client,
            identity: DeviceIdentity(deviceID: active.id),
            token: token,
            transport: transport
        )
    }

    @MainActor
    static func coordinator(for device: LiveDevice, cache: CacheStore = InMemoryCacheStore()) -> SyncCoordinator {
        SyncCoordinator(client: device.client, device: device.identity, cache: cache)
    }

    /// A unique item ID per test: the suite shares one daemon process,
    /// so isolation comes from identity, never from resetting state.
    static func uniqueItemID(_ label: String) -> String {
        "item-conv-\(label)-\(UUID().uuidString.prefix(8).lowercased())"
    }

    /// Seeds one fresh item under a unique ID and returns that ID; the
    /// one opening move every convergence test shares.
    @MainActor
    static func seedUniqueItem(label: String) async throws -> String {
        let itemID = uniqueItemID(label)
        try await control().seedItem(id: itemID, version: 1)
        return itemID
    }
}
