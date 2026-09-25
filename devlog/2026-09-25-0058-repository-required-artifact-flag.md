# Flag a Missing Repository-Required Artifact Through the Findings Channel

For #1542, the production reviewer flags a candidate that plausibly falls
under a target-repository rule requiring an artifact (a decision note, a doc
update) when the candidate lacks it. The flag is an ordinary review finding
admitted under a second, separately stated admission class, not a new
advisory item. The owner decided that the reviewer must at least flag the
gap; the planner chose this shape and the owner let it stand at
`Handle #1542`.

Evidence: in the #1445 exit run (run-113-after-1536), gh-imgup PR #120
changed a CLI output contract that gh-imgup's AGENTS.md puts on its
mandatory-note list, and shipped without a note. The reviewer had that
AGENTS.md in its bundle, but the defect admission test requires a
demonstrable failure path, and a missing file has none. The operator caught
it at the ready card by hand.

The class admits a finding only when three conditions hold: a repository
instruction block names this kind of change as requiring a specific artifact
in mandatory terms; the changed files plausibly fall under that kind; and the
artifact is absent. Severity is fixed at P3, the location is
`whole_file:true` on the triggering changed file (so the diff-overlap gate
accepts it), and the explanation starts with `Repository-required artifact
missing:` and quotes the rule with the Scope of its instruction block, so the
operator can judge whether it applies. The prompt asks for the Scope, not the
file path, because the composed bundle labels each block by directory scope
and digest only; a reviewer asked for the file name would have to guess
between `AGENTS.md` and `AGENTS.override.md`. The defect admission test is
unchanged word for word. The implementer and remediator prompts now say such
an artifact is part of done, not scope widening, and route an out-of-scope
path through Required Work Outside Scope.

Rejected: a separate advisory item in the review output. It would change the
shared review JSON schema, `exec.ReviewResult`, a domain type, store
persistence, engine surfacing, `api/openapi.yaml`, generated Swift, and the
ready card: a cross-component `kind:contract` unit serialized behind the open
contract chain, for a P3 the operator already catches by hand. Its advantage,
a glance instead of an adjudication, doesn't justify that cost yet.

Cost of the chosen shape: each flag is adjudicated like any finding and may
cost a remediation round; a false flag costs a round. The three conditions
and the exemption sentences (no flag for exempt, routine, or discretionary
cases, nor for process or forge state such as an open PR or green checks,
which cannot exist before publication and so could never be remediated) are
the only brake. The reviewer bundle holds only `AGENTS.md` and
`AGENTS.override.md` from the exact base, so a rule that lives only in a
repository's CLAUDE.md stays invisible to this flag.

Revisit when a real run shows repeated false flags or remediation churn from
this class, or when a second non-defect advisory class appears; then the
advisory item earns its contract unit.
