import SwiftUI

// The launch inputs that name SwiftUI values, read from the raw arguments
// LaunchInputs keeps so the struct itself builds without SwiftUI.
extension LaunchInputs {
    /// `-FreesideColorScheme light|dark`; unset or unrecognized
    /// follows the system.
    public var colorScheme: ColorScheme? {
        switch colorSchemeRaw {
        case "light": .light
        case "dark": .dark
        default: nil
        }
    }

    /// `-FreesideDynamicType <size>`; unset or unrecognized follows the
    /// system. The accepted names mirror the documented screenshot inputs.
    public var dynamicTypeSize: DynamicTypeSize? {
        switch dynamicTypeSizeRaw {
        case "x-small": .xSmall
        case "small": .small
        case "medium": .medium
        case "large": .large
        case "x-large": .xLarge
        case "xx-large": .xxLarge
        case "xxx-large": .xxxLarge
        case "ax1": .accessibility1
        case "ax2": .accessibility2
        case "ax3": .accessibility3
        case "ax4": .accessibility4
        case "ax5": .accessibility5
        default: nil
        }
    }
}
