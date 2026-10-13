#if os(macOS)
    import Testing

    @testable import FreesideCore

    @Suite @MainActor struct ChoiceListTests {
        @Test func arrowKeysChooseAnEndWhenNothingIsSelected() {
            #expect(ChoiceList<String>.indexAfterMoving(optionCount: 3, selectedIndex: nil, offset: 1) == 0)
            #expect(ChoiceList<String>.indexAfterMoving(optionCount: 3, selectedIndex: nil, offset: -1) == 2)
        }

        @Test func arrowKeysKeepAnEndSelectedAndIgnoreEmptyLists() {
            #expect(ChoiceList<String>.indexAfterMoving(optionCount: 3, selectedIndex: 0, offset: -1) == 0)
            #expect(ChoiceList<String>.indexAfterMoving(optionCount: 3, selectedIndex: 2, offset: 1) == 2)
            #expect(ChoiceList<String>.indexAfterMoving(optionCount: 0, selectedIndex: nil, offset: 1) == nil)
        }
    }
#endif
