# Count Slot Waiters on the Pool, Not Parked Runs on a Window

Issue #3677. Client only, Tier B.

## Decision

The Usage pane counts the runs waiting for a slot on each pool in a row of
its own under the pool's windows, `N runs waiting for a slot ›`, whenever
the count is above zero. The row opens the list narrowed to those runs'
tasks. It replaces the ` · N runs parked` clause on a used-up window's reset
line, which reverses the parked count on a used-up window recorded in
`devlog/2026-11-04-1251-settings-usage-gate-answers.md` (Values Sync Lacks)
and drawn by #2583.

## Why the Clause Was Wrong

The count reads `hold_usage_pool` on the synced runs (#2578), which the API
sets only beside a live `usage_pool_exhausted` hold: the pool is at its
limit of parallel runs. Those runs wait for a slot, not for a window to
reset, so a clause on the reset line said the wrong thing, and it showed
only when a window happened to be used up. A run that does wait for a window
holds `quota_window_exhausted`, whose pool sync doesn't carry; counting it
would need an API change and stays out of scope.

## Choices and What They Rejected

- **One narrowing slot for the list,** over a second filter beside the work
  status count. Choosing the pool replaces a count and the reverse, with
  the count's kept-row and reveal rules, so the list has one filter line
  and one Clear, and a route that a narrowing hides drops it the same way.
- **The narrowing names the pool, not a task set,** over capturing the task
  ids when the row is chosen. The list rebuilds from synced runs, so a task
  leaves once its last hold clears (unless it is the selected row) and the
  filter never shows a task that no longer waits.
- **The slot row drops the project filter,** over keeping it. A pool
  serves every project and the row counts them all, so under a project
  filter the list could show fewer tasks than the row named, or none. A
  work status count keeps the filter because it is counted under it.
- **`unsupported` draws faint, not wax.** Nothing failed: the provider
  reports no usage for the plan. Wax stays for readings that can't be
  trusted.

## Revisit When

Sync carries the pool of a `quota_window_exhausted` hold, so the pane could
also count runs waiting for a window.
