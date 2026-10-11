import Foundation

/// A `freeside://` link from a notification's tap action. The daemon puts
/// one on every notification it sends (`notificationFor` in
/// `daemon/internal/signet/ntfy.go`); the two must agree on the form.
///
/// Any app or web page on the phone can open such a link, so a link only
/// names a place to show and the delivery attempt to mark opened. It never
/// carries an action, and every other URL parses to nothing.
public enum NotificationLink: Equatable, Sendable {
    public static let scheme = "freeside"

    /// `freeside://attention/items/<id>?channel=<name>&attempt=<n>`: the
    /// item's card, and the delivery attempt whose notification was tapped.
    case attentionItem(itemID: String, channel: String, attempt: Int)
    /// `freeside://inbox`: the inbox tab as it stands, as the test notice
    /// links. It names no item, so it leaves an open card alone.
    case inbox

    public init?(_ url: URL) {
        guard
            let components = URLComponents(url: url, resolvingAgainstBaseURL: false),
            components.scheme?.lowercased() == Self.scheme,
            components.user == nil, components.password == nil,
            components.port == nil, components.fragment == nil
        else { return nil }
        // The host is matched as written. `URLComponents` would also read
        // `%69nbox` as "inbox" and drop an empty port (`inbox:`); the daemon
        // writes neither.
        let afterScheme = url.absoluteString.dropFirst(Self.scheme.count)
        if afterScheme == "://inbox" {
            self = .inbox
            return
        }
        let segments = components.percentEncodedPath.split(separator: "/", omittingEmptySubsequences: false)
        // A leading slash makes the first segment empty: ["", "items", id].
        guard afterScheme.hasPrefix("://attention/"),
            segments.count == 3, segments[0].isEmpty, segments[1] == "items",
            let itemID = String(segments[2]).removingPercentEncoding, Self.isPathValue(itemID),
            let query = Self.singleValues(components.queryItems ?? []),
            query.count == 2,
            let channel = query["channel"], Self.isPathValue(channel),
            let attempt = query["attempt"].flatMap(Self.attemptNumber)
        else { return nil }
        self = .attentionItem(itemID: itemID, channel: channel, attempt: attempt)
    }

    /// Whether an item ID or channel name is one the receipt's request can
    /// carry as a path segment. A dot segment is refused because a URL
    /// loader resolves it, which would aim the receipt at another path.
    private static func isPathValue(_ text: String) -> Bool {
        !text.isEmpty && text != "." && text != ".."
            && !text.unicodeScalars.contains { $0.properties.generalCategory == .control }
    }

    /// The query as one value per name, or nil when a name repeats or has
    /// no value: a link the daemon did not write.
    private static func singleValues(_ items: [URLQueryItem]) -> [String: String]? {
        var values: [String: String] = [:]
        for item in items {
            guard let value = item.value, values.updateValue(value, forKey: item.name) == nil else {
                return nil
            }
        }
        return values
    }

    /// A 1-based attempt number in plain decimal digits with no leading
    /// zero, as the daemon writes it. `Int("+1")` and `Int("１")` both parse,
    /// so the digits are checked first.
    private static func attemptNumber(_ text: String) -> Int? {
        guard text.utf8.first != UInt8(ascii: "0"),
            !text.isEmpty, text.utf8.allSatisfy({ (UInt8(ascii: "0")...UInt8(ascii: "9")).contains($0) }),
            let attempt = Int(text), attempt >= 1
        else { return nil }
        return attempt
    }
}
