# Freeside

**Freeside is a local, durable workflow controller that turns a software work item into an evidence-backed pull request and interrupts me only when judgment is required.**

Category: an agent control plane. Harnesses (Claude Code, Codex) run the agent's inner loop; Freeside is the outer loop: what work starts, inside what boundary, with which credentials withheld, what counts as done, when a human is interrupted, and what survives a crash. The harness runs the agent; the reins are yours.

**Status:** Phase 1A (the secure publish path) underway. The daemon is initialized (Wave 0 unit 1: module, dual-platform CI, test conventions); this monorepo's other component directories stay intentionally empty until their phase begins.

- **Everything** — charter, architecture, roadmap, decisions: [`docs/plan.md`](docs/plan.md).
- **Conventions** — how to work in this repo: [`AGENTS.md`](AGENTS.md).

## License

This work is licensed under [AGPL-3.0-or-later](./LICENSE).

The copyright holder applies this grant to every revision in this repository's
history, except material that states a different license or copyright holder.

See [LICENSING-PHILOSOPHY.md](./LICENSING-PHILOSOPHY.md) for why we chose
this license.

The philosophy describes how we choose a license for a project; it does not
grant separate licenses to individual files in this repository.

---

A [Free as in Bird](https://freeasinbird.com) project.
