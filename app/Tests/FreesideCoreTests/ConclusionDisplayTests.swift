import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// A closed card's lead sentence, Outcome facts, applied routes, and
/// timeline link, read from the item's conclusion (#2097).
@Suite struct ConclusionDisplayTests {
    private let now = Date(timeIntervalSince1970: 1_800_000_000)
    private let locale = Locale(identifier: "en_US")
    private let utc = TimeZone(identifier: "UTC") ?? .gmt

    @Test func openItemsLeadWithTheirAskAndDrawNoOutcome() {
        let item = AttentionFixtures.fixture(type: .spec_approval).item
        #expect(ConclusionDisplay.leadSentence(item) == nil)
        #expect(ConclusionDisplay.facts(item, thisDeviceID: nil, now: now, locale: locale, timeZone: utc).isEmpty)
    }

    @Test func eachCauseLeadsWithWhatHappened() {
        #expect(
            ConclusionDisplay.leadSentence(AttentionFixtures.decidedReviewDispute().item)
                == "Findings adjudicated.")
        #expect(
            ConclusionDisplay.leadSentence(AttentionFixtures.expiredPublishBlocked().item)
                == "Expired before a decision.")
        #expect(
            ConclusionDisplay.leadSentence(AttentionFixtures.supersededEffectProposal().item)
                == "Superseded by a newer proposal.")
        #expect(
            ConclusionDisplay.leadSentence(AttentionFixtures.resolvedSystemHealth().item)
                == "Resolved by the daemon.")
    }

    /// The inbox row repeats the card's lead, so the list and the card say
    /// the same thing about one closed item.
    @Test func rowSummaryRepeatsTheLead() {
        for snapshot in AttentionFixtures.closedInbox() {
            #expect(AttentionDisplay.rowSummary(snapshot.item) == ConclusionDisplay.leadSentence(snapshot.item))
        }
    }

    /// An item closed before conclusions existed keeps its status sentence
    /// and draws no facts, rather than inventing a cause.
    @Test func legacyClosedItemKeepsItsStatusSentence() {
        var item = AttentionFixtures.decidedReviewDispute().item
        item.conclusion = nil
        #expect(ConclusionDisplay.leadSentence(item) == "This review dispute item is resolved.")
        #expect(ConclusionDisplay.facts(item, thisDeviceID: nil, now: now, locale: locale, timeZone: utc).isEmpty)
    }

    @Test func aDecisionRevisingItsItemSaysItSuperseded() {
        var item = AttentionFixtures.decidedReviewDispute().item
        item._type = .spec_approval
        item.status = .superseded
        #expect(ConclusionDisplay.leadSentence(item) == "Superseded by a revised specification.")
    }

    @Test func decisionFactsNameTheActionAndTheDecidingDevice() {
        let item = AttentionFixtures.decidedReviewDispute().item
        let facts = ConclusionDisplay.facts(
            item, thisDeviceID: DeviceFixtures.currentDeviceID, now: now, locale: locale, timeZone: utc)
        #expect(facts.map(\.label) == ["Action", "Decided by", "When"])
        #expect(facts[0].value == AttentionDisplay.label(.adjudicate_findings))
        #expect(facts[1].value == "Ben's iPhone")

        let here = ConclusionDisplay.facts(
            item, thisDeviceID: DeviceFixtures.otherDeviceID, now: now, locale: locale, timeZone: utc)
        #expect(here[1].value == "This device")
    }

    /// The card prints the instant in the view's zone, not the process's,
    /// so a screenshot pinned to UTC renders the same on every machine.
    @Test func whenPrintsInTheGivenTimeZone() throws {
        let item = AttentionFixtures.decidedReviewDispute().item
        let concludedAt = try #require(item.conclusion?.value1.concluded_at)
        let tokyo = try #require(TimeZone(identifier: "Asia/Tokyo"))
        func when(_ zone: TimeZone) -> String? {
            ConclusionDisplay.facts(item, thisDeviceID: nil, now: now, locale: locale, timeZone: zone)
                .first { $0.label == "When" }?.value
        }
        #expect(when(utc) == FreesideFormat.shortTime(concludedAt, now: now, locale: locale, timeZone: utc))
        #expect(when(tokyo) == FreesideFormat.shortTime(concludedAt, now: now, locale: locale, timeZone: tokyo))
        #expect(when(utc) != when(tokyo))
    }

    @Test func daemonFactsNameTheDaemonTheReasonAndTheSuccessor() throws {
        let item = AttentionFixtures.supersededEffectProposal().item
        let facts = ConclusionDisplay.facts(item, thisDeviceID: nil, now: now, locale: locale, timeZone: utc)
        #expect(facts.map(\.label) == ["Closed by", "When", "Reason", "Replaced by"])
        #expect(facts[0].value == "Daemon")
        #expect(facts[2].value == item.conclusion?.value1.reason)
        let successor = try #require(item.conclusion?.value1.successor_item_id)
        #expect(facts[3].form == .item(id: successor))
    }

    @Test func appliedRouteFollowsTheConcludingAction() throws {
        let decided = AttentionFixtures.decidedReviewDispute().item
        #expect(ConclusionDisplay.appliedRoute(for: "finding-1", proposed: nil, in: decided) == .remediate)
        #expect(ConclusionDisplay.appliedRoute(for: "finding-2", proposed: nil, in: decided) == ._defer)

        var accepted = decided
        var conclusion = try #require(accepted.conclusion?.value1)
        var decision = try #require(conclusion.decision?.value1)
        decision.action = .accept_recommended_route
        decision.finding_routes = nil
        conclusion.decision = .init(value1: decision)
        accepted.conclusion = .init(value1: conclusion)
        #expect(ConclusionDisplay.appliedRoute(for: "finding-1", proposed: .decline, in: accepted) == .decline)

        let expired = AttentionFixtures.expiredPublishBlocked().item
        #expect(ConclusionDisplay.appliedRoute(for: "finding-1", proposed: .decline, in: expired) == nil)
    }

    /// The link names only a task the app has synced, so it never opens a
    /// task detail that says "Not available".
    @Test func timelineLinkResolvesTheSubjectsSyncedTask() throws {
        let item = AttentionFixtures.expiredPublishBlocked().item
        guard case .run(let run) = item.subject, let taskID = run.task_id else {
            Issue.record("the expired fixture is run-scoped with a task")
            return
        }
        var task = try #require(TaskFixtures.defaultTasks().first)
        #expect(ConclusionDisplay.timelineTaskID(for: item, in: [task]) == nil)
        task.task.id = taskID
        #expect(ConclusionDisplay.timelineTaskID(for: item, in: [task]) == taskID)
    }
}
