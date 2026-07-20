import CryptoKit
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

    /// Issues a new sync epoch on its own, without a data restore: the
    /// minimal §5.14 test-8 stimulus. The real restore (data rollback
    /// plus rotation) is `restore(checkpoint:)`.
    func rotateEpoch() async throws {
        _ = try await post("control/epoch")
    }

    /// Snapshots the daemon's store to a fresh checkpoint file and
    /// returns its path, so a later `restore` can roll the daemon back
    /// to exactly this state.
    func checkpoint() async throws -> String {
        let data = try await post("control/checkpoint")
        let payload = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        guard let path = payload?["checkpoint"] as? String, !path.isEmpty else {
            throw ConvergenceOutage()
        }
        return path
    }

    /// Restores the daemon to a named checkpoint, rolling the data back
    /// and rotating the sync epoch in one operation (the real §5.14
    /// restore, test 8's data half).
    func restore(checkpoint: String) async throws {
        _ = try await post("control/restore", body: ["checkpoint": checkpoint])
    }

    /// Seeds or advances one attention item; the harness constructs the
    /// item body server-side, so version bumps here are the real
    /// analogue of the mock's advance hook. A non-nil `textClaim` attaches
    /// one markdown text claim carrying that content, digest-bound by the
    /// daemon (#217).
    func seedItem(id: String, version: Int, textClaim: String? = nil) async throws {
        var body: [String: Any] = ["id": id, "item_version": version]
        if let textClaim { body["text_claim"] = textClaim }
        _ = try await post("control/items", body: body)
    }

    /// Seeds one item at an explicit type and offered action set and returns the
    /// daemon's verdict (status + `message`) instead of throwing on a 4xx, so a
    /// policy test can assert the accept/reject outcome. `type` / `actions` are
    /// raw wire strings sent only when non-nil: raw strings reach the real
    /// per-type policy for the invalid/unknown cases the typed enums cannot
    /// represent, and a nil field lets the daemon apply its default. An explicit
    /// empty `actions` (`[]`) is distinct from nil — it drives the blocked accept
    /// and the non-blocked no-actions rejection.
    func seedItemOutcome(
        id: String, version: Int = 1, type: String?, actions: [String]?
    ) async throws -> SeedOutcome {
        var body: [String: Any] = ["id": id, "item_version": version]
        if let type { body["type"] = type }
        if let actions { body["requested_decision"] = actions }
        var request = URLRequest(url: baseURL.appendingPathComponent("control/items"))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(withJSONObject: body)
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw ConvergenceOutage() }
        let message =
            (try? JSONSerialization.jsonObject(with: data) as? [String: Any])?["message"] as? String
        return SeedOutcome(statusCode: http.statusCode, message: message)
    }

    /// Typed convenience for the valid enumeration: maps the generated enums to
    /// their wire strings so a caller can drive `phase1Types` / `phase1Actions`
    /// directly.
    func seedItemOutcome(
        id: String, type: Components.Schemas.AttentionType, actions: [Components.Schemas.Action]
    ) async throws -> SeedOutcome {
        try await seedItemOutcome(id: id, type: type.rawValue, actions: actions.map(\.rawValue))
    }
}

/// The daemon's verdict on a `control/items` seed: the HTTP status and the JSON
/// `message` body (nil when absent). A 4xx is a value here, not a thrown error,
/// so the policy-parity suite can assert the daemon's accept/reject decision.
struct SeedOutcome {
    let statusCode: Int
    let message: String?
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

    /// Uploads bytes through the real uploadAttachment route and returns
    /// the content digest they were stored under, so a test can then read
    /// them back through getAttachment. The daemon verifies the body hashes
    /// to the path digest, so the digest is computed from the bytes here,
    /// never guessed. No control route is involved: the contract PUT is the
    /// seeding surface for a stored blob.
    @MainActor
    static func uploadAttachment(_ bytes: Data, on device: LiveDevice) async throws -> String {
        let digest =
            "sha256:"
            + SHA256.hash(data: bytes)
            .map { String(format: "%02x", $0) }.joined()
        let response = try await device.client.uploadAttachment(
            path: .init(digest: digest),
            body: .binary(HTTPBody(bytes))
        )
        switch response {
        case .created, .ok:
            return digest
        default:
            throw ConvergenceOutage()
        }
    }
}
