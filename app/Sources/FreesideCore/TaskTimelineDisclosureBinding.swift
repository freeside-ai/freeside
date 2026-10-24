import SwiftUI

extension TaskTimelineDisclosurePreferences {
    func binding(_ section: Section, taskID: String) -> Binding<Bool> {
        Binding(
            get: { self.isExpanded(section, taskID: taskID) },
            set: { self.setExpanded($0, section, taskID: taskID) })
    }
}
