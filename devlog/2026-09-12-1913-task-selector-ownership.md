# Authenticate Task Selector Ownership

Chose to authenticate the selected campaign's specification root in the CLI
before passing its campaign ID to the retry engine. The task's persisted
`CampaignIDs` list is decoded data, not proof of ownership. A valid task row
can name another task's valid campaign, and the engine would otherwise
authenticate that campaign independently and allocate a retry for the wrong
task.

The initial production attempt identifies the campaign's immutable
specification run. Reading that run through the existing store gate and
comparing its task and project to the requested task establishes ownership.
The retry engine still resolves the latest attempt inside its write
transaction and preserves its live-parent refusal and interrupted-allocation
recovery. Rejected adding task selection to the shared engine/store API
because campaign lineage already keeps this ownership fixed.

The corruption regression confirmed that the previous CLI allocated an
attempt for another task after accepting a mismatched campaign history. With
the ownership check, that history is refused and the latest attempt number
stays unchanged. The companion resume selector uses `task_runs`; a corruption
probe confirmed that substituting another task's run fails at the existing
run reconstruction gate before any observation output. No extra resume
guard is needed.

Revisit when campaign or run ownership becomes mutable, or task selection
must provide an atomic view across concurrent campaign creation.
