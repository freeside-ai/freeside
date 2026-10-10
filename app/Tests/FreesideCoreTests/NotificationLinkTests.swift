import Foundation
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
