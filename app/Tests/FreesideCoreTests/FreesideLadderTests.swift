import SwiftUI
import Testing

@testable import FreesideCore

struct FreesideLadderTests {
    /// R10 as the 8 Oct 2026 interfaces handoff states it: the Mac sets on
    /// its native sizes.
    @Test func theMacLadderIsTheHandoffs() {
        let mac = FreesideLadder.mac
        #expect(mac.ask == 20)
        #expect(mac.sheetAsk == 20)
        #expect(mac.statement == 15)
        #expect(mac.rowTitle == 14)
        #expect(mac.label == 13)
        #expect(mac.body == 13)
        #expect(mac.monoValue == 12)
        #expect(mac.trailingSummary == 12)
        #expect(mac.keyword == 11)
        #expect(mac.chip == 11)
        #expect(mac.link == 13)
        #expect(mac.actionLabel == 13)
        #expect(mac.actionHeight == 28)
        #expect(mac.actionPadding == CGSize(width: 14, height: 4))
        #expect(mac.infoMark == 15)
        #expect([mac.sectionGap, mac.moduleGap, mac.controlGap, mac.foldLead] == [18, 9, 8, 14])
        #expect(mac.floor == 11)
    }

    /// The iPhone keeps the survey card's scale a point smaller.
    @Test func thePhoneLadderIsTheHandoffs() {
        let phone = FreesideLadder.phone
        #expect(phone.ask == 24)
        #expect(phone.sheetAsk == 22)
        #expect(phone.statement == 16)
        #expect(phone.rowTitle == 15.5)
        #expect(phone.label == 15)
        #expect(phone.body == 13.5)
        #expect(phone.monoValue == 13.5)
        #expect(phone.trailingSummary == 12.5)
        #expect(phone.keyword == 11.5)
        #expect(phone.chip == 11.5)
        #expect(phone.link == 14)
        #expect(phone.actionLabel == 15)
        #expect(phone.actionHeight == 44)
        #expect(phone.actionPadding == CGSize(width: 16, height: 7))
        #expect(phone.infoMark == 16)
        #expect([phone.sectionGap, phone.moduleGap, phone.controlGap, phone.foldLead] == [20, 10, 10, 16])
        #expect(phone.floor == 11.5)
    }

    /// Nothing a ladder sets as text is under its platform's floor, and an
    /// action never shrinks at an accessibility size.
    @Test(arguments: [FreesideLadder.mac, FreesideLadder.phone])
    func noTextStepIsUnderTheFloor(ladder: FreesideLadder) {
        let text = [
            ladder.ask, ladder.sheetAsk, ladder.statement, ladder.rowTitle, ladder.label, ladder.body,
            ladder.monoValue, ladder.trailingSummary, ladder.keyword, ladder.chip, ladder.link,
            ladder.actionLabel,
        ]
        #expect(text.allSatisfy { $0 >= ladder.floor })
        #expect(ladder.accessibilityActionHeight > ladder.actionHeight)
    }

    /// A platform text style the system sets under the floor is held at it:
    /// macOS draws its caption and footnote styles at 10pt.
    @Test func aPlatformTextStyleIsHeldAtTheFloor() {
        #expect(FreesideFont.floored(10, on: .mac) == 11)
        #expect(FreesideFont.floored(11, on: .phone) == 11.5)
        #expect(FreesideFont.floored(13, on: .mac) == 13)
        for style in [Font.TextStyle.caption2, .caption, .footnote, .subheadline, .callout, .body] {
            #expect(FreesideFont.size(of: style) >= FreesideLadder.current.floor)
        }
    }

    #if os(macOS)
        @Test func aMacBuildDrawsTheMacLadder() {
            #expect(FreesideLadder.current == .mac)
        }
    #endif
}
