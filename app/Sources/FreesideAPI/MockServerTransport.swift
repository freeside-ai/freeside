import Foundation
import HTTPTypes
import OpenAPIRuntime

/// Routes generated-client requests to a MockServer over real JSON, so
/// every call exercises the full generated encode/decode pipeline.
public struct MockServerTransport: ClientTransport {
    public let server: MockServer

    public init(server: MockServer) {
        self.server = server
    }

    public func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        try await server.runBeforeRespond(operationID)
        let response = try await route(request, body: body, operationID: operationID)
        try await server.runAfterRespond(operationID)
        return response
    }

    private func route(
        _ request: HTTPRequest,
        body: HTTPBody?,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        // The daemon authorizes before any handler runs and fails closed
        // (#105); pairing is the one unauthenticated operation.
        var authenticatedDevice: String?
        if operationID != "pairDevice" {
            switch await server.authenticate(
                authorization: request.headerFields[.authorization])
            {
            case .anonymous:
                break
            case .device(let id):
                authenticatedDevice = id
            case .revokedDevice(let id):
                // A revoked credential authenticates nothing except test
                // 16's recorded-replay branch on command submission.
                if operationID == "submitCommand", let body {
                    let data = try await Data(collecting: body, upTo: 1 << 20)
                    if let command = try? Self.decoder.decode(
                        Components.Schemas.ClientCommand.self, from: data),
                        let recorded = await server.recordedResultForRevokedRetry(
                            command, deviceID: id)
                    {
                        return try Self.json(status: .ok, body: recorded)
                    }
                }
                return try Self.json(
                    status: .unauthorized,
                    body: Components.Schemas._Error(message: "unauthorized"))
            case .unauthorized:
                return try Self.json(
                    status: .unauthorized,
                    body: Components.Schemas._Error(message: "unauthorized"))
            }
        }
        switch operationID {
        case "getSyncRevision":
            return try Self.json(status: .ok, body: await server.serverRevision())
        case "getSyncBootstrap":
            do {
                return try Self.json(status: .ok, body: await server.bootstrapSnapshot())
            } catch let invalid as MockServer.InvalidItemError {
                // One invalid row fails the whole bootstrap closed, as the
                // daemon's single-read upper-bound gate does (#105).
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message:
                            "bootstrap reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            } catch let invalid as MockServer.InvalidDeliveryError {
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message:
                            "bootstrap reconstruction failed: delivery for item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            }
        case "listAttentionItems":
            do {
                return try Self.json(status: .ok, body: await server.listAttentionItems())
            } catch let invalid as MockServer.InvalidItemError {
                // The daemon fails the whole read on the first invalid
                // row; a client sees a failed refresh, never a partial
                // inbox.
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message: "list reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            }
        case "getAttentionItem":
            do {
                guard
                    let itemID = Self.lastPathComponent(request.path),
                    let snapshot = try await server.servedSnapshot(itemID: itemID)
                else {
                    return try Self.json(
                        status: .notFound,
                        body: Components.Schemas._Error(
                            message: "no entity exists under the identifier")
                    )
                }
                return try Self.json(status: .ok, body: snapshot)
            } catch let invalid as MockServer.InvalidItemError {
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message: "reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            }
        case "pairDevice":
            guard let body else {
                return (HTTPResponse(status: .badRequest), nil)
            }
            let data = try await Data(collecting: body, upTo: 1 << 20)
            let pairing = try Self.decoder.decode(
                Components.Schemas.PairingRequest.self, from: data)
            do {
                return try Self.json(status: .created, body: try await server.pairDevice(pairing))
            } catch is MockServer.PairingRejectedError {
                return try Self.json(
                    status: .forbidden,
                    body: Components.Schemas._Error(
                        message: "the pairing code is unknown, expired, or already consumed")
                )
            }
        case "revokeDevice":
            guard let deviceID = Self.deviceID(inRevokePath: request.path) else {
                return (HTTPResponse(status: .badRequest), nil)
            }
            switch await server.revokeDevice(id: deviceID) {
            case .revoked(let snapshot):
                return try Self.json(status: .ok, body: snapshot)
            case .unknown:
                return try Self.json(
                    status: .notFound,
                    body: Components.Schemas._Error(
                        message: "no entity exists under the identifier")
                )
            }
        case "submitCommand":
            guard let body else {
                return (HTTPResponse(status: .badRequest), nil)
            }
            let data = try await Data(collecting: body, upTo: 1 << 20)
            let command = try Self.decoder.decode(
                Components.Schemas.ClientCommand.self, from: data)
            // One valid device credential can never name another device
            // in a command body (#105), ahead of the contract semantics.
            if let authenticatedDevice, command.device_id != authenticatedDevice {
                return try Self.json(
                    status: .forbidden,
                    body: Components.Schemas._Error(
                        message: "device_id does not match the authenticated device")
                )
            }
            do {
                switch try await server.submitCommand(command) {
                case .ok(let result):
                    return try Self.json(status: .ok, body: result)
                case .stale(let rejection):
                    return try Self.json(status: .conflict, body: rejection)
                }
            } catch let missing as MockServer.UnknownItemError {
                // Daemon rejections are authoritative HTTP responses, not
                // transport failures: the generated client surfaces the
                // undocumented ones as their status, distinguishable from
                // a lost response.
                return try Self.json(
                    status: .notFound,
                    body: Components.Schemas._Error(
                        message: "no item exists under \(missing.itemID)")
                )
            } catch let rejection as MockServer.ImmutableConflictError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message: "command \(rejection.commandID) reused with a different body")
                )
            } catch let rejection as MockServer.ActionNotOfferedError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message:
                            "action \(rejection.action.rawValue) not offered by item \(rejection.itemID)"
                    )
                )
            } catch let rejection as MockServer.UnsupportedActionError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message:
                            "action \(rejection.action.rawValue) is not acceptable yet; its transaction belongs to a later unit"
                    )
                )
            } catch let rejection as MockServer.MalformedCommandError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message: "malformed command: \(rejection.reason)")
                )
            } catch let rejection as MockServer.ItemPolicyError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message: "item \(rejection.itemID) fails signet policy: \(rejection.reason)"
                    )
                )
            } catch let rejection as MockServer.InvalidItemError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message: "item \(rejection.itemID) fails validation: \(rejection.reason)"
                    )
                )
            }
        case "listAttentionItemDeliveries":
            guard let itemID = Self.itemID(inDeliveriesPath: request.path) else {
                return (HTTPResponse(status: .badRequest), nil)
            }
            do {
                return try Self.json(
                    status: .ok,
                    body: try await server.listAttentionItemDeliveries(itemID: itemID))
            } catch let missing as MockServer.UnknownItemError {
                return try Self.json(
                    status: .notFound,
                    body: Components.Schemas._Error(
                        message: "no entity exists under \(missing.itemID)")
                )
            } catch let invalid as MockServer.InvalidDeliveryError {
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message:
                            "list reconstruction failed: delivery for item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            } catch let invalid as MockServer.InvalidItemError {
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message: "list reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            }
        case "reportDeliveryOpened":
            guard let identity = Self.deliveryIdentity(inOpenedPath: request.path) else {
                return try Self.json(
                    status: .badRequest,
                    body: Components.Schemas._Error(
                        message: "attempt must be a positive integer")
                )
            }
            do {
                switch try await server.reportDeliveryOpened(
                    itemID: identity.itemID, channel: identity.channel,
                    attempt: identity.attempt, deviceID: authenticatedDevice)
                {
                case .ok(let snapshot):
                    return try Self.json(status: .ok, body: snapshot)
                case .unknown:
                    return try Self.json(
                        status: .notFound,
                        body: Components.Schemas._Error(
                            message: "no entity exists under the identifier")
                    )
                }
            } catch let invalid as MockServer.InvalidDeliveryError {
                // The daemon's wire method re-validates the snapshot it
                // returns and fails closed (writeReadError → 500).
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message:
                            "delivery reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            } catch let invalid as MockServer.InvalidItemError {
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message:
                            "delivery reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            }
        case "getAttachment":
            // The digest-addressed read path: stored bytes verbatim, or
            // an authoritative 404 the client renders as a placeholder.
            // Bytes are opaque to the server (plan §5.15: nothing
            // server-side decodes an image; rendering is the client's).
            guard let digest = Self.lastPathComponent(request.path),
                let bytes = await server.attachmentBytes(digest: digest)
            else {
                return try Self.json(
                    status: .notFound,
                    body: Components.Schemas._Error(
                        message: "no attachment exists under the digest")
                )
            }
            let response = HTTPResponse(
                status: .ok,
                headerFields: [.contentType: "application/octet-stream"]
            )
            return (response, HTTPBody(bytes))
        default:
            return (HTTPResponse(status: .notImplemented), nil)
        }
    }

    private static let encoder: JSONEncoder = {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        return encoder
    }()

    private static let decoder: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return decoder
    }()

    private static func json(
        status: HTTPResponse.Status,
        body: some Encodable
    ) throws -> (HTTPResponse, HTTPBody?) {
        let response = HTTPResponse(
            status: status,
            headerFields: [.contentType: "application/json"]
        )
        return (response, HTTPBody(try encoder.encode(body)))
    }

    private static func lastPathComponent(_ path: String?) -> String? {
        path?.split(separator: "/").last.flatMap { String($0).removingPercentEncoding }
    }

    /// `/devices/{device_id}/revoke`: the id is the segment ahead of the
    /// trailing verb.
    private static func deviceID(inRevokePath path: String?) -> String? {
        let parts = path?.split(separator: "/") ?? []
        guard parts.count >= 2, parts.last == "revoke" else { return nil }
        return String(parts[parts.count - 2]).removingPercentEncoding
    }

    /// `/attention/items/{item_id}/deliveries`: the id is the segment
    /// ahead of the trailing collection name.
    private static func itemID(inDeliveriesPath path: String?) -> String? {
        let parts = (path?.split(separator: "/") ?? [])
            .map { String($0).removingPercentEncoding ?? String($0) }
        guard parts.count == 4, parts[0] == "attention", parts[1] == "items",
            parts[3] == "deliveries"
        else { return nil }
        return parts[2]
    }

    /// `/attention/items/{item_id}/deliveries/{channel}/{attempt}/opened`:
    /// nil when the shape is wrong or the attempt segment is not a
    /// positive integer (the daemon answers 400 before its service runs).
    private static func deliveryIdentity(
        inOpenedPath path: String?
    ) -> (itemID: String, channel: String, attempt: Int)? {
        let parts = (path?.split(separator: "/") ?? [])
            .map { String($0).removingPercentEncoding ?? String($0) }
        guard parts.count == 7, parts[0] == "attention", parts[1] == "items",
            parts[3] == "deliveries", parts[6] == "opened",
            let attempt = Int(parts[5]), attempt >= 1
        else { return nil }
        return (parts[2], parts[4], attempt)
    }
}
