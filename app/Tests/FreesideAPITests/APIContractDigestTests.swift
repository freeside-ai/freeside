import Crypto
import Foundation
import FreesideAPI
import Testing

/// Drift gate for the bundled OpenAPI spec copy and the contract digest
/// computed from it: APIContract.digest must equal "sha256:" plus the
/// lowercase hex SHA-256 of the repository's api/openapi.yaml.
@Suite struct APIContractDigestTests {
    @Test func digestMatchesRepositorySpec() throws {
        let repositoryRoot = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()  // FreesideAPITests
            .deletingLastPathComponent()  // Tests
            .deletingLastPathComponent()  // app
            .deletingLastPathComponent()
        let specURL = repositoryRoot.appendingPathComponent("api/openapi.yaml")
        let specBytes = try Data(contentsOf: specURL)
        let expected =
            "sha256:" + SHA256.hash(data: specBytes).map { String(format: "%02x", $0) }.joined()

        #expect(APIContract.digest == expected)
    }
}
