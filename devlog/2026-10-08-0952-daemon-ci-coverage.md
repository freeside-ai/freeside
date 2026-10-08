# Ship Scenario Parallelism Before CI Sharding

The owner chose the isolated scenario scheduling change before full CI
sharding because the scheduling change has a smaller verification surface
and the measured whole-gate sharding benefit remains unproved. A passing
matrix and complete coverage do not establish a faster end-to-end gate;
macOS allocation delays dominated the first hosted comparison. Shipping the
larger layout on individual shard gains would overstate the evidence.

Each changed case owns its temporary store, repositories, fake forge, and
mutable harness state. Parent and child parallelism let those independent
cases use Go's existing test slots. Read-only transition tables and the
once-initialized store template are shared. Environment-mutating tests remain
serial, and assertions, default concurrency, and timeouts are preserved.
The affected families passed focused race verification.

Kept production git-read optimization, shared-fixture rewrites, and broader
package parallelization separate. They change different boundaries and
would make the scheduling result harder to assess. The current delivery
makes no claim of a 50% whole-CI reduction.

Follow-up: [#1877](https://github.com/freeside-ai/freeside/issues/1877).

Revisit when controlled whole-gate measurements can distinguish scheduling
improvement from runner queues and repeated cache/setup costs.
