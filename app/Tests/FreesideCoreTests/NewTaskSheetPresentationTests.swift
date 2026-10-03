import Testing

@testable import FreesideCore

/// The New task form's fields (visual audit D05): an empty field shows a
/// placeholder that is never its value and never submitted.
@MainActor
@Suite struct NewTaskSheetPresentationTests {
    private let placeholders = [NewTaskSheet.sourcePlaceholder, NewTaskSheet.namePlaceholder]

    @Test func emptyDraftShowsPlaceholdersAndSubmitsNoName() {
        #expect(placeholders == ["Describe the task…", "Name (optional)"])
        for placeholder in placeholders {
            #expect(NewTaskSheet.fieldDisplay("", placeholder: placeholder) == .placeholder(placeholder))
        }
        let empty = NewTaskSheet.submission(source: "", name: "")
        #expect(empty.source.isEmpty)
        #expect(empty.name == nil)
    }

    /// One rule for the editor's drawn placeholder, the name field's native
    /// prompt, and the static render: any typed character hides it. The
    /// whitespace is still not submitted.
    @Test func whitespaceOnlyDraftHidesThePlaceholderAndSubmitsNothing() {
        for placeholder in placeholders {
            for blank in ["  ", "\n\t"] {
                #expect(NewTaskSheet.fieldDisplay(blank, placeholder: placeholder) == .value(blank))
            }
        }
        let blank = NewTaskSheet.submission(source: " \n", name: " \n")
        #expect(blank == .init(source: "", name: nil))
    }

    @Test func typedTextIsAValueEvenWhenItReadsLikeThePlaceholder() {
        for placeholder in placeholders {
            #expect(
                NewTaskSheet.fieldDisplay("Fix the flaky rig", placeholder: placeholder)
                    == .value("Fix the flaky rig"))
            // The operator typed these words, so they are the draft.
            #expect(NewTaskSheet.fieldDisplay(placeholder, placeholder: placeholder) == .value(placeholder))
        }
        let typed = NewTaskSheet.submission(
            source: NewTaskSheet.sourcePlaceholder, name: NewTaskSheet.namePlaceholder)
        #expect(typed.source == NewTaskSheet.sourcePlaceholder)
        #expect(typed.name == NewTaskSheet.namePlaceholder)
    }

    @Test func submissionCarriesOnlyTheTrimmedDraft() {
        let draft = NewTaskSheet.submission(source: "  Repair the acceptance rig\n", name: " rig ")
        #expect(draft == .init(source: "Repair the acceptance rig", name: "rig"))
        // A field left blank sends nothing in its place, least of all the
        // placeholder it showed.
        let blank = NewTaskSheet.submission(source: "", name: "")
        #expect(blank == .init(source: "", name: nil))
        let unnamed = NewTaskSheet.submission(source: "Repair the acceptance rig", name: "")
        #expect(unnamed == .init(source: "Repair the acceptance rig", name: nil))
    }
}
