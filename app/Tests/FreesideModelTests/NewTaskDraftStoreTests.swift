import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

/// The New Task draft (#2935): kept for the app session, and without its
/// attachments across a relaunch, in a device- and deployment-owned file.
@Suite @MainActor struct NewTaskDraftStoreTests {
    private let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)

    private func store(_ deployment: String = "daemon", _ device: String = "device") -> NewTaskDraftStore {
        NewTaskDraftStore(deploymentID: deployment, deviceID: device, directory: directory)
    }

    private func attachment() -> TaskAttachmentDraft {
        var attachments = TaskAttachmentDraft()
        _ = attachments.add(name: "crash.log", claimedMediaType: "text/plain", bytes: Data("boom".utf8))
        return attachments
    }

    @Test func aDraftIsTextAttachmentsOrChoicesButNeverAProjectAlone() {
        var draft = NewTaskDraft()
        #expect(draft.isEmpty)
        draft.projectID = "freeside"
        draft.agentsOpen = true
        #expect(draft.isEmpty)
        draft.source = " \n\t"
        #expect(draft.isEmpty)
        draft.source = "Fix the rig"
        #expect(!draft.isEmpty)

        var choice = NewTaskDraft()
        choice.choices[.reviewer] = UsageFixtures.agentSelection("claude-primary")
        #expect(!choice.isEmpty)

        var attached = NewTaskDraft()
        attached.attachments = attachment()
        #expect(!attached.isEmpty)
    }

    @Test func theRowShowsOnlyForADraftWithItsProjectAndFirstLine() {
        var draft = NewTaskDraft()
        draft.projectID = "freeside"
        #expect(draft.row == nil)
        draft.source = "\n   \n  Repair the acceptance rig  \nwith a test"
        #expect(draft.row == .init(project: "freeside", title: "Repair the acceptance rig"))

        var untitled = NewTaskDraft()
        untitled.choices[.implementer] = UsageFixtures.agentSelection("claude-secondary")
        #expect(untitled.row == .init(project: "No project", title: "Untitled draft"))
    }

    @Test func aProjectSwitchKeepsTheText() {
        let drafts = NewTaskDraftStore(deploymentID: "daemon", deviceID: "device")
        drafts.draft.source = "Keep me"
        drafts.draft.projectID = "a"
        drafts.draft.projectID = "b"
        #expect(drafts.draft.source == "Keep me")
        #expect(drafts.draft.projectID == "b")
    }

    @Test func textProjectChoicesAndAgentsChipSurviveARelaunch() throws {
        defer { try? FileManager.default.removeItem(at: directory) }
        let choices: [AgentChoiceDisplay.Role: AgentChoiceDisplay.Selection] = [
            .implementer: UsageFixtures.agentSelection("claude-secondary"),
            .reviewer: UsageFixtures.agentSelection("claude-primary"),
        ]
        let first = store()
        first.draft.source = "Add a health endpoint.\nWith a test."
        first.draft.projectID = "freeside"
        first.draft.choices = choices
        first.draft.agentsOpen = true

        let restored = store().draft
        #expect(restored.source == "Add a health endpoint.\nWith a test.")
        #expect(restored.projectID == "freeside")
        #expect(restored.choices == choices)
        #expect(restored.agentsOpen)
        #expect(restored.attachments.isEmpty)
    }

    @Test func anotherDeploymentOrDeviceIgnoresTheFile() {
        defer { try? FileManager.default.removeItem(at: directory) }
        store().draft.source = "Mine"
        #expect(store("other-daemon").draft.isEmpty)
        #expect(store("daemon", "other-device").draft.isEmpty)
        #expect(store().draft.source == "Mine")
    }

    @Test func aCorruptOrUnknownFileYieldsNoDraft() throws {
        defer { try? FileManager.default.removeItem(at: directory) }
        store().draft.source = "Written"
        let file = try #require(
            FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil).first)
        #expect(file.lastPathComponent.hasPrefix("new-task-draft-"))

        try Data("{not json".utf8).write(to: file)
        #expect(store().draft.isEmpty)

        let otherVersion = """
            {"version":2,"deploymentID":"daemon","deviceID":"device","source":"x","choices":{},
            "agentsOpen":false,"hadAttachments":false}
            """
        try Data(otherVersion.utf8).write(to: file)
        #expect(store().draft.isEmpty)
    }

    @Test func aRoleThisBuildDoesntNameIsSkipped() throws {
        defer { try? FileManager.default.removeItem(at: directory) }
        store().draft.source = "Written"
        let file = try #require(
            FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil).first)
        let record = """
            {"version":1,"deploymentID":"daemon","deviceID":"device","source":"Kept","projectID":"freeside",
            "choices":{"auditor":{"enrollment_id":"e","model_id":"m","effort":"high"},
            "reviewer":{"enrollment_id":"e","model_id":"m","effort":"high"}},
            "agentsOpen":true,"hadAttachments":false}
            """
        try Data(record.utf8).write(to: file)
        let restored = store().draft
        #expect(restored.source == "Kept")
        #expect(restored.choices == [.reviewer: .init(enrollment_id: "e", model_id: "m", effort: "high")])
    }

    @Test func aFailedWriteWarnsAndKeepsTheDraft() {
        struct Refused: Error {}
        var fails = true
        let drafts = NewTaskDraftStore(
            deploymentID: "daemon", deviceID: "device", directory: directory,
            write: { _, _ in if fails { throw Refused() } })
        drafts.draft.source = "Still here"
        #expect(drafts.saveWarning != nil)
        #expect(drafts.draft.source == "Still here")
        fails = false
        drafts.draft.source = "Still here, saved"
        #expect(drafts.saveWarning == nil)
    }

    @Test func attachmentsDontSurviveARelaunchAndTheRestoredDraftSaysSo() {
        defer { try? FileManager.default.removeItem(at: directory) }
        let first = store()
        first.draft.source = "With a log"
        first.draft.attachments = attachment()
        #expect(!first.attachmentsDroppedAtRelaunch)

        let restored = store()
        #expect(restored.draft.source == "With a log")
        #expect(restored.draft.attachments.isEmpty)
        #expect(restored.attachmentsDroppedAtRelaunch)
        // Still unattached, so a second relaunch says it again.
        #expect(store().attachmentsDroppedAtRelaunch)

        restored.draft.attachments = attachment()
        #expect(!restored.attachmentsDroppedAtRelaunch)

        let again = store()
        #expect(again.attachmentsDroppedAtRelaunch)
        again.clear()
        #expect(!again.attachmentsDroppedAtRelaunch)
        #expect(again.draft.isEmpty)
        #expect(store().draft.isEmpty)
        #expect(!store().attachmentsDroppedAtRelaunch)
    }

    /// A draft that held only attachments restores as nothing, so there is
    /// nothing to say they were dropped from.
    @Test func anAttachmentOnlyDraftRestoresAsNoDraft() {
        defer { try? FileManager.default.removeItem(at: directory) }
        store().draft.attachments = attachment()
        let restored = store()
        #expect(restored.draft.isEmpty)
        #expect(!restored.attachmentsDroppedAtRelaunch)
    }

    /// A project alone, a keystroke's no-op set, and another attachment
    /// change nothing the file must hold, so they write nothing.
    @Test func onlyAChangeToTheRecordWrites() {
        var writes = 0
        let drafts = NewTaskDraftStore(
            deploymentID: "daemon", deviceID: "device", directory: directory, write: { _, _ in writes += 1 })
        drafts.draft.projectID = "freeside"
        #expect(writes == 0)
        drafts.draft.source = "A"
        drafts.draft.source = "A"
        #expect(writes == 1)
        // The first attachment flips the record's attachment mark; another
        // leaves the record as it was.
        drafts.draft.attachments = attachment()
        #expect(writes == 2)
        _ = drafts.draft.attachments.add(name: "more.log", claimedMediaType: "text/plain", bytes: Data("x".utf8))
        #expect(writes == 2)
    }

    /// Clearing removes the file rather than writing an empty record, so a
    /// sent draft can't return at relaunch even when writes fail.
    @Test func clearRemovesTheFileAndWarnsWhenItCant() throws {
        defer { try? FileManager.default.removeItem(at: directory) }
        let drafts = store()
        drafts.draft.source = "Sent"
        drafts.clear()
        #expect(try FileManager.default.contentsOfDirectory(atPath: directory.path).isEmpty)
        #expect(store().draft.isEmpty)
        drafts.clear()
        #expect(drafts.saveWarning == nil)

        struct Refused: Error {}
        let stuck = NewTaskDraftStore(
            deploymentID: "daemon", deviceID: "device", directory: directory, remove: { _ in throw Refused() })
        stuck.draft.source = "Sent"
        stuck.clear()
        #expect(stuck.draft.isEmpty)
        #expect(stuck.saveWarning != nil)
    }

    @Test func restorePutsBackAnUnsentDraftOnlyOverAnEmptyOne() {
        let drafts = NewTaskDraftStore(deploymentID: "daemon", deviceID: "device")
        var unsent = NewTaskDraft()
        unsent.source = "Rejected"
        drafts.restore(unsent)
        #expect(drafts.draft.source == "Rejected")
        var other = NewTaskDraft()
        other.source = "Other"
        drafts.restore(other)
        #expect(drafts.draft.source == "Rejected")
    }

    @Test func withoutADirectoryTheDraftStaysInMemory() {
        var writes = 0
        let drafts = NewTaskDraftStore(deploymentID: "mock", deviceID: "device", write: { _, _ in writes += 1 })
        drafts.draft.source = "Memory only"
        drafts.clear()
        #expect(writes == 0)
        #expect(drafts.saveWarning == nil)
    }
}
