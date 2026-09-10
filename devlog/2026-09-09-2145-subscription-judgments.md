# Reuse The Claude Subscription For Daemon Judgments

The owner required the missing production classifier/adjudicator backend to
use existing mechanisms. We chose the native Claude CLI and the same private
setup-token snapshot reader used for Claude shadow review. A separate API key
account or a direct HTTP implementation of subscription authentication would
introduce a different credential mechanism and does not meet that assignment.

The adapter is outside `internal/inference`, injected through its existing
structured Driver interface. It does not reuse StageDriver or ReviewSource:
those grant repository access and run Ward workloads, which the judgment
contract explicitly excludes. The native CLI receives a fresh empty home and
configuration directory, safe mode, an empty tool set, no MCP configuration,
no saved session, and only allowlisted task fields through stdin. The provider
binding remains deployment-pinned; this does not decide #900's lineup question.

The executable content is pinned and copied into private per-call scratch
before execution. This prevents a concurrent CLI updater from changing the
binary between verification and execution. The snapshot digest and model feed
the secret-free composition digest. Retained restoration refuses a change to
that binding, including enabling judgments on a previously unbound session.
Startup requires the digest from that successful preflight manifest and compares
it with the binding it reads. This closes the token-rotation gap between
preflight and launch; accepted credentials then remain in memory for that run.

Compute units measure generated tokens. Input size has the site's separate
byte bound; the existing ledger reserves the full output allowance before
dispatch. The CLI gets that output limit, disabled thinking, one turn, and the
site deadline. Missing or contradictory terminal/usage evidence fails closed.
Only classifier and adjudicator sites use this adapter; other sites retain
their existing fail-safe outputs.

Independent refutation found that per-site single-flight did not constrain
different judgment sites sharing this adapter. A driver-wide single-call gate
now refuses overlapping calls without queuing. The binding still does not
claim admitted-agent account or usage-pool attribution; that identity question
remains #900. The adapter does not refresh or write the subscription auth store.
The same review found that absent tool-usage fields were interpreted as zero.
The decoder now requires their presence, so incomplete usage evidence cannot
become an accepted proposal.
The daemon's subprocess enumeration also caught a locally implemented lifetime
bound. The adapter now uses `procbound.Run`, including the shared descendant
reaping behavior, instead of maintaining a second cancellation mechanism.

A localhost synthetic-provider probe of Claude CLI 2.1.267 observed one model
request with an empty tool list, disabled thinking, the requested output limit,
and no repository context. This establishes a protocol property, not a real
provider judgment or the controlled live challenge's acceptance.

Revisit when the CLI pin changes, the subscription authentication mechanism
changes, or #900 resolves identity and lineup treatment for utility roles.
