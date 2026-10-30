import SwiftUI

/// The project filter's list: a search field over `All projects`, every known
/// project, and `No project` when a scoped item names none. Read only: the
/// daemon owns the projects, so nothing here creates or edits one.
struct ProjectFilterPopover: View {
    /// Two letters from the project's last word, so sibling repositories
    /// tell apart: `freeside-ai/freeside` reads `FR`.
    static func monogram(_ project: String) -> String {
        let word = project.split(whereSeparator: { "/-_ ".contains($0) }).last.map(String.init) ?? project
        return word.prefix(2).uppercased()
    }

    let projects: [String]
    let offersNoProject: Bool
    let selection: String?
    let onSelect: (String?) -> Void
    /// False for the screenshot goldens: `ImageRenderer` cannot draw a
    /// `TextField`, so the field renders as a static stand-in.
    var rendersInteractiveControls = true
    @State private var query = ""
    @FocusState private var searchFocused: Bool

    private var matches: [String] {
        query.isEmpty ? projects : projects.filter { $0.localizedCaseInsensitiveContains(query) }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Group {
                if rendersInteractiveControls {
                    TextField("Search projects…", text: $query)
                        .textFieldStyle(.plain)
                        .focused($searchFocused)
                } else {
                    Text("Search projects…").foregroundStyle(Color.inkDim)
                }
            }
            .font(FreesideFont.cardBody)
            .padding(.vertical, 6)
            .overlay(alignment: .bottom) {
                Rectangle().fill(Color.accentText).frame(height: 1)
            }
            row(title: "All projects", value: nil) {
                tile {
                    Image(systemName: "checkmark")
                        .foregroundStyle(Color.accentText)
                        .opacity(selection == nil ? 1 : 0)
                }
            }
            ForEach(matches, id: \.self) { project in
                row(title: project, value: project) {
                    tile { Text(Self.monogram(project)) }
                }
            }
            if offersNoProject, query.isEmpty {
                row(title: "No project", value: UnifiedList.noProject) {
                    Color.clear
                        .frame(width: 22, height: 22)
                        .overlay(
                            RoundedRectangle(cornerRadius: 5)
                                .strokeBorder(
                                    Color.ruleStrong, style: StrokeStyle(lineWidth: 1, dash: [3, 2]))
                        )
                        .accessibilityHidden(true)
                }
            }
        }
        .padding(10)
        .frame(width: 268)
        .background(Color.ground2)
        .onAppear { searchFocused = true }
    }

    private func row(title: String, value: String?, leading: () -> some View) -> some View {
        Button {
            onSelect(value)
        } label: {
            HStack(spacing: 8) {
                leading()
                Text(title)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.ink)
                    .lineLimit(1)
                    .truncationMode(.middle)
                Spacer(minLength: 0)
            }
            .modifier(SidebarRowSurface(isSelected: selection == value))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(title)
        .accessibilityAddTraits(selection == value ? .isSelected : [])
    }

    private func tile(@ViewBuilder content: () -> some View) -> some View {
        content()
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
            .frame(width: 22, height: 22)
            .background(RoundedRectangle(cornerRadius: 5).fill(Color.ground3))
            .accessibilityHidden(true)
    }
}
