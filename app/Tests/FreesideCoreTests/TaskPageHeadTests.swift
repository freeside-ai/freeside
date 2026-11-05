import Foundation
import FreesideAPI
import FreesideTestSupport
import SwiftUI
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// The task page's head (#2609): the documents line under the status, the
/// outline row's chips, and the specification reader the line opens.
@Suite @MainActor struct TaskPageHeadTests {
    private let approvedDigest = "sha256:" + String(repeating: "c", count: 64)
    private let repo = "freeside-ai/freeside"

    private func links(
        _ page: TaskProgressFixtures.Case, history: Components.Schemas.TaskTimeline?,
        completion: Components.Schemas.WorkUnitCompletionFacts? = nil
    ) -> [TaskTimelinePresentation.DocumentLink] {
        TaskTimelinePresentation.documentLinks(
            page.task, timeline: history, completion: completion, items: page.items.map(\.item))
    }

    private func completion(pr number: Int) -> Components.Schemas.WorkUnitCompletionFacts {
        .init(pr_number: number, merge_commit_sha: "4be1d0a7", recorded_at: Date(timeIntervalSince1970: 0))
    }

    private func issueURL(_ number: Int) -> URL? { URL(string: "https://github.com/\(repo)/issues/\(number)") }

    /// An item under the task that names the PR's repository.
    private func readyItem(pr number: Int) -> Components.Schemas.AttentionItemSnapshot {
        var item = TaskStageFixtures.item(.ready_for_final_review, id: "head-ready", hoursOld: 0)
        item.item.pr_reference = .init(value1: .init(repo: repo, number: number))
        return item
    }

    /// A held approval whose Specification claim binds `digest`.
    private func approval(binding digest: String) throws -> Components.Schemas.AttentionItemSnapshot {
        var item = TaskStageFixtures.item(.spec_approval, id: "head-approval", hoursOld: 0)
        let index = try #require(
            item.item.agent_claims.firstIndex { $0.label == AgentClaimLabels.specification })
        item.item.agent_claims[index].digest = digest
        return item
    }

    // MARK: Documents line

    @Test func anIssueTaskWithANamedPullRequestLinksAllThree() throws {
        var page = TaskStageFixtures.page(.review)
        page.items = [readyItem(pr: 2431)]
        let source = try #require(TaskDisplay.issueReference(page.task))
        let issue = try #require(Int(source.dropFirst()))
        #expect(
            links(page, history: page.history, completion: completion(pr: 2431)) == [
                .issue(number: issue, url: issueURL(issue)),
                .specification(.init(digest: approvedDigest, revision: nil)),
                .pullRequest(number: 2431, url: URL(string: "https://github.com/\(repo)/pull/2431")),
            ])
    }

    @Test func aTypedTaskWithAnApprovedSpecificationAndNoPullRequest() {
        var page = TaskStageFixtures.page(.implementation)
        page.task.source = .init(
            value1: .work_item_artifact(
                .init(
                    kind: .work_item_artifact, work_item_artifact_id: "source-1",
                    work_item_artifact_digest: .init(value1: "sha256:" + String(repeating: "e", count: 64)))))
        page.items = []
        #expect(
            links(page, history: page.history) == [
                .specification(.init(digest: approvedDigest, revision: nil)), .noPullRequest,
            ])
    }

    @Test func aTaskStillInSpecificationHasNoSpecificationLink() {
        var page = TaskStageFixtures.page(.specification)
        page.items = []
        let parts = links(page, history: page.history)
        #expect(!parts.contains { if case .specification = $0 { true } else { false } })
        #expect(parts.last == .noPullRequest)
    }

    @Test func aPullRequestNoHeldItemNamesIsPlainText() {
        var page = TaskStageFixtures.page(.review)
        page.items = [readyItem(pr: 99)]
        #expect(
            links(page, history: page.history, completion: completion(pr: 2431)).last
                == .pullRequest(number: 2431, url: nil))
    }

    @Test func theHistoryRecordsThePullRequestBeforeACompletionDoes() throws {
        var page = TaskStageFixtures.page(.review)
        page.items = [readyItem(pr: 77)]
        var history = try #require(page.history)
        history.events.insert(
            .init(kind: .pr_opened, recorded_at: Date(), run_id: page.task.run_ids.last, pr_number: 77), at: 0)
        #expect(
            links(page, history: history).last
                == .pullRequest(number: 77, url: URL(string: "https://github.com/\(repo)/pull/77")))
    }

    @Test func beforeTheHistoryLoadsThePullRequestPartIsLeftOut() {
        var page = TaskStageFixtures.page(.review)
        page.items = []
        let parts = links(page, history: nil)
        #expect(parts.count == 1)
        if case .issue = parts.first {} else { Issue.record("expected the issue alone, got \(parts)") }
    }

    /// A PR the run's completion names needs no history, so it draws
    /// before the history loads; only `No pull request yet` waits for it.
    @Test func aCompletionNamesThePullRequestBeforeTheHistoryLoads() {
        var page = TaskStageFixtures.page(.review)
        page.items = [readyItem(pr: 2431)]
        #expect(
            links(page, history: nil, completion: completion(pr: 2431)).last
                == .pullRequest(number: 2431, url: URL(string: "https://github.com/\(repo)/pull/2431")))
    }

    /// The revision word is the held approval's, and only when its
    /// Specification claim binds the digest the campaign approved.
    @Test func theRevisionComesOnlyFromAnApprovalBindingTheApprovedDigest() throws {
        var page = TaskStageFixtures.page(.implementation)
        let matching = try approval(binding: approvedDigest)
        let revision = try #require(DecisionDetailView.specificationRevisionIteration(in: matching.item))
        page.items = [matching]
        #expect(
            links(page, history: page.history).contains(
                .specification(.init(digest: approvedDigest, revision: revision))))

        page.items = [try approval(binding: "sha256:" + String(repeating: "d", count: 64))]
        #expect(
            links(page, history: page.history).contains(.specification(.init(digest: approvedDigest, revision: nil))))
        #expect(SpecApprovalReader.revisionChip(nil) == nil)
        #expect(SpecApprovalReader.revisionChip(revision)?.label == "Revision \(revision)")
    }

    /// VoiceOver reads each part with somewhere to go as a link naming
    /// where it goes; a part with nowhere to go is plain text.
    @Test func eachDocumentLinkSaysWhereItGoes() {
        let url = URL(string: "https://github.com/\(repo)/issues/712")
        let request = TaskSpecificationRequest(digest: approvedDigest, revision: 2)
        let cases: [(TaskTimelinePresentation.DocumentLink, String, String, String?)] = [
            (.issue(number: 712, url: url), "Issue #712", "Issue 712", "Opens on GitHub"),
            (
                .specification(request), "Specification rev 2", "Specification revision 2",
                "Opens the specification reader"
            ),
            (
                .specification(.init(digest: approvedDigest, revision: nil)), "Specification", "Specification",
                "Opens the specification reader"
            ),
            (.pullRequest(number: 2431, url: url), "Pull request #2431", "Pull request 2431", "Opens on GitHub"),
            (.pullRequest(number: 2431, url: nil), "Pull request #2431", "Pull request 2431", nil),
            (.noPullRequest, "No pull request yet", "No pull request yet", nil),
        ]
        for (link, title, spoken, hint) in cases {
            #expect(link.title == title)
            #expect(link.spokenTitle == spoken)
            #expect(link.accessibilityHint == hint)
        }
    }

    // MARK: Outline and Task Actions

    /// Task Actions sits in the outline row at regular width and in the
    /// navigation bar on a phone, so the phone's row holds no menu. Its own
    /// item draws while the Identifiers footer does: once the history
    /// loads, or while the leading decision names identifiers.
    @Test func taskActionsSitWherePinnedAndHoldTodaysItems() {
        #expect(TaskPageView.TaskActionsSlot(compact: false) == .outlineRow)
        #expect(TaskPageView.TaskActionsSlot(compact: true) == .navigationBar)
        #expect(TaskPageView.taskActions(nil, taskID: "task-1").isEmpty)
        #expect(
            TaskPageView.taskActions(nil, taskID: "task-1", decision: [.init(label: "Item", value: "item-1")])
                .map(\.title) == ["Copy all identifiers"])
        let history = TaskStageFixtures.page(.review).history
        #expect(TaskPageView.taskActions(history, taskID: "task-1").map(\.title) == ["Copy all identifiers"])
    }

    @Test func theConversationChipDrawsFromOneMessage() {
        #expect(TaskPageSection.outline(decisions: 0, conversationMessages: 0) == [.task, .timeline, .runs])
        #expect(
            TaskPageSection.outline(decisions: 2, conversationMessages: 1) == [
                .decisions, .task, .conversation, .timeline, .runs,
            ])
    }

    // MARK: Specification reader

    @Test func theReaderLoadsTheApprovedBytesAndNamesTheDigestWhenItCannot() async {
        let bytes = Data("# Specification\n\nStore first, then API.\n".utf8)
        let loaded = AttachmentLoader(
            client: APIClientFactory.mock(server: MockServer(attachments: [approvedDigest: bytes])))
        #expect(
            await TaskSpecificationReaderSheet.load(approvedDigest) { await loaded.nonImageBytes(for: $0) }
                == .loaded(bytes))

        let missing = AttachmentLoader(client: APIClientFactory.mock(server: MockServer(attachments: [:])))
        #expect(
            await TaskSpecificationReaderSheet.load(approvedDigest) { await missing.nonImageBytes(for: $0) }
                == .unavailable)

        let server = MockServer(attachments: [approvedDigest: bytes])
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { throw InjectedFailure() }
        }
        let failing = AttachmentLoader(client: APIClientFactory.mock(server: server))
        #expect(
            await TaskSpecificationReaderSheet.load(approvedDigest) { await failing.nonImageBytes(for: $0) }
                == .unavailable)
    }
}
