import FreesideAPI
import FreesideCore
import Foundation
import HTTPTypes
import OpenAPIRuntime

/// A reusable one-shot gate: `wait()` suspends until `open()`.
actor AsyncGate {
    private var isOpen = false
    private var waiters: [CheckedContinuation<Void, Never>] = []

    func wait() async {
        if isOpen { return }
        await withCheckedContinuation { waiters.append($0) }
    }

    func open() {
        isOpen = true
        for waiter in waiters {
            waiter.resume()
        }
        waiters.removeAll()
    }
}

struct InjectedFailure: Error {}

func testDeviceToken(for deviceID: String, secretByte: UInt8 = 1) -> String {
    func base64URL(_ data: Data) -> String {
        data.base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }
    let encodedID = base64URL(Data(deviceID.utf8))
    let encodedSecret = base64URL(Data(repeating: secretByte, count: 32))
    return "fsd1.\(encodedID).\(encodedSecret)"
}

/// Throws on the first `times` calls to `consume()`; later calls pass.
actor InjectedFailures {
    private var remaining: Int

    init(times: Int) {
        remaining = times
    }

    func consume() throws {
        guard remaining > 0 else { return }
        remaining -= 1
        throw InjectedFailure()
    }
}

@MainActor
func makeStore(server: MockServer) async -> InboxStore {
    let store = InboxStore(client: APIClientFactory.mock(server: server))
    await store.refresh()
    return store
}

/// A well-formed command fixture for ledger persistence tests: the mock
/// accepts its shape, and one naming an unknown item draws the
/// authoritative rejection, never a validation error. The device
/// defaults to the store default (`DeviceIdentity.mock`) so the entry
/// survives the restore-time device re-gate.
func makeCommand(
    itemID: String, commandID: String = "cmd-fixture",
    deviceID: String = DeviceIdentity.mock.deviceID
) -> Components.Schemas.ClientCommand {
    .init(
        command_id: commandID,
        device_id: deviceID,
        expected_entity_version: 1,
        expected_bindings: .init(additionalProperties: [:]),
        payload: .init(
            item_id: itemID,
            action: .approve,
            item_version: 1,
            pr_head_sha: "",
            artifact_digests: []
        )
    )
}

/// Answers the first matching operation with a bare HTTP status instead
/// of reaching the mock server (simulating a transient gateway/server
/// failure before commit); everything else passes through.
struct StatusOverrideTransport: ClientTransport {
    let base: MockServerTransport
    let operationID: String
    let status: Int
    let once: OneShot

    func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        if operationID == self.operationID, await once.fire() {
            return (HTTPResponse(status: .init(code: status)), nil)
        }
        return try await base.send(request, body: body, baseURL: baseURL, operationID: operationID)
    }
}

/// Counts how many times an operation ran, for asserting re-fetches.
actor Counter {
    private(set) var count = 0
    func increment() { count += 1 }
    func incrementAndGet() -> Int {
        count += 1
        return count
    }
}

/// True exactly once.
actor OneShot {
    private var fired = false

    func fire() -> Bool {
        if fired { return false }
        fired = true
        return true
    }
}

/// Scripts one behavior per matching call, in order; exhausted → pass.
actor ScriptedResponses {
    enum Step {
        case pass
        case fail
        /// Opens `reached`, then suspends until `release` opens.
        case hold(reached: AsyncGate, release: AsyncGate)
    }

    private var steps: [Step]

    init(_ steps: [Step]) {
        self.steps = steps
    }

    func next() async throws {
        guard !steps.isEmpty else { return }
        switch steps.removeFirst() {
        case .pass:
            return
        case .fail:
            throw InjectedFailure()
        case .hold(let reached, let release):
            await reached.open()
            await release.wait()
        }
    }
}
