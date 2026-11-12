#if os(macOS)
    import Testing

    @testable import FreesideCore

    @Suite @MainActor struct ChoiceListTests {
        @Test func arrowKeysChooseAnEndWhenNothingIsSelected() {
            #expect(
                ChoiceList<String>.indexAfterMoving(isEnabled: [true, true, true], selectedIndex: nil, offset: 1) == 0)
            #expect(
                ChoiceList<String>.indexAfterMoving(isEnabled: [true, true, true], selectedIndex: nil, offset: -1) == 2)
        }

        @Test func arrowKeysKeepAnEndSelectedAndIgnoreEmptyLists() {
            #expect(
                ChoiceList<String>.indexAfterMoving(isEnabled: [true, true, true], selectedIndex: 0, offset: -1) == 0)
            #expect(
                ChoiceList<String>.indexAfterMoving(isEnabled: [true, true, true], selectedIndex: 2, offset: 1) == 2)
            #expect(ChoiceList<String>.indexAfterMoving(isEnabled: [], selectedIndex: nil, offset: 1) == nil)
            #expect(ChoiceList<String>.indexAfterMoving(isEnabled: [false, false], selectedIndex: 0, offset: 1) == nil)
        }

        /// The role picker's rows (#3027): a row that can't be picked is
        /// stepped over, and one that is selected moves from where it sits,
        /// not from an end.
        @Test func arrowKeysStepOverDisabledOptionsFromWhereTheSelectionSits() {
            let rows = [true, false, true, false, true]
            #expect(ChoiceList<String>.indexAfterMoving(isEnabled: rows, selectedIndex: 0, offset: 1) == 2)
            #expect(ChoiceList<String>.indexAfterMoving(isEnabled: rows, selectedIndex: 1, offset: 1) == 2)
            #expect(ChoiceList<String>.indexAfterMoving(isEnabled: rows, selectedIndex: 1, offset: -1) == 0)
            #expect(ChoiceList<String>.indexAfterMoving(isEnabled: rows, selectedIndex: 3, offset: -1) == 2)
            #expect(ChoiceList<String>.indexAfterMoving(isEnabled: [true, false], selectedIndex: 1, offset: 1) == 1)
            #expect(ChoiceList<String>.indexAfterMoving(isEnabled: [false, true], selectedIndex: nil, offset: 1) == 1)
        }
    }
#endif
