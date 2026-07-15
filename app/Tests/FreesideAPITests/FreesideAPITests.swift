import FreesideAPI
import Testing

@Test
func generatedClientDecodesMockRevision() async throws {
    let response = try await APIClientFactory.mock().getSyncRevision()
    let revision = try response.ok.body.json

    #expect(revision.sync_epoch == "mock-epoch")
    #expect(revision.revision == 1)
}
