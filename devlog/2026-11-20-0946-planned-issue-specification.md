# Specify a Planned Issue From Its Contract and Plan

A planning session's current plan comment now starts with
`<!-- freeside-implementation-plan:v1 -->`, and `docs/coordination.md`
(Stages) defines it. When a specifier fetches its task's bound issue's first
page of comments and exactly one comment on it carries the marker from an
owner, member, or collaborator, the daemon tags that research block with the
comment's `plan_comment_id`. The specifier then treats the issue body as the
contract and the comment as the plan, and returns a specification without
asking unless they conflict with each other, the code, or policy (#3405).

## The Marker Is the One Planning Sessions Already Write

Chose the existing first line over a new heading or a body pattern because
planning sessions already write it (#3441, #3442, #3443), so every plan
written since is recognized without a rewrite. Marking a plan non-current
removes the line, the same way the daemon releases its claim by removing its
claim marker. Plans written before the marker are not recognized; those
issues specify as before.

## The Daemon Decides, Not the Model

Chose recognition in the daemon at render time over a prompt rule that asks
the model to find the plan. The model can't check who wrote a comment, and
it could pick a stale plan when two look current. The daemon checks the
`author_association` (the owner, member, and collaborator set the claim
protocol uses), that the marker is the first line, and that exactly one
comment on the page qualifies, then states the result as a field it
controls. The comment itself stays untrusted research text, screened as
before; the field only says which comment the specifier may treat as the
plan, and the specifier still checks that plan against the code.

The check accepts only the bound issue's first comments page, by its final
URL, with exactly `per_page=100` as its query. A smaller page, a later page,
or a filter such as `since` could hide a second marked comment from the
exactly-one check. A plan past the first 100 comments is not found, which
is a stated non-goal.

## The Daemon Fetches Nothing Itself

Chose having the prompt ask for the comments over the daemon fetching them.
A daemon fetch would add egress on every issue-backed task and change an
unplanned task's inputs. With the specifier asking, an unplanned issue's
inputs change only by one research block it requested, and every block
without a plan renders byte for byte as before.

## The Specifier Prompt's Budget Rises to 5 KiB

The owner chose raising `TestPhase1ASpecifierPromptLeavesEnvelopeHeadroom`'s
budget from 4 KiB to 5 KiB over fitting the new rule in 4 KiB. The prompt
was already at 4,096 bytes, and the tightest wording that kept every rule
was still 338 bytes over, so fitting would have dropped existing guidance,
most likely the per-field size limits. The budget recorded no reason beyond
"envelope headroom", and about 700 bytes more prompt is small beside the
31 KiB rendered input.

## The Five #2968 Questions Were Not This Gap

On #2382's real root, five specifications of #2968 each stopped on the same
question. #2968 has no comments, and its contract left one choice to the
owner, so asking was correct. The questions repeated because each was a new
task, and the owner's answer lived only in the previous task's conversation
(findings on #3405). A plan settles such a choice in the contract or plan,
so the fast path removes the repeat for a planned issue; an unplanned issue
with an open choice still asks.

Revisit when: planning sessions record plans somewhere other than an issue
comment, or a project's plan comments routinely pass 100 comments per issue.
