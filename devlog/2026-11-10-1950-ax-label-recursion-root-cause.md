# Accessibility Label Recursion: Root Cause and the Element-Before-Label Rule

Work unit: #2993 (reopened after PR #3012).

## Finding

The Mac client's stack overflow in accessibility label resolution is a
SwiftUI defect (observed on macOS 27.2) that the app reached at many sites,
not at one card.

- **Construct.** Under the window's selectable-text default, a `Text` is
  backed by an AppKit text view. An `.accessibilityLabel` that reaches the
  `Text` through a wrapper (a layout or style modifier, a `Group`, a
  `Label`) makes that text view the platform element of the labelled node.
- **Trigger.** The view is inserted after an assistive client has turned
  accessibility on. SwiftUI then installs a description handler on the text
  view while also exposing the label as the element's value. A read of the
  description or title asks the node, the node falls back to the text
  view's description, and the handler asks the node again.
- **Why it looked random.** A view built before the client attached does
  not recurse, and neither does an in-place update. The first screenshot
  turned accessibility on; the app died on a later read, after the page
  inserted a view of this shape.
- **The view in the reported crashes.** `StateChip`, the review round's
  verdict chip in the task's Current Run section, on the `quota-move`
  scenario after the task moved to another agent.

PR #3012 turned text selection off on two menu labels without a
reproduction. A labelled menu does not recurse in isolation, so that change
did not address the crash.

## Decisions

- **Chose to fix every site, over the issue's non-goal of listing other
  sites and filing an issue.** The owner asked on 2026-11-10 for the crash
  to be fixed completely. The construct sits in shared components (29
  `StateChip` call sites, the task name, prose blocks), so a ready-card-only
  fix would have left the reported crash in place.
- **Chose `.accessibilityElement(children: .ignore)` before the label, over
  `.textSelection(.disabled)` at each site.** The element form keeps the
  text copyable (app/README.md, Copyable Text), is already the project's
  idiom at 29 sites, and keeps the spoken label. Writing the label directly
  on the `Text` avoids the recursion, but macOS then drops the label.
- **Chose an in-process test that interposes on SwiftUI's private
  `AccessibilityNode.accessibilityLabel`, over a test that crashes or an
  XCUITest target.** The method is the one Objective-C dispatch in the
  loop, so a test can count re-entry and stop it. A crashing test gives no
  failure to read, and the project has no UI-test target. The cost is a
  dependency on a private class name; the test fails loudly if it moves.
- **Chose not to install that interposition in the shipping app.** It would
  stop the crash at sites the tests miss, but it patches a private SwiftUI
  method in production. The owner can revisit this.

## Revisit When

- The positive-control test (`theProbeCatchesALabelOnAWrappedSelectableText`)
  fails on a newer macOS: SwiftUI no longer recurses, and the rule and the
  insertion tests can go.
- A crash of this shape recurs at a site no test inserts: reconsider the
  shipping interposition.
