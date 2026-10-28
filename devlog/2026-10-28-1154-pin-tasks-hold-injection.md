# Pin Each Task to One Subscription and Hold Credential Injection

Plan revision 103. Mandatory note: a material plan change (§4, §5.4, §5.7,
§11, §14) and an owner decision that would otherwise live only in a session.
The owner made both decisions on 2026-10-28; the contract is #2416.

## Decisions

1. **Chose to pick a Claude subscription only when a task starts, and keep
   every role of that task on it, over revision 99's mid-task failover.**
   At the first admission of a task's first task-line role (specifier,
   implementer, remediator, or reviewer) with no operator line, the router
   picks one usage pool. It writes a router-sourced line for every task-line
   role without an operator line, naming the agent the lineup carries for
   that role on that pool (the first in the policy order if it carries
   several). A role with no agent on that pool keeps its lineup agent, and an
   operator-set line always wins.
   - A quota failure parks the task under a visible hold naming the window's
     reset. No card is raised, and the role's next attempt is admitted on the
     same line at the reset.
   - Only the operator moves a parked task, as a recorded handoff: with the
     alternate-agent card, or with a task-line change. The owner named the
     card as the only way; the plan already lets the operator change a task
     line while no attempt of the role runs, and that is just as explicit,
     so the revision keeps both. The card's move re-pins every
     router-sourced line to the chosen agent's pool; otherwise it would cover
     one attempt and the next role would park again on the exhausted pool.
   - When neither the refusal nor a reading gives a reset time, nothing can
     resume the task, so the ordinary quota card (retry under an alternate
     agent, wait, or stop) is raised.
   - When every pool is exhausted at a task's first admission, the router
     picks none and the role parks until the earliest reset.
   - The choice reads the first role's model window. A later role whose model
     window on the pinned pool is exhausted parks until it resets; it never
     reroutes.
   - A lineup's per-failure-class switch no longer applies to a quota
     failure, because it would move a task in flight automatically. It still
     applies to expiry and capacity failures.
   - Priority with fallback (drain-first) stays, for new tasks only. The
     percentage split and round robin are removed.
2. **Chose to hold `subscription_injected` and `api_key_isolated` entirely
   over keeping them in Phase 2.** Nothing schedules them. The design text
   stays in the plan, marked as held, and #2175, #2173, and #2174 stay open
   and deferred. Hosts the owner doesn't physically hold, and host-run
   writers on either Mac, keep waiting; they don't fall back to copying the
   token. `api_key_isolated` goes with it because it reuses the same proxy
   path.

## Why

- **Stay close to Anthropic's terms for consumer plans.** The passages the
  decision rests on, checked against the live pages on 2026-10-28:
  - Consumer Terms of Service (effective 2025-10-08), section 3: users may
    not engage in "bypassing any of our systems or protective measures".
  - Claude Code's legal and compliance page
    (`code.claude.com/docs/en/legal-and-compliance`): "Advertised usage
    limits for Pro and Max plans assume ordinary, individual usage of Claude
    Code and the Agent SDK."
  - Usage Policy (effective 2026-11-12): users may not "Resell, proxy, or
    otherwise provide access to Claude through unauthorized means, including
    services that route requests through consumer subscriptions or
    misrepresent the product or client being used".
- **Mid-task failover is the riskiest reading.** Moving a task to a second
  subscription the moment the first runs out, so the work keeps going past
  one plan's limit, looks most like getting around a limit. Choosing per new
  task is closer to a person deciding which account to open for the next job.
- **Per-role choice would undo the pin.** If each role chose its own pool,
  review could run on subscription B right after implementation used up A.
- **Percentage and round robin spread load to stretch capacity.** They exist
  to draw on both plans at once, which is the reading the owner wants to
  avoid.
- **Injection isn't on the near-term map and adds little yet.** It would add
  work and complexity now. A secondary reason: with a subscription token,
  Anthropic would see the daemon's TLS and HTTP client sending Claude Code's
  requests, which is close to the Usage Policy's "misrepresent the product
  or client being used".

## Rejected

- **Automatic failover to another subscription mid-task** (revision 99): see
  above.
- **A separate account or session manager outside Freeside.** The owner
  considered one. It would make the same selection and leave the terms
  question where it is.
- **Injection with a subscription token**, in Phase 2 or pulled forward by
  fiat for the first host the owner doesn't hold (revision 95).
- **Keeping the percentage split and round robin** as non-default modes.

## Open

Whether Anthropic permits drain-first selection across one owner's
subscriptions is not settled. The plan keeps it as an operating risk in §14
and recommends asking Anthropic.

## Revisit When

- Anthropic answers the drain-first question, either way.
- A host the owner doesn't physically hold becomes near-term work; that
  needs injection, so it reopens decision 2.
- Anthropic's Consumer Terms, Usage Policy, or Claude Code legal page change
  on subscription routing, proxies, or client identity.
- A usage spike shows Claude quota refusals rarely carry a reset time and no
  reading supplies one. Then most quota failures raise the card, and the
  "park with no card" path is mostly theoretical; the plan text stays
  correct, but the owner may want a different default.
