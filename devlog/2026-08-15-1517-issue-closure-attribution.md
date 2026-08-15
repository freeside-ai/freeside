# Resolve Issue Closure Attribution Through the Actual Closer

Work unit #810 changes a returned-object trust boundary that feeds the
Section 5.18 completion gate, so this note records the attribution surface and
the refute-first result required for that boundary.

## Use the Closed Event's Closer as a Fallback

Chose GraphQL `ClosedEvent.closer` over a pull request's
`closingIssuesReferences`, because the closer identifies what actually
performed the closure. A closing-keyword reference survives when a person
manually closes the issue before merge and would therefore manufacture a
false attribution; the closed event remains null in that case.

Chose a fallback-only GraphQL read over replacing the proven REST event walk.
Commit-message closures retain their existing direct commit attribution and
request path. GraphQL runs only for the previously unsatisfiable PR-body and
manual-close cases, maps a merged pull request to its merge commit, and leaves
the durable fact shape and exact merge-SHA criterion unchanged.

Rejected adding a closing-pull field to the fact. The completion criterion
needs only the attributed merge SHA, while another field would widen store
goldens and the reconstruction trust gate without adding evaluative power.

## Returned-Object Refute-First Result

The first adversarial pass disproved agreement-by-recency between the REST
walk and GraphQL `last: 1`: a stale or spoofed GraphQL response could name an
older attributed close after the latest REST close was manual, and that SHA
could then satisfy and enter the completion gate. The resolver now binds the
REST event's global `node_id` to GraphQL `ClosedEvent.id` before trusting its
closer.

The boundary suite rejects transport and decode failures, GraphQL errors,
absent or mismatched issue and closed-event identity, missing or multiple
closed events, unexpected event types, an omitted closer field, unknown closer
types, incomplete commit closers, and unmerged or incomplete pull-request
closers. An explicit null closer is the sole unattributed closed result and is
deliberately not ETag-cached, so the next poll re-runs both attribution
surfaces instead of pinning an empty value. An attributed closure remains
cacheable and preserves the existing 304 no-churn behavior.

The live regression against `freeasinbird/gh-imgup#76` confirmed the two APIs
name the same closed event (`CE_lADOTDP-Us8AAAABJz79U88AAAAG3okC8A`): REST
still reports a null commit, while GraphQL identifies merged PR #96 and merge
commit `a74d41f516cd947db9014b845112d3c1f29b0e58`.

## Revisit When

Revisit the fallback-only split if another GraphQL issue-observation consumer
lands or GitHub gives REST issue events durable attribution for PR-body
closures.
