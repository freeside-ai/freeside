/// Shortens an opaque identifier for primary text, where it appears only to
/// disambiguate one row from another. A real run or campaign id is its kind
/// prefix plus 64 hex characters and a task id its prefix plus 26 characters
/// (`daemon/internal/store/production_attempt.go`, `task.go`); a digest is
/// `sha256:` plus 64 hex characters. The exact value stays in the technical
/// details section, so a short form never has to round-trip.
package enum ShortIdentifier {
    private static let digestPrefix = "sha256:"
    private static let kindPrefixes = ["run-", "campaign-", "task-"]

    /// An id of 24 characters or fewer prints whole. A longer one keeps its
    /// kind prefix and the next 8 characters; a digest keeps `sha256:` and its
    /// first 12 hex characters. An id with no known kind prefix is left whole
    /// rather than truncated at a meaningless point.
    package static func short(_ identifier: String) -> String {
        if identifier.hasPrefix(digestPrefix) {
            let hex = identifier.dropFirst(digestPrefix.count)
            return hex.count > 12 ? "\(digestPrefix)\(hex.prefix(12))…" : identifier
        }
        guard identifier.count > 24 else { return identifier }
        for prefix in kindPrefixes where identifier.hasPrefix(prefix) {
            return "\(prefix)\(identifier.dropFirst(prefix.count).prefix(8))…"
        }
        return identifier
    }
}
