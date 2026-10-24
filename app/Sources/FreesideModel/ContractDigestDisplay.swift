import FreesideAPI

/// Compact rendering of an API contract digest for the skew surfaces (the
/// freshness banner, the operational summary, and the Mac daemon menu),
/// kept in one place so the three read alike.
public enum ContractDigestDisplay {
    /// The first 12 hex characters of a `sha256:<hex>` digest: enough to
    /// distinguish two builds at a glance without showing all 64. Falls
    /// back to the raw string when it lacks the expected prefix.
    public static func short(_ digest: String) -> String {
        let hex = digest.hasPrefix("sha256:") ? String(digest.dropFirst("sha256:".count)) : digest
        return String(hex.prefix(12))
    }

    /// This client's own compiled-in contract digest, shortened. Lets the
    /// FreesideCore-only Mac app target render the client digest without
    /// importing FreesideAPI directly.
    public static var shortClient: String { short(APIContract.digest) }
}
