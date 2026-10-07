# Prompt History Is Draft Input

For #1823, chose a separate local prompt-history file over extending the
pending-submission cache. History is convenience data: a failed write must
leave a confirmed task successful. The recovery ledger still owns exact
commands and retry identity. Only the existing submission-result trust gate
can add history, using the sent source rather than mutable form fields.

The record validates its version, deployment and device ownership, entry
count, and nonempty fields when loaded. Missing or invalid history starts
empty. Deployment directories and device-derived filenames keep connection
changes separate. Mock and ephemeral sessions use memory. Decoded entries
can only populate the source draft; explicit Submit still creates a new
command. Names, task lines, commands, and credentials are absent from history.
Confirmed task IDs deduplicate replays, including adjacent identical prompts
coalesced into one entry. They never select a recovery command or a task to
execute. The 50-entry bound applies across projects for one deployment/device.

Chose an editor-local AppKit responder over a window-wide keyboard monitor.
Only unmodified arrows with a collapsed selection and no marked text can
browse. The first Up also requires the first visual line. Browsing snapshots
the current project's entries and original draft; Down past the newest and a
project change restore that draft exactly. A native text edit ends browsing.

Independent refutation confirmed that assigning `NSTextView.string` while
retaining typing undo ranges could crash on Undo after recalling shorter
text. History replacement now uses native insertion and undo coalescing
boundaries. External draft replacement clears obsolete undo records. The
regression exercises the native editor and Undo manager.

Revisit when history needs device sync, search, or an operator-facing
retention policy. Those needs would require a separate data-lifecycle design;
they do not justify coupling this record to recovery or server authority.
