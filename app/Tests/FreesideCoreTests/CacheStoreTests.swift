import Foundation
import FreesideAPI
import FreesideCore
import Testing

private func temporaryStore() -> (DiskCacheStore, URL) {
    let directory = FileManager.default.temporaryDirectory
        .appendingPathComponent("freeside-cache-tests-\(UUID().uuidString)")
    return (DiskCacheStore(directory: directory), directory)
}

private func sampleState(revision: Int64 = 5) -> CachedState {
    CachedState(
        cursors: SyncCursors(
            syncEpoch: "epoch-1",
            lastFullSnapshotRevision: revision,
            highestObservedServerRevision: revision
        ),
        attentionItems: [AttentionFixtures.fixture(type: .spec_approval)]
    )
}

@Suite struct DiskCacheStoreTests {
    @Test func roundTripsTheCachedState() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }

        #expect(store.load() == nil)
        let state = sampleState()
        store.save(state)
        #expect(store.load() == state)

        // A later save replaces the earlier state wholesale, as a
        // bootstrap rebuild does.
        let newer = sampleState(revision: 9)
        store.save(newer)
        #expect(store.load() == newer)
    }

    @Test func anythingUnreadableLoadsAsAbsent() throws {
        // The cache is disposable by design: corruption, a foreign
        // format, or a future incompatible version all mean "bootstrap",
        // never a decode error surfaced to the user.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        try FileManager.default.createDirectory(
            at: directory, withIntermediateDirectories: true)
        let file = directory.appendingPathComponent("cache.json")

        try Data("not json {".utf8).write(to: file)
        #expect(store.load() == nil)

        try Data(#"{"format": 999, "state": {}}"#.utf8).write(to: file)
        #expect(store.load() == nil)
    }

    @Test func discardDeletesTheFile() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        store.save(sampleState())
        #expect(store.load() != nil)

        store.discard()

        #expect(store.load() == nil)
        #expect(
            !FileManager.default.fileExists(
                atPath: directory.appendingPathComponent("cache.json").path))
    }
}

@Suite struct CredentialStoreTests {
    @Test func inMemoryStoreRoundTrips() throws {
        let store = InMemoryCredentialStore()
        #expect(try store.load() == nil)

        let credential = DeviceCredential(deviceID: "device-1", token: "fsd1.id.secret")
        try store.save(credential)
        #expect(try store.load() == credential)

        try store.delete()
        #expect(try store.load() == nil)
    }

    @Test func keychainStoreRoundTripsUnderAScopedService() throws {
        // Runs against the real Keychain (FreesideCoreTests are
        // Apple-platform, developer-machine tests; CI's Linux job never
        // sees them). The service name is unique per run and the item
        // is removed either way.
        let store = KeychainCredentialStore(
            service: "ai.freeside.tests.\(UUID().uuidString)")
        defer { try? store.delete() }

        #expect(try store.load() == nil)
        let first = DeviceCredential(deviceID: "device-1", token: "fsd1.one.secret")
        try store.save(first)
        #expect(try store.load() == first)

        // A re-pair replaces the whole identity.
        let second = DeviceCredential(deviceID: "device-2", token: "fsd1.two.secret")
        try store.save(second)
        #expect(try store.load() == second)

        try store.delete()
        #expect(try store.load() == nil)
    }
}
