# Work items — Settings, Usage, New Task, 4 Nov 2026

Three PRs, serial. Each re-records the screenshot digests it touches in both appearances and writes a devlog note.

## 1 · Usage, and the two recipes

Files: `app/Sources/FreesideCore/DesignLanguage.swift` (add `CapacityMeter`, `CapacityBlock`, `ComposerChip`, `RoleChip`), `app/Sources/FreesideCore/AccountUsageView.swift`, `app/Tests/…/DesignContrastTests*`.

Usage becomes one card per pool under a provider keyword: a ground-3 name band (pool id in the statement face, plan in mono, read time trailing in faint mono; wax `stale since <time>` when stale), then one `CapacityBlock` per window in `UsageDisplay.windows` order. The block: name with the value trailing; the reset line with the sub-cap and the parked count; the meter. Collectors becomes a link to the existing enrollment list; Refresh trails the page. The per-window mono lines, the refresh-status block and the `Stale` / `Used up` keywords go; everything they said is in the block or the band.

Done when: Usage matches band 2 in both grounds on Mac and iPhone; `DesignContrastTests` cover the accent and wax fills on ground-3; VoiceOver reads one element per window.

## 2 · The Settings window and list

Files: `app/Sources/FreesideCore/SettingsWindow.swift` (new, macOS), `SettingsList.swift` (new, iOS), `RoutingPolicyView.swift`, `DevicesView.swift`, `DaemonMenuPanel.swift`, `FreesideRootView.swift`, `NavigationModel`; `SettingsSheet.swift` retired.

The window (`⌘,`) with a source list of Agents, Usage, Devices, Notifications, Daemon and the host at its foot; the iPhone grouped list pushed from the gear. Pane grammar as the README states. Agents: popups per role, `Change order…` to the existing editor, the tree as read-only rows with the pool trailing. Usage: PR 1's view. Devices and Daemon: today's content as grouped rows. Notifications: the subscription state only.

Done when: every pane matches band 1; `-FreesideDevices`, `-FreesideRouting`, `-FreesideUsage` open their pane; the sheet is gone from both clients.

## 3 · The New Task composer

Files: `app/Sources/FreesideCore/NewTaskSheet.swift`, `TaskSubmissionRecoverySheet.swift`, `app/SURFACES.md`.

The composer: a growing text area, foot with project chip and Agents chip, Return to submit and Shift+Return for a line; the role-chip row when Agents is open; no name field. iPhone: in a sheet with Cancel and Submit in the bar (◆3 default). The recovery sheet drops its name line. SURFACES.md updated as the brief's Finish states.

Done when: New Task matches band 3 in both grounds on Mac and iPhone; the same `task_lines` are sent for the same choices (test); no name is ever sent.
