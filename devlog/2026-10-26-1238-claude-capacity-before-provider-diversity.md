# Claude Capacity Before Provider Diversity

Plan revision 99. The owner downgraded the Codex plan and wants Freeside
developing Freeside on Claude, across two Claude subscriptions, sooner than
the wave 9 row allowed. The row was built around a second provider: the
Codex execution driver, its enrollment cutover, and pi specification. This
note records why that row became "Claude capacity" and what the change
overturns.

## Decisions

- **Chose two Claude subscriptions over the Codex execution driver as 1B's
  capacity.** The Codex tail (#406, #407, #408, #873, #868, #1718) and pi
  (#895, #897) move to Phase 2. With the plan downgraded, the driver is a
  hedge that holds contract-chain slots through four units and delays
  Freeside's own onboarding. Codex review stays as built, because review on
  a second vendor is still the independence §7 prefers, and #408 keeps its
  place after #397 when it returns. Rejected: dropping Codex altogether.
- **Chose a router above admission over revision 77's rule that no reading
  selects an agent.** Revision 77 kept usage readings advisory because the
  owner would balance providers by hand. That hand balancing is the daily
  interruption Freeside exists to remove once two subscriptions carry all
  the work. The router reads readings and failures and writes the task
  line; admission reads only the line. Every switch is a recorded fact
  naming its policy revision, the reading or mark it acted on, and the
  agent it passed over, so nothing is silent. Rejected: routing on failures
  only, because it can't empty one subscription before its reset on
  purpose, which is the policy the owner wants; routing inside admission,
  because admission would then read a reading and the line would stop
  being the one authority.
- **Chose priority-with-fallback as the default routing mode**, with
  percentage and round robin beside it. The two subscriptions reset on
  different dates, so draining the earlier one first wastes nothing. A
  model-scoped window counts separately: a subscription with allowance left
  but none for the role's model routes to the other.
- **Chose operator settings in the app over committed-tree configuration
  for the routing policy.** The plan said clients never edit settings. Task
  lines already opened the class this policy joins: an authenticated
  operator's recorded choice among credentials the committed lineup
  approved, which approves none. The owner's reason: a tool nobody can
  configure in practice isn't usable. Other configuration still passes the
  §5.8 gate. Rejected: a PR per change of preference; an approval card per
  change, where the operator would approve their own choice.
- **Chose to move the usage record, collector, and account usage view into
  wave 9 and fold wave 10.** The agent proposed keeping the screen in wave
  10 and shipping only the facts beside the retry card, on the ground that
  the question is "why is nothing running." The owner corrected that: the
  decisions they make are about how full each subscription is, so the view
  is part of the loop. The router needs the record anyway.
- **Chose to run the Claude quantitative usage spike first.** #1713 proved
  only a window status and reset time under the setup token, no used
  share. The view's shape follows the spike's result: shares per window, or
  status and reset only, or unsupported per window. Rejected: planning the
  view on a share nobody has proved.
- **Chose Freeside's own onboarding, `daemon/` first, as wave 9's exit
  proof.** The baked recipe verifies only `daemon/` networkless; `api/`,
  `scripts/`, and `docs/` follow as their recipes do. The owner refused any
  proof that drains a real subscription. The real run proves the ordinary
  path and a seeded exhausted window or injected refusal proves the
  fallback, the pattern wave 8's fixture passes already use.

## Changed Assumptions

- Revision 77 assumed the operator would do usage-driven switching by hand
  and counted it as attention work until routing absorbed it (§8). Routing
  absorbs the usage part now; task class, quality, latency, and cost routing
  stay Phase 3.
- The §14 single-provider risk assumed the Codex driver as the hedge. Two
  subscriptions hedge a quota window, not a Claude outage or policy change.
  The owner accepts that residual while the Codex plan is downgraded.

## Revisit When

- The Codex plan is restored, or Claude review on real items argues for a
  second execution vendor again: the Phase 2 bullets name the units.
- The usage spike proves a used share: the view and the exhausted mark can
  read it instead of waiting for a refusal.
- Routing wants a second input (cost, latency, quality): that is the Phase 3
  routing this revision left there.
- A third subscription or a team pool joins: percentage and round robin
  were designed for it but the exit proof covers two pools only.
