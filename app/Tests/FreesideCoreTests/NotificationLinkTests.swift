import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// The tap link's form is shared with the daemon, which writes it
/// (`notificationFor` in `daemon/internal/signet/ntfy.go`). Any app on the
/// phone can open a `freeside://` link, so everything else parses to nothing.
@Suite struct NotificationLinkTests {
    private func link(_ text: String) throws -> NotificationLink? {
        NotificationLink(try #require(URL(string: text)))
    }

    @Test func parsesAnItemLinkAsTheDaemonWritesIt() throws {
        #expect(
            try link("freeside://attention/items/item-42?channel=ntfy&attempt=3")
                == .attentionItem(itemID: "item-42", channel: "ntfy", attempt: 3))
        // The daemon path-escapes the item ID.
        #expect(
            try link("freeside://attention/items/a%2Fb%20c?channel=ntfy&attempt=1")
                == .attentionItem(itemID: "a/b c", channel: "ntfy", attempt: 1))
        // Query order is not part of the form.
        #expect(
            try link("freeside://attention/items/item-42?attempt=2&channel=ntfy")
                == .attentionItem(itemID: "item-42", channel: "ntfy", attempt: 2))
    }

    @Test func parsesTheInboxLink() throws {
        #expect(try link("freeside://inbox") == .inbox)
        #expect(try link("FREESIDE://inbox") == .inbox)
    }

    @Test(arguments: [
        // Attempt: missing, empty, zero, negative, signed, non-numeric, beyond Int.
        "freeside://attention/items/item-42?channel=ntfy",
        "freeside://attention/items/item-42?channel=ntfy&attempt=",
        "freeside://attention/items/item-42?channel=ntfy&attempt=0",
        "freeside://attention/items/item-42?channel=ntfy&attempt=-1",
        "freeside://attention/items/item-42?channel=ntfy&attempt=+1",
        "freeside://attention/items/item-42?channel=ntfy&attempt=one",
        "freeside://attention/items/item-42?channel=ntfy&attempt=1.0",
        "freeside://attention/items/item-42?channel=ntfy&attempt=99999999999999999999",
        "freeside://attention/items/item-42?channel=ntfy&attempt=01",
        // Channel: missing, empty, or a dot segment.
        "freeside://attention/items/item-42?attempt=1",
        "freeside://attention/items/item-42?channel=&attempt=1",
        "freeside://attention/items/item-42?channel=..&attempt=1",
        "freeside://attention/items/item-42?channel=.&attempt=1",
        // Repeated or unknown query names.
        "freeside://attention/items/item-42?channel=ntfy&attempt=1&attempt=2",
        "freeside://attention/items/item-42?channel=ntfy&attempt=1&action=approve",
        // Item ID: empty, a dot segment, a control character, or more path
        // than the form has.
        "freeside://attention/items/?channel=ntfy&attempt=1",
        "freeside://attention/items/..?channel=ntfy&attempt=1",
        "freeside://attention/items/%2E%2E?channel=ntfy&attempt=1",
        "freeside://attention/items/.?channel=ntfy&attempt=1",
        "freeside://attention/items/a%00b?channel=ntfy&attempt=1",
        "freeside://attention/items?channel=ntfy&attempt=1",
        "freeside://attention/items/item-42/approve?channel=ntfy&attempt=1",
        "freeside://attention/item-42?channel=ntfy&attempt=1",
        "freeside://attention/tasks/item-42?channel=ntfy&attempt=1",
        // The inbox link takes nothing else.
        "freeside://inbox/item-42",
        "freeside://inbox?attempt=1",
        "freeside://inbox/",
        // Other hosts, authority parts, and schemes.
        "freeside://tasks",
        "freeside://",
        "freeside:inbox",
        "freeside://user@inbox",
        "freeside://inbox:80",
        "freeside://inbox:",
        // A host the daemon does not write, though it decodes to one it does.
        "freeside://%69nbox",
        "freeside://attentio%6E/items/item-42?channel=ntfy&attempt=1",
        "freeside://ATTENTION/items/item-42?channel=ntfy&attempt=1",
        "freeside://inbox#fragment",
        "http://attention/items/item-42?channel=ntfy&attempt=1",
        "https://inbox",
    ])
    func refusesEveryOtherURL(_ text: String) throws {
        #expect(try link(text) == nil)
    }
}

/// What a tapped link does to a session: it marks this device's delivery
/// attempt opened, then shows the item, and does nothing else.
@Suite @MainActor struct NotificationLinkOpeningTests {
    private static let itemID = "item-spec_approval"

    private func server() -> MockServer {
        MockServer(deliveries: [
            .init(
                as_of_revision: 1, entity_version: 1,
                delivery: .submitted(
                    .init(
                        item_id: Self.itemID, device_id: "device-7", channel: "ntfy", attempt: 1,
                        submitted_at: Date(timeIntervalSince1970: 1_752_000_000),
                        delivery_status: .submitted)))
        ])
    }

    private func session(_ server: MockServer, paired: Bool = true) throws -> AppSession {
        let credential = try #require(
            DeviceCredential(
                deviceID: "device-7", token: testDeviceToken(for: "device-7"), ntfySubscription: .mock))
        return AppSession(
            client: APIClientFactory.mock(server: server),
            credentials: InMemoryCredentialStore(credential: paired ? credential : nil),
            cache: InMemoryCacheStore())
    }

    private func openedAttempts(_ server: MockServer) async throws -> [Int] {
        try await APIClientFactory.mock(server: server)
            .listAttentionItemDeliveries(path: .init(item_id: Self.itemID)).ok.body.json
            .compactMap {
                if case .opened(let row) = $0.delivery { row.attempt } else { nil }
            }
    }

    @Test func anItemLinkReportsItsReceiptBeforeItShowsTheCard() async throws {
        let server = server()
        let session = try session(server)
        let navigation = NavigationModel(launchInputs: .standard())
        // Hold the receipt's answer: a card shown now would load the item
        // at the version the receipt is about to raise.
        let (arrived, release) = (AsyncGate(), AsyncGate())
        await server.setBeforeRespond { operationID in
            guard operationID == "reportDeliveryOpened" else { return }
            await arrived.open()
            await release.wait()
        }

        let opening = Task {
            await session.open(
                .attentionItem(itemID: Self.itemID, channel: "ntfy", attempt: 1), in: navigation,
                receiptBound: .seconds(60))
        }
        await arrived.wait()
        #expect(navigation.inboxPath.isEmpty)

        await release.open()
        await opening.value
        #expect(navigation.selectedTab == .inbox)
        #expect(navigation.inboxPath == [Self.itemID])
        #expect(navigation.linkedItemID == Self.itemID)
        #expect(try await openedAttempts(server) == [1])
    }

    @Test func aReceiptThatOutlastsItsBoundDoesNotHoldTheCard() async throws {
        let server = server()
        let session = try session(server)
        let navigation = NavigationModel(launchInputs: .standard())
        let never = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "reportDeliveryOpened" { await never.wait() }
        }

        await session.open(
            .attentionItem(itemID: Self.itemID, channel: "ntfy", attempt: 1), in: navigation,
            receiptBound: .milliseconds(20))

        #expect(navigation.inboxPath == [Self.itemID])
        await never.open()
    }

    /// Opens the link for `itemID` and returns once its receipt is held at
    /// the server, with later receipts free to answer. `release` lets the
    /// held one go.
    private func openWithHeldReceipt(
        _ session: AppSession, _ server: MockServer, in navigation: NavigationModel
    ) async -> (opening: Task<Void, Never>, release: AsyncGate) {
        let (arrived, release) = (AsyncGate(), AsyncGate())
        await server.setBeforeRespond { operationID in
            guard operationID == "reportDeliveryOpened" else { return }
            await arrived.open()
            await release.wait()
        }
        let opening = Task {
            await session.open(
                .attentionItem(itemID: Self.itemID, channel: "ntfy", attempt: 1), in: navigation,
                receiptBound: .seconds(60))
        }
        await arrived.wait()
        await server.setBeforeRespond(nil)
        return (opening, release)
    }

    @Test func aNewerLinkWinsOverAnOlderOneStillWaiting() async throws {
        let server = server()
        let session = try session(server)
        let navigation = NavigationModel(launchInputs: .standard())
        let (older, release) = await openWithHeldReceipt(session, server, in: navigation)

        // The newer link's receipt is refused at once, so its card shows
        // while the older link is still waiting.
        await session.open(
            .attentionItem(itemID: "item-newer", channel: "ntfy", attempt: 1), in: navigation)
        #expect(navigation.inboxPath == ["item-newer"])

        await release.open()
        await older.value
        #expect(navigation.inboxPath == ["item-newer"])
    }

    @Test func theInboxLinkWinsOverAnOlderItemLinkStillWaiting() async throws {
        let server = server()
        let session = try session(server)
        let navigation = NavigationModel(launchInputs: .standard())
        let (older, release) = await openWithHeldReceipt(session, server, in: navigation)

        // Already on the inbox, so the tab does not change.
        await session.open(.inbox, in: navigation)

        await release.open()
        await older.value
        #expect(navigation.inboxPath.isEmpty)
    }

    @Test func operatorNavigationDuringTheWaitWinsOverTheLink() async throws {
        let server = server()
        let session = try session(server)
        let navigation = NavigationModel(launchInputs: .standard())
        let (opening, release) = await openWithHeldReceipt(session, server, in: navigation)

        navigation.selectTab(.tasks)

        await release.open()
        await opening.value
        #expect(navigation.selectedTab == .tasks)
        #expect(navigation.inboxPath.isEmpty)
    }

    @Test func aRefusedReceiptStillShowsTheCard() async throws {
        let server = server()
        let session = try session(server)
        let navigation = NavigationModel(launchInputs: .standard())

        // No such attempt: the daemon answers 404, as it does for a forged
        // link or another device's attempt.
        await session.open(
            .attentionItem(itemID: Self.itemID, channel: "ntfy", attempt: 9), in: navigation)

        #expect(navigation.inboxPath == [Self.itemID])
        #expect(try await openedAttempts(server).isEmpty)
    }

    @Test func theInboxLinkShowsTheInbox() async throws {
        let session = try session(server())
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil, screenRaw: "tasks"))
        #expect(navigation.selectedTab == .tasks)

        await session.open(.inbox, in: navigation)

        #expect(navigation.selectedTab == .inbox)
        #expect(navigation.inboxPath.isEmpty)
    }

    @Test func theInboxLinkLeavesAnOpenCardOpen() async throws {
        let session = try session(server())
        let navigation = NavigationModel(
            launchInputs: LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil, screenRaw: "tasks"))
        navigation.inboxPath = [Self.itemID]

        await session.open(.inbox, in: navigation)

        #expect(navigation.selectedTab == .inbox)
        #expect(navigation.inboxPath == [Self.itemID])
    }

    @Test func anUnpairedSessionIgnoresALink() async throws {
        let server = server()
        let session = try session(server, paired: false)
        let navigation = NavigationModel(launchInputs: .standard())

        await session.open(
            .attentionItem(itemID: Self.itemID, channel: "ntfy", attempt: 1), in: navigation)

        #expect(navigation.inboxPath.isEmpty)
        #expect(try await openedAttempts(server).isEmpty)
    }
}
