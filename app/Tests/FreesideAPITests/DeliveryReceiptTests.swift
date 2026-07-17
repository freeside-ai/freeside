import Foundation
import FreesideAPI
import HTTPTypes
import OpenAPIRuntime
import Testing

/// Wraps the mock transport with a fixed bearer credential, standing in
/// for the client middleware where the mock enforces authentication.
private struct AuthorizedTransport: ClientTransport {
    let server: MockServer
    let token: String?

    func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        var request = request
        if let token {
            request.headerFields[.authorization] = "Bearer \(token)"
        }
        return try await MockServerTransport(server: server)
            .send(request, body: body, baseURL: baseURL, operationID: operationID)
    }
}

private func client(server: MockServer, token: String? = nil) -> Client {
    Client(
        serverURL: URL(string: "https://freeside.invalid")!,
        transport: AuthorizedTransport(server: server, token: token)
    )
}

private func submittedDelivery(
    itemID: String, deviceID: String, attempt: Int = 1
) -> Components.Schemas.AttentionDeliverySnapshot {
    .init(
        as_of_revision: 1,
        entity_version: 1,
        delivery: .submitted(
            .init(
                item_id: itemID,
                device_id: deviceID,
                channel: "ntfy",
                attempt: attempt,
                submitted_at: Date(timeIntervalSince1970: 1_752_000_000),
                delivery_status: .submitted
            ))
    )
}

/// #130's client half: the generated client constructs the opened-receipt
/// PUT from the deep link's identity (item, channel, attempt), renders the
/// recorded snapshot, and replay converges on the same snapshot without
/// consuming revision.
@Suite struct DeliveryReceiptTests {
    @Test func reportsOpenedAndReplaysIdempotently() async throws {
        // A real inbox item, so the receipt's same-revision item-timing
        // recompute (the daemon's recomputeItemTiming) is observable.
        let itemID = AttentionFixtures.defaultInboxItemIDs()[0]
        let server = MockServer(
            deliveries: [submittedDelivery(itemID: itemID, deviceID: "device-1")])
        let api = client(server: server)

        // Seeding already derived the item's timing from the rows and
        // bumped the item's versions (the daemon recomputes both in the
        // write that records a delivery), so the pre-receipt state is one
        // the daemon could actually serve.
        let seeded = try await api.getAttentionItem(path: .init(item_id: itemID)).ok.body.json
        #expect(seeded.item.timing.delivery_count == 1)
        #expect(seeded.item.timing.first_submitted_at != nil)
        #expect(seeded.item.timing.first_opened_at == nil)
        #expect(seeded.item.item_version == 2)

        let opened = try await api.reportDeliveryOpened(
            path: .init(item_id: itemID, channel: "ntfy", attempt: 1)
        ).ok.body.json
        guard case .opened(let row) = opened.delivery else {
            Issue.record("expected an opened row, got \(opened.delivery)")
            return
        }
        #expect(row.attempt == 1)

        let item = try await api.getAttentionItem(path: .init(item_id: itemID)).ok.body.json
        #expect(item.item.timing.first_opened_at == row.opened_at)
        #expect(item.item.timing.delivery_count == 1)
        #expect(item.item.item_version == 3)
        #expect(item.as_of_revision == opened.as_of_revision)

        let heartbeat = try await api.getSyncRevision().ok.body.json
        let replay = try await api.reportDeliveryOpened(
            path: .init(item_id: itemID, channel: "ntfy", attempt: 1)
        ).ok.body.json
        #expect(replay == opened)
        let after = try await api.getSyncRevision().ok.body.json
        #expect(after.revision == heartbeat.revision)
        let untouched = try await api.getAttentionItem(path: .init(item_id: itemID)).ok.body.json
        #expect(untouched.item.item_version == 3)

        // The read surface serves the recorded row, and a missing parent
        // item is a loud not-found, never an empty history.
        let listed = try await api.listAttentionItemDeliveries(
            path: .init(item_id: itemID)
        ).ok.body.json
        #expect(listed == [replay])
        let ghost = try await api.listAttentionItemDeliveries(path: .init(item_id: "item-ghost"))
        _ = try ghost.notFound
    }

    @Test func corruptSiblingRowFailsTheReceiptClosed() async throws {
        // The daemon's receipt write re-lists the item's rows through the
        // store gate before recomputing timing, so a corrupt sibling
        // fails the receipt with no effect instead of being folded into
        // a served timing aggregate.
        let itemID = AttentionFixtures.defaultInboxItemIDs()[0]
        let corruptSibling = Components.Schemas.AttentionDeliverySnapshot(
            as_of_revision: 1,
            entity_version: 1,
            delivery: .opened(
                .init(
                    item_id: itemID,
                    device_id: "device-2",
                    channel: "ntfy",
                    attempt: 2,
                    submitted_at: Date(timeIntervalSince1970: 1_752_000_000),
                    opened_at: Date(timeIntervalSince1970: 1_751_999_000),
                    delivery_status: .opened
                ))
        )
        let server = MockServer(
            deliveries: [
                submittedDelivery(itemID: itemID, deviceID: "device-1"),
                corruptSibling,
            ])
        let receipt = try await client(server: server).reportDeliveryOpened(
            path: .init(item_id: itemID, channel: "ntfy", attempt: 1))
        guard case .undocumented(let status, _) = receipt else {
            Issue.record("expected the receipt to fail closed, got \(receipt)")
            return
        }
        #expect(status == 500)
    }

    @Test func corruptRowForAnotherItemFailsBothDeliveryPathsClosed() async throws {
        // The daemon's receipt write (recomputeItemTiming) and its
        // delivery listing both reconstruct the whole attention_deliveries
        // table through ListAttentionDeliveries — which cannot skip a gate
        // the Get runs — before filtering to the requested item. So a
        // corrupt row for a *different* item fails both paths closed rather
        // than the mock serving a 200 the daemon would roll back.
        let itemID = AttentionFixtures.defaultInboxItemIDs()[0]
        let otherItemID = AttentionFixtures.defaultInboxItemIDs()[1]
        let corruptOtherItemRow = Components.Schemas.AttentionDeliverySnapshot(
            as_of_revision: 1,
            entity_version: 1,
            delivery: .opened(
                .init(
                    item_id: otherItemID,
                    device_id: "device-2",
                    channel: "ntfy",
                    attempt: 1,
                    submitted_at: Date(timeIntervalSince1970: 1_752_000_000),
                    opened_at: Date(timeIntervalSince1970: 1_751_999_000),
                    delivery_status: .opened
                ))
        )
        let server = MockServer(
            deliveries: [
                submittedDelivery(itemID: itemID, deviceID: "device-1"),
                corruptOtherItemRow,
            ])
        let receipt = try await client(server: server).reportDeliveryOpened(
            path: .init(item_id: itemID, channel: "ntfy", attempt: 1))
        guard case .undocumented(let receiptStatus, _) = receipt else {
            Issue.record("expected the receipt to fail closed on a corrupt cross-item row, got \(receipt)")
            return
        }
        #expect(receiptStatus == 500)

        let listing = try await client(server: server).listAttentionItemDeliveries(
            path: .init(item_id: itemID))
        guard case .undocumented(let listStatus, _) = listing else {
            Issue.record("expected the listing to fail closed on a corrupt cross-item row, got \(listing)")
            return
        }
        #expect(listStatus == 500)
    }

    @Test func invalidSeededDeliveryFailsTheBootstrapClosed() async throws {
        // The daemon's bootstrap re-validates every row it would serve
        // and fails the whole read on the first bad one; a seeded
        // delivery snapshot the daemon could never produce must not
        // become servable cache state.
        let itemID = AttentionFixtures.defaultInboxItemIDs()[0]
        let invalid = Components.Schemas.AttentionDeliverySnapshot(
            as_of_revision: 0,
            entity_version: 1,
            delivery: .submitted(
                .init(
                    item_id: itemID,
                    device_id: "device-1",
                    channel: "ntfy",
                    attempt: 1,
                    submitted_at: Date(timeIntervalSince1970: 1_752_000_000),
                    delivery_status: .submitted
                ))
        )
        let server = MockServer(deliveries: [invalid])
        let api = client(server: server)

        // The invalid row never folds into the parent item's timing
        // either: the daemon's PutAttentionDelivery would have rejected
        // it before recomputeItemTiming ran, so item reads keep serving
        // the fixture aggregates at the fixture version.
        let parent = try await api.getAttentionItem(path: .init(item_id: itemID)).ok.body.json
        #expect(parent.item.timing.delivery_count == 0)
        #expect(parent.item.item_version == 1)

        // The receipt fails closed before any effect (the daemon
        // reconstructs the row ahead of the write), so the corrupt seed
        // is never healed into a servable 200.
        let receipt = try await api.reportDeliveryOpened(
            path: .init(item_id: itemID, channel: "ntfy", attempt: 1))
        guard case .undocumented(let receiptStatus, _) = receipt else {
            Issue.record("expected the receipt to fail closed, got \(receipt)")
            return
        }
        #expect(receiptStatus == 500)

        let bootstrap = try await api.getSyncBootstrap()
        guard case .undocumented(let status, _) = bootstrap else {
            Issue.record("expected the bootstrap to fail closed, got \(bootstrap)")
            return
        }
        #expect(status == 500)
    }

    @Test func unknownAttemptIsNotFoundAndZeroIsRejected() async throws {
        let itemID = AttentionFixtures.defaultInboxItemIDs()[0]
        let server = MockServer(
            deliveries: [submittedDelivery(itemID: itemID, deviceID: "device-1")])
        let api = client(server: server)

        let missing = try await api.reportDeliveryOpened(
            path: .init(item_id: itemID, channel: "ntfy", attempt: 9))
        _ = try missing.notFound

        let rejected = try await api.reportDeliveryOpened(
            path: .init(item_id: itemID, channel: "ntfy", attempt: 0))
        _ = try rejected.badRequest
    }

    @Test func deviceOpensOnlyItsOwnAttempts() async throws {
        // Enforcing mode: the receipt's device is the credential identity
        // (never a path or payload field), so another device's attempt is
        // indistinguishable from absent.
        let itemID = AttentionFixtures.defaultInboxItemIDs()[0]
        let server = MockServer(
            deliveries: [
                submittedDelivery(itemID: itemID, deviceID: "device-9"),
                submittedDelivery(itemID: itemID, deviceID: "device-1", attempt: 2),
            ],
            authMode: .enforcing
        )
        await server.seedPairingCode("483911")
        let grant = try await client(server: server).pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Receipt device"))
        ).created.body.json
        let api = client(server: server, token: grant.device_token)

        // Two seeded rows replay as two daemon writes: two version bumps.
        let seeded = try await api.getAttentionItem(path: .init(item_id: itemID)).ok.body.json
        #expect(seeded.item.item_version == 3)

        let foreign = try await api.reportDeliveryOpened(
            path: .init(item_id: itemID, channel: "ntfy", attempt: 1))
        _ = try foreign.notFound

        let own = try await api.reportDeliveryOpened(
            path: .init(item_id: itemID, channel: "ntfy", attempt: 2)
        ).ok.body.json
        guard case .opened = own.delivery else {
            Issue.record("expected the credential device's attempt to open, got \(own.delivery)")
            return
        }
    }

    @Test func subsecondReceiptTimestampsDeriveAConsistentSummary() async throws {
        // The wire encoder emits whole seconds and the derivation uses those
        // same encoded instants, so the generated client never receives a
        // duration that disagrees with its decoded endpoints.
        let itemID = AttentionFixtures.defaultInboxItemIDs()[0]
        let opened = Components.Schemas.AttentionDeliverySnapshot(
            as_of_revision: 1,
            entity_version: 1,
            delivery: .opened(
                .init(
                    item_id: itemID,
                    device_id: "device-1",
                    channel: "ntfy",
                    attempt: 1,
                    submitted_at: Date(timeIntervalSince1970: 1_752_000_000),
                    // Above the half-second mark: JSONEncoder's `.iso8601`
                    // strategy truncates rather than rounds, which the timing
                    // derivation must mirror.
                    opened_at: Date(timeIntervalSince1970: 1_752_000_060.79),
                    delivery_status: .opened
                ))
        )
        let server = MockServer(deliveries: [opened])
        let item = try await client(server: server).getAttentionItem(
            path: .init(item_id: itemID)
        ).ok.body.json
        guard let submitted = item.item.timing.first_submitted_at,
            let openedAt = item.item.timing.first_opened_at,
            let span = item.item.timing.submit_to_first_open
        else {
            Issue.record("expected complete submit-to-open timing")
            return
        }
        let listed = try await client(server: server).listAttentionItemDeliveries(
            path: .init(item_id: itemID)
        ).ok.body.json
        guard case .opened(let wireDelivery) = listed.first?.delivery else {
            Issue.record("expected the seeded opened delivery")
            return
        }
        #expect(submitted == wireDelivery.submitted_at)
        #expect(openedAt == wireDelivery.opened_at)
        #expect(
            span == Int64(
                (openedAt.timeIntervalSince(submitted) * 1_000_000_000).rounded()))
    }

    @Test func longReceiptSpanSaturatesLikeGoDuration() async throws {
        // Go's time.Time.Sub saturates when a valid timestamp span exceeds
        // time.Duration's int64 nanosecond range; the mock must not trap while
        // deriving the same response.
        let itemID = AttentionFixtures.defaultInboxItemIDs()[0]
        let opened = Components.Schemas.AttentionDeliverySnapshot(
            as_of_revision: 1,
            entity_version: 1,
            delivery: .opened(
                .init(
                    item_id: itemID,
                    device_id: "device-1",
                    channel: "ntfy",
                    attempt: 1,
                    submitted_at: Date(timeIntervalSince1970: -59_000_000_000),
                    opened_at: Date(timeIntervalSince1970: 253_402_300_799),
                    delivery_status: .opened
                ))
        )
        let server = MockServer(deliveries: [opened])
        let item = try await client(server: server).getAttentionItem(
            path: .init(item_id: itemID)
        ).ok.body.json
        #expect(item.item.timing.submit_to_first_open == Int64.max)
    }

    @Test func corruptParentItemFailsTheDeliverySurfaceClosed() async throws {
        // The daemon reads the parent item through the reconstruction
        // gate in both delivery paths (the listing validates the item
        // snapshot; the receipt's recompute reads it via
        // GetAttentionItemSnapshot), so a corrupt item fails both closed.
        // The corruption is a stale publish_eligible bit: a field the
        // seed-time timing derivation never touches, so it cannot be
        // healed on the way in.
        var corrupt = AttentionFixtures.defaultInbox()[0]
        corrupt.item.evidence_snapshot[0].publish_eligible = false
        let itemID = corrupt.item.id
        let server = MockServer(
            items: [corrupt],
            deliveries: [submittedDelivery(itemID: itemID, deviceID: "device-1")])
        let api = client(server: server)

        let list = try await api.listAttentionItemDeliveries(path: .init(item_id: itemID))
        guard case .undocumented(let listStatus, _) = list else {
            Issue.record("expected the listing to fail closed, got \(list)")
            return
        }
        #expect(listStatus == 500)

        let receipt = try await api.reportDeliveryOpened(
            path: .init(item_id: itemID, channel: "ntfy", attempt: 1))
        guard case .undocumented(let receiptStatus, _) = receipt else {
            Issue.record("expected the receipt to fail closed, got \(receipt)")
            return
        }
        #expect(receiptStatus == 500)
    }

    @Test func zeroSubmittedAtSeedFailsClosed() async throws {
        // AttentionDelivery.Validate rejects SubmittedAt.IsZero(), so a
        // seeded row with the daemon zero instant (an unset submitted_at)
        // is unproducible state: it must not fold into item timing nor be
        // served, matching the daemon's store gate.
        let itemID = AttentionFixtures.defaultInboxItemIDs()[0]
        let zeroSubmittedAt = Components.Schemas.AttentionDeliverySnapshot(
            as_of_revision: 1,
            entity_version: 1,
            delivery: .submitted(
                .init(
                    item_id: itemID,
                    device_id: "device-1",
                    channel: "ntfy",
                    attempt: 1,
                    // Go's exact 0001-01-01T00:00:00Z zero instant, the value
                    // AttentionDelivery.Validate rejects as unset.
                    submitted_at: {
                        var components = DateComponents()
                        components.year = 1
                        components.month = 1
                        components.day = 1
                        var calendar = Calendar(identifier: .gregorian)
                        calendar.timeZone = TimeZone(identifier: "UTC")!
                        return calendar.date(from: components)!
                    }(),
                    delivery_status: .submitted
                ))
        )
        let server = MockServer(deliveries: [zeroSubmittedAt])
        let api = client(server: server)

        // Never folded into the parent item's timing on the way in.
        let parent = try await api.getAttentionItem(path: .init(item_id: itemID)).ok.body.json
        #expect(parent.item.timing.delivery_count == 0)

        let bootstrap = try await api.getSyncBootstrap()
        guard case .undocumented(let status, _) = bootstrap else {
            Issue.record("expected the bootstrap to fail closed on a zero submitted_at, got \(bootstrap)")
            return
        }
        #expect(status == 500)
    }

    @Test func invalidParentIsNotHealedBySeedTiming() async throws {
        // Seed-time timing derivation must not rewrite an invalid parent
        // into a servable snapshot: the daemon's recompute reconstructs the
        // item via GetAttentionItemSnapshot and fails closed. entity_version
        // == 0 is exactly the metadata withDerivedTiming's version bump
        // would otherwise heal, so the parent must stay invalid and every
        // item read fail closed.
        var invalidParent = AttentionFixtures.defaultInbox()[0]
        invalidParent.entity_version = 0
        let itemID = invalidParent.item.id
        let server = MockServer(
            items: [invalidParent],
            deliveries: [submittedDelivery(itemID: itemID, deviceID: "device-1")])
        let api = client(server: server)

        let item = try await api.getAttentionItem(path: .init(item_id: itemID))
        guard case .undocumented(let itemStatus, _) = item else {
            Issue.record("expected getAttentionItem to fail closed on an invalid parent, got \(item)")
            return
        }
        #expect(itemStatus == 500)

        let bootstrap = try await api.getSyncBootstrap()
        guard case .undocumented(let bootstrapStatus, _) = bootstrap else {
            Issue.record("expected the bootstrap to fail closed on an invalid parent, got \(bootstrap)")
            return
        }
        #expect(bootstrapStatus == 500)
    }
}
