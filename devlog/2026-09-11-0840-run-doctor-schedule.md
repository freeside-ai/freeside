# Run Doctor Through The Existing Schedule

Chose an atomic advance of the existing doctor job's next due time over a
second request queue or an HTTP-handler diagnostic run. The accepted command
and due timer commit together. The scheduler already owns diagnostic execution,
pending-occurrence redelivery, operating-mode eligibility and failure reporting.
Startup preserves the clock for an unchanged schedule, so a restart cannot lose
the request. Replaying the command does not advance the timer again.

Several requests before a due pass coalesce into that pass. Accepting a request
does not claim diagnostics succeeded, resolve the health item or change the
diagnostic suite. A missing or inactive configured job refuses the command
instead of recording a successful action with no consumer. Composition also
tracks whether the doctor scheduler is running: an armed row left by another
mode, or a stopped scheduler with HTTP still serving, cannot accept a request.
These known no-effect rejections use existing HTTP 4xx responses so the
client releases its pending command instead of treating a rolled-back request
as an uncertain commit. Unexpected storage errors retain their existing mapping.

Revisit when operator diagnostics need a different suite or a per-command
completion artifact. The current action requests the deployment's existing
doctor pass and does not need a new schedule kind or persistence schema.
