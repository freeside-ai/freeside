import Crypto
import Foundation

extension APIContract {
    /// "sha256:" plus the lowercase hex SHA-256 of the spec's exact bytes.
    /// Computed once from the bundled openapi.yaml resource rather than
    /// committed as a constant, so unrelated spec edits do not collide on a
    /// hash line during rebase. It names the contract for diagnosis;
    /// compatibility comes from the level and floor, never from a digest
    /// difference.
    public static let digest: String = {
        guard let url = Bundle.module.url(forResource: "openapi", withExtension: "yaml") else {
            preconditionFailure("FreesideAPI bundle is missing openapi.yaml")
        }
        do {
            let spec = try Data(contentsOf: url)
            return "sha256:" + SHA256.hash(data: spec).map { String(format: "%02x", $0) }.joined()
        } catch {
            preconditionFailure("FreesideAPI failed to read openapi.yaml: \(error)")
        }
    }()
}
